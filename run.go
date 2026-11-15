package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// A run is reported as failed when more than this share of its rows is rejected.
const maxRejectRatio = 0.5

type Summary struct {
	RunID      int    `json:"run_id"`
	PipelineID int    `json:"pipeline_id"`
	Status     string `json:"status"`
	Trigger    string `json:"trigger"`
	Read       int    `json:"read"`
	Written    int    `json:"written"`
	Rejected   int    `json:"rejected"`
}

type Report struct {
	Summary
	StartedAt  time.Time   `json:"started_at"`
	Error      string      `json:"error,omitempty"`
	Rejections []Rejection `json:"rejections"`
}

// runLocked runs a pipeline and stores its report. The caller holds s.mu for
// the whole run, so runs never interleave; with in-memory data that is cheap,
// and per-destination locks are the upgrade if runs ever become slow.
func (s *Store) runLocked(p *Pipeline, trigger string) *Report {
	rep := &Report{
		Summary:    Summary{RunID: len(s.runs) + 1, PipelineID: p.ID, Status: "failed", Trigger: trigger},
		StartedAt:  time.Now().UTC(),
		Rejections: []Rejection{},
	}
	out, err := s.transform(p, rep)
	rep.Rejected = len(rep.Rejections)
	// The destination is touched only here, after every row has been processed.
	if err == nil {
		err = s.commit(p.Destination, out)
	}
	if err != nil {
		rep.Error = err.Error()
	} else {
		rep.Status, rep.Written = "completed", len(out)
	}
	s.runs = append(s.runs, rep)
	slog.Info("run", "pipeline", p.Name, "run_id", rep.RunID, "trigger", trigger, "status", rep.Status,
		"read", rep.Read, "written", rep.Written, "rejected", rep.Rejected, "error", rep.Error)
	return rep
}

func (s *Store) transform(p *Pipeline, rep *Report) ([]map[string]any, error) {
	rows, rejected, read, err := s.readSource(p.Source)
	rep.Read = read
	rep.Rejections = append(rep.Rejections, rejected...)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	for i, step := range p.steps {
		reject := func(r Row, err error) {
			rep.Rejections = append(rep.Rejections, Rejection{r.N, i + 1, p.ops[i], err.Error(), r.F})
		}
		if rows, err = step.Run(rows, reject); err != nil {
			return nil, fmt.Errorf("step %d (%s): %w", i+1, p.ops[i], err)
		}
	}
	if n := len(rep.Rejections); read > 0 && float64(n)/float64(read) > maxRejectRatio {
		return nil, fmt.Errorf("rejected %d of %d rows, above the %v limit", n, read, maxRejectRatio)
	}
	out := make([]map[string]any, len(rows))
	for i, r := range rows {
		out[i] = r.F
	}
	return out, nil
}

func (s *Store) filePath(name string) string {
	return filepath.Join(s.dataDir, name+".json")
}

// commit replaces the destination's contents in one step: a slice assignment
// for memory, a rename over the old file for files. A reader sees the old
// rows or the new rows, never a mix.
func (s *Store) commit(d Destination, rows []map[string]any) error {
	if d.Type == "memory" {
		s.tables[d.Name] = rows
		return nil
	}
	b, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dataDir, d.Name+"-*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), s.filePath(d.Name))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

func (s *Store) data(name string) ([]map[string]any, bool) {
	if rows, ok := s.tables[name]; ok {
		return rows, true
	}
	if !nameRE.MatchString(name) {
		return nil, false
	}
	b, err := os.ReadFile(s.filePath(name))
	if err != nil {
		return nil, false
	}
	var rows []map[string]any
	return rows, json.Unmarshal(b, &rows) == nil
}

// lookup resolves the {id} path value against a slice where the item with
// ID n sits at index n-1, writing the 400 or 404 itself.
func lookup[T any](w http.ResponseWriter, r *http.Request, items []T, kind string) (item T, ok bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, kind+" id must be an integer")
		return item, false
	}
	if id < 1 || id > len(items) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s %d does not exist", kind, id))
		return item, false
	}
	return items[id-1], true
}

func (s *Store) runPipeline(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := lookup(w, r, s.pipelines, "pipeline"); ok {
		writeJSON(w, http.StatusOK, s.runLocked(p, "manual").Summary)
	}
}

func (s *Store) listRuns(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := make([]Summary, len(s.runs))
	for i, rep := range s.runs {
		list[i] = rep.Summary
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Store) getRun(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rep, ok := lookup(w, r, s.runs, "run"); ok {
		writeJSON(w, http.StatusOK, rep)
	}
}

func (s *Store) getData(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := r.PathValue("destination")
	rows, ok := s.data(name)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("destination %q does not exist", name))
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// schedule runs p on every tick until the store is reset. The caller holds
// s.mu. The context is checked again under the lock so that a tick racing
// with /reset cannot add a run to the fresh store.
func (s *Store) schedule(p *Pipeline, every time.Duration) {
	ctx := s.ctx
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.mu.Lock()
				if ctx.Err() == nil {
					s.runLocked(p, "schedule")
				}
				s.mu.Unlock()
			}
		}
	}()
}
