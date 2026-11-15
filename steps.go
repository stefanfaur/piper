package main

import (
	"encoding/json"
	"errors"
	"fmt"
)

type Row struct {
	N int // 1-based position in the source; 0 for rows made by aggregate
	F map[string]any
}

// A Step turns one batch of rows into the next. It reports rows it cannot
// process through reject and returns an error only to abort the whole run.
type Step interface {
	Run(rows []Row, reject func(Row, error)) ([]Row, error)
}

// rowStep adapts a per-row function to Step, the way http.HandlerFunc adapts
// a function to http.Handler. keep=false drops the row; an error rejects it.
type rowStep func(Row) (out Row, keep bool, err error)

func (f rowStep) Run(rows []Row, reject func(Row, error)) ([]Row, error) {
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		nr, keep, err := f(r)
		switch {
		case err != nil:
			reject(r, err)
		case keep:
			out = append(out, nr)
		}
	}
	return out, nil
}

// To add a step: write a builder and register it here.
var ops = map[string]func(json.RawMessage) (Step, error){
	"rename":    newRename,
	"select":    newSelect,
	"compute":   newCompute,
	"filter":    newFilter,
	"validate":  newValidate,
	"aggregate": newAggregate,
}

func buildSteps(raw []json.RawMessage) (steps []Step, names []string, err error) {
	for i, r := range raw {
		head, err := params[struct{ Op string }](r)
		if err != nil {
			return nil, nil, fmt.Errorf("step %d: %v", i+1, err)
		}
		build, ok := ops[head.Op]
		if !ok {
			return nil, nil, fmt.Errorf("step %d: unknown op %q", i+1, head.Op)
		}
		st, err := build(r)
		if err != nil {
			return nil, nil, fmt.Errorf("step %d: %v", i+1, err)
		}
		steps, names = append(steps, st), append(names, head.Op)
	}
	return steps, names, nil
}

func params[T any](raw json.RawMessage) (p T, err error) {
	err = json.Unmarshal(raw, &p)
	return p, err
}

func newRename(raw json.RawMessage) (Step, error) {
	p, err := params[struct{ From, To string }](raw)
	if err != nil {
		return nil, err
	}
	if p.From == "" || p.To == "" {
		return nil, errors.New(`rename needs "from" and "to"`)
	}
	return rowStep(func(r Row) (Row, bool, error) {
		v, ok := r.F[p.From]
		if !ok {
			return r, false, fmt.Errorf("missing field %q", p.From)
		}
		delete(r.F, p.From)
		r.F[p.To] = v
		return r, true, nil
	}), nil
}

func newSelect(raw json.RawMessage) (Step, error) {
	p, err := params[struct{ Fields []string }](raw)
	if err != nil {
		return nil, err
	}
	if len(p.Fields) == 0 {
		return nil, errors.New(`select needs "fields"`)
	}
	return rowStep(func(r Row) (Row, bool, error) {
		kept := make(map[string]any, len(p.Fields))
		for _, k := range p.Fields {
			v, ok := r.F[k]
			if !ok {
				return r, false, fmt.Errorf("missing field %q", k)
			}
			kept[k] = v
		}
		return Row{r.N, kept}, true, nil
	}), nil
}

func newCompute(raw json.RawMessage) (Step, error) {
	p, err := params[struct{ Field, Expr string }](raw)
	if err != nil {
		return nil, err
	}
	if p.Field == "" {
		return nil, errors.New(`compute needs "field"`)
	}
	e, err := compile(p.Expr)
	if err != nil {
		return nil, err
	}
	return rowStep(func(r Row) (Row, bool, error) {
		v, err := eval(e, r.F)
		if err != nil {
			return r, false, err
		}
		r.F[p.Field] = v
		return r, true, nil
	}), nil
}

func newFilter(raw json.RawMessage) (Step, error) {
	p, err := params[struct{ Where string }](raw)
	if err != nil {
		return nil, err
	}
	e, err := compile(p.Where)
	if err != nil {
		return nil, err
	}
	return rowStep(func(r Row) (Row, bool, error) {
		keep, err := evalBool(e, r.F)
		return r, keep, err
	}), nil
}

// abortStep fails the whole run on the first row its check refuses.
type abortStep func(Row) error

func (check abortStep) Run(rows []Row, _ func(Row, error)) ([]Row, error) {
	for _, r := range rows {
		if err := check(r); err != nil {
			return nil, fmt.Errorf("row %d: %w", r.N, err)
		}
	}
	return rows, nil
}

func newValidate(raw json.RawMessage) (Step, error) {
	p, err := params[struct {
		Rule   string
		OnFail string `json:"on_fail"`
	}](raw)
	if err != nil {
		return nil, err
	}
	e, err := compile(p.Rule)
	if err != nil {
		return nil, err
	}
	check := func(r Row) error {
		ok, err := evalBool(e, r.F)
		if err == nil && !ok {
			err = fmt.Errorf("validation failed: %s", p.Rule)
		}
		return err
	}
	switch p.OnFail {
	case "", "reject":
		return rowStep(func(r Row) (Row, bool, error) { return r, true, check(r) }), nil
	case "abort":
		return abortStep(check), nil
	}
	return nil, fmt.Errorf(`on_fail must be "reject" or "abort", not %q`, p.OnFail)
}

type aggregate struct {
	GroupBy []string `json:"group_by"`
	Fn      string
	Field   string
	As      string
}

func newAggregate(raw json.RawMessage) (Step, error) {
	a, err := params[aggregate](raw)
	if err != nil {
		return nil, err
	}
	switch a.Fn {
	case "sum", "avg", "min", "max":
		if a.Field == "" {
			return nil, fmt.Errorf(`aggregate %s needs "field"`, a.Fn)
		}
	case "count":
	default:
		return nil, fmt.Errorf(`aggregate fn must be sum, count, avg, min or max, not %q`, a.Fn)
	}
	if a.As == "" {
		return nil, errors.New(`aggregate needs "as"`)
	}
	return a, nil
}

func (a aggregate) Run(rows []Row, reject func(Row, error)) ([]Row, error) {
	type group struct {
		keys             map[string]any
		n, sum, min, max float64
	}
	groups := map[string]*group{}
	var order []*group

rows:
	for _, r := range rows {
		keys, id := map[string]any{}, ""
		for _, k := range a.GroupBy {
			v, ok := r.F[k]
			if !ok {
				reject(r, fmt.Errorf("missing field %q", k))
				continue rows
			}
			keys[k] = v
			id += fmt.Sprint(v) + "\x00"
		}
		var v float64
		if a.Fn != "count" {
			var ok bool
			if v, ok = toNum(r.F[a.Field]); !ok {
				reject(r, fmt.Errorf("%s: %#v is not a number", a.Field, r.F[a.Field]))
				continue
			}
		}
		g := groups[id]
		if g == nil {
			g = &group{keys: keys, min: v, max: v}
			groups[id] = g
			order = append(order, g)
		}
		g.n++
		g.sum += v
		g.min, g.max = min(g.min, v), max(g.max, v)
	}

	out := make([]Row, 0, len(order))
	for _, g := range order {
		g.keys[a.As] = map[string]float64{
			"sum": g.sum, "count": g.n, "avg": g.sum / g.n, "min": g.min, "max": g.max,
		}[a.Fn]
		out = append(out, Row{F: g.keys})
	}
	return out, nil
}
