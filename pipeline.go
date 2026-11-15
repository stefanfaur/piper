package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type Source struct {
	Type string           `json:"type"`
	Rows []map[string]any `json:"rows,omitempty"`
	Data string           `json:"data,omitempty"`
	Name string           `json:"name,omitempty"`
}

type Destination struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// UnmarshalJSON accepts both "sales" and {"type":"file","name":"sales"}.
func (d *Destination) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		d.Type = "memory"
		return json.Unmarshal(b, &d.Name)
	}
	type plain Destination // drops the method, so this does not recurse
	if err := json.Unmarshal(b, (*plain)(d)); err != nil {
		return err
	}
	if d.Type == "" {
		d.Type = "memory"
	}
	return nil
}

type Pipeline struct {
	ID          int               `json:"id"`
	Name        string            `json:"name"`
	Source      Source            `json:"source"`
	Steps       []json.RawMessage `json:"steps"`
	Destination Destination       `json:"destination"`
	Schedule    string            `json:"schedule,omitempty"`

	steps []Step
	ops   []string
	every time.Duration
}

type Rejection struct {
	Row    int            `json:"row"`
	Step   int            `json:"step"`
	Op     string         `json:"op"`
	Reason string         `json:"reason"`
	Data   map[string]any `json:"data"`
}

type apiError struct {
	status int
	msg    string
}

// Names become file paths for file destinations, so they are kept strict.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func parsePipeline(body []byte) (*Pipeline, *apiError) {
	var p Pipeline
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, &apiError{http.StatusBadRequest, "invalid JSON: " + err.Error()}
	}
	p.ID = 0
	if p.Steps == nil {
		p.Steps = []json.RawMessage{}
	}

	bad := func(msg string) (*Pipeline, *apiError) { return nil, &apiError{http.StatusBadRequest, msg} }
	switch {
	case p.Name == "":
		return bad("name is required")
	case p.Source.Type == "":
		return bad("source is required")
	case !nameRE.MatchString(p.Destination.Name):
		return bad("destination must be a name made of letters, digits, _ and -")
	case p.Destination.Type != "memory" && p.Destination.Type != "file":
		return bad(fmt.Sprintf("unknown destination type %q", p.Destination.Type))
	}
	if p.Schedule != "" {
		every, err := time.ParseDuration(p.Schedule)
		if err != nil || every < time.Second {
			return bad(`schedule must be a duration of at least one second, such as "30s"`)
		}
		p.every = every
	}

	var err error
	switch p.Source.Type {
	case "inline", "csv":
	case "table":
		if !nameRE.MatchString(p.Source.Name) {
			err = errors.New("table source needs a valid name")
		}
	default:
		err = fmt.Errorf("unknown source type %q", p.Source.Type)
	}
	if err == nil {
		p.steps, p.ops, err = buildSteps(p.Steps)
	}
	if err != nil {
		return nil, &apiError{http.StatusUnprocessableEntity, err.Error()}
	}
	return &p, nil
}

// readSource returns the rows of a source, numbered from 1. Each row gets its
// own map, so steps can change it without touching stored data. The caller
// holds s.mu.
func (s *Store) readSource(src Source) (rows []Row, rejected []Rejection, read int, err error) {
	switch src.Type {
	case "inline":
		rows = cloneRows(src.Rows)
		return rows, nil, len(rows), nil
	case "table":
		table, ok := s.tables[src.Name]
		if !ok {
			return nil, nil, 0, fmt.Errorf("source table %q does not exist", src.Name)
		}
		rows = cloneRows(table)
		return rows, nil, len(rows), nil
	}

	r := csv.NewReader(strings.NewReader(src.Data))
	header, err := r.Read()
	if err == io.EOF {
		return nil, nil, 0, nil
	}
	if err != nil {
		return nil, nil, 0, err
	}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return rows, rejected, read, nil
		}
		read++
		switch {
		case errors.Is(err, csv.ErrFieldCount):
			rejected = append(rejected, Rejection{
				Row: read, Op: "source", Reason: err.Error(), Data: map[string]any{"fields": rec},
			})
		case err != nil:
			return rows, rejected, read, err
		default:
			f := make(map[string]any, len(header))
			for i, name := range header {
				f[name] = rec[i]
			}
			rows = append(rows, Row{read, f})
		}
	}
}

func cloneRows(src []map[string]any) []Row {
	rows := make([]Row, len(src))
	for i, m := range src {
		f := maps.Clone(m)
		if f == nil {
			f = map[string]any{}
		}
		rows[i] = Row{i + 1, f}
	}
	return rows
}

func (s *Store) createPipeline(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body: "+err.Error())
		return
	}
	p, apiErr := parsePipeline(body)
	if apiErr != nil {
		writeError(w, apiErr.status, apiErr.msg)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, other := range s.pipelines {
		if other.Name == p.Name {
			writeError(w, http.StatusConflict, fmt.Sprintf("pipeline %q already exists", p.Name))
			return
		}
	}
	p.ID = len(s.pipelines) + 1
	s.pipelines = append(s.pipelines, p)
	if p.every > 0 {
		s.schedule(p, p.every)
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Store) listPipelines(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.pipelines)
}
