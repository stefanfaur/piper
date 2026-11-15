package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func do(t *testing.T, h http.Handler, method, path, body string) (int, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec.Code, rec.Body.Bytes()
}

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("bad JSON %q: %v", b, err)
	}
	return v
}

func TestCommonRoutes(t *testing.T) {
	h := newStore(t.TempDir()).routes()

	code, body := do(t, h, "GET", "/health", "")
	health := decode[map[string]any](t, body)
	if _, isNum := health["uptime_seconds"].(float64); code != 200 || health["status"] != "ok" || !isNum {
		t.Errorf("health: %d %s", code, body)
	}

	code, body = do(t, h, "GET", "/version", "")
	ver := decode[map[string]string](t, body)
	for _, k := range []string{"app", "version", "commit", "built_at"} {
		if ver[k] == "" {
			t.Errorf("version: missing %q in %s", k, body)
		}
	}
	if code != 200 {
		t.Errorf("version: %d", code)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Errorf("index: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}

	if code, body = do(t, h, "POST", "/reset", ""); code != 204 || len(body) != 0 {
		t.Errorf("reset: %d %q", code, body)
	}
	if code, _ = do(t, h, "GET", "/nope", ""); code != 404 {
		t.Errorf("unknown route: %d", code)
	}
}

func TestEval(t *testing.T) {
	row := map[string]any{"qty": "2", "unit_price": "150.00", "name": "desk", "n": 3.0, "bad": "x"}
	cases := []struct {
		expr string
		want any
		err  string
	}{
		{"qty * unit_price", 300.0, ""},
		{"1 + 2 * 3", 7.0, ""},
		{"(1 + 2) * 3", 9.0, ""},
		{"-n", -3.0, ""},
		{`n >= 3 && name == "desk"`, true, ""},
		{"!(n < 3) || false", true, ""},
		{"qty == 2", true, ""},
		{`name != "lamp"`, true, ""},
		{"bad * 2", nil, `bad: "x" is not a number`},
		{"missing + 1", nil, `unknown field "missing"`},
		{"n / 0", nil, "division by zero"},
	}
	for _, c := range cases {
		e, err := compile(c.expr)
		if err != nil {
			t.Errorf("compile(%q): %v", c.expr, err)
			continue
		}
		got, err := eval(e, row)
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Errorf("eval(%q) error = %v, want %q", c.expr, err, c.err)
			}
		} else if err != nil || got != c.want {
			t.Errorf("eval(%q) = %v, %v, want %v", c.expr, got, err, c.want)
		}
	}
}

func TestCompileRejects(t *testing.T) {
	for _, src := range []string{"qty *", "f(1)", "a[0]", "a.b", ""} {
		if _, err := compile(src); err == nil {
			t.Errorf("compile(%q) succeeded", src)
		}
	}
}

func runSteps(t *testing.T, stepsJSON string, in ...map[string]any) (out []Row, rejected []string, err error) {
	t.Helper()
	steps, _, err := buildSteps(decode[[]json.RawMessage](t, []byte(stepsJSON)))
	if err != nil {
		t.Fatalf("buildSteps: %v", err)
	}
	for i, f := range in {
		out = append(out, Row{N: i + 1, F: f})
	}
	for _, st := range steps {
		out, err = st.Run(out, func(_ Row, e error) { rejected = append(rejected, e.Error()) })
		if err != nil {
			return nil, rejected, err
		}
	}
	return out, rejected, nil
}

func TestSteps(t *testing.T) {
	ns := func(vals ...float64) (rows []map[string]any) {
		for _, v := range vals {
			rows = append(rows, map[string]any{"n": v})
		}
		return rows
	}

	out, rej, _ := runSteps(t, `[{"op":"rename","from":"a","to":"x"},{"op":"select","fields":["x"]}]`,
		map[string]any{"a": "1", "b": "2"})
	if len(out) != 1 || len(out[0].F) != 1 || out[0].F["x"] != "1" || len(rej) != 0 {
		t.Errorf("rename+select: %v %v", out, rej)
	}

	out, rej, _ = runSteps(t, `[{"op":"rename","from":"a","to":"x"}]`, map[string]any{"b": "2"})
	if len(out) != 0 || len(rej) != 1 || rej[0] != `missing field "a"` {
		t.Errorf("rename missing: %v %v", out, rej)
	}

	out, rej, _ = runSteps(t, `[{"op":"filter","where":"n > 1"}]`, ns(1, 2, 3)...)
	if len(out) != 2 || len(rej) != 0 {
		t.Errorf("filter: %v %v", out, rej)
	}

	out, rej, _ = runSteps(t, `[{"op":"validate","rule":"n > 1"}]`, ns(1, 2)...)
	if len(out) != 1 || len(rej) != 1 || rej[0] != "validation failed: n > 1" {
		t.Errorf("validate reject: %v %v", out, rej)
	}

	_, _, err := runSteps(t, `[{"op":"validate","rule":"n > 1","on_fail":"abort"}]`, ns(1, 2)...)
	if err == nil || err.Error() != "row 1: validation failed: n > 1" {
		t.Errorf("validate abort: %v", err)
	}

	sales := []map[string]any{{"p": "a", "v": "1"}, {"p": "b", "v": 5.0}, {"p": "a", "v": 2.0}, {"p": "a", "v": "x"}}
	out, rej, _ = runSteps(t, `[{"op":"aggregate","group_by":["p"],"fn":"sum","field":"v","as":"total"}]`, sales...)
	if len(out) != 2 || out[0].F["p"] != "a" || out[0].F["total"] != 3.0 || out[0].N != 0 ||
		out[1].F["p"] != "b" || out[1].F["total"] != 5.0 || len(rej) != 1 {
		t.Errorf("aggregate sum: %v %v", out, rej)
	}

	out, _, _ = runSteps(t, `[{"op":"aggregate","group_by":["p"],"fn":"count","as":"n"}]`, sales...)
	if len(out) != 2 || out[0].F["n"] != 3.0 || out[1].F["n"] != 1.0 {
		t.Errorf("aggregate count: %v", out)
	}
}

func TestBuildStepsErrors(t *testing.T) {
	cases := map[string]string{
		`[{"op":"rename","from":"a","to":"b"},{"op":"pivot"}]`: `step 2: unknown op "pivot"`,
		`[{"op":"compute","field":"t","expr":"a *"}]`:          "step 1:",
		`[{"op":"validate","rule":"a > 0","on_fail":"boom"}]`:  "step 1:",
		`[{"op":"aggregate","fn":"median","as":"m"}]`:          "step 1:",
	}
	for in, want := range cases {
		_, _, err := buildSteps(decode[[]json.RawMessage](t, []byte(in)))
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("buildSteps(%s) = %v, want prefix %q", in, err, want)
		}
	}
}

const example = `{"name":"clean-sales",
 "source":{"type":"inline","rows":[
   {"prod":"desk","qty":"2","unit_price":"150.00"},
   {"prod":"lamp","qty":"1","unit_price":"19.99"},
   {"prod":"chair","qty":"x","unit_price":"75.00"}]},
 "steps":[
   {"op":"rename","from":"prod","to":"product"},
   {"op":"compute","field":"total","expr":"qty * unit_price"},
   {"op":"filter","where":"total >= 100"}],
 "destination":"sales_clean"}`

func TestPipelineRoutes(t *testing.T) {
	h := newStore(t.TempDir()).routes()

	if code, body := do(t, h, "GET", "/pipelines", ""); code != 200 || strings.TrimSpace(string(body)) != "[]" {
		t.Errorf("empty list: %d %s", code, body)
	}

	code, body := do(t, h, "POST", "/pipelines", example)
	created := decode[Pipeline](t, body)
	if code != 201 || created.ID != 1 || created.Destination != (Destination{"memory", "sales_clean"}) {
		t.Errorf("create: %d %s", code, body)
	}

	rejected := []struct {
		name, body string
		code       int
		errCode    string
		contains   string
	}{
		{"duplicate", example, 409, "conflict", "clean-sales"},
		{"unknown op", `{"name":"a","source":{"type":"inline"},"steps":[{"op":"rename","from":"a","to":"b"},{"op":"pivot"}],"destination":"d"}`, 422, "unprocessable", `step 2: unknown op "pivot"`},
		{"unknown source", `{"name":"a","source":{"type":"ftp"},"destination":"d"}`, 422, "unprocessable", "ftp"},
		{"bad table name", `{"name":"a","source":{"type":"table","name":"a/b"},"destination":"d"}`, 422, "unprocessable", ""},
		{"bad json", `{"name":`, 400, "validation", ""},
		{"no name", `{"source":{"type":"inline"},"destination":"d"}`, 400, "validation", "name"},
		{"no source", `{"name":"a","destination":"d"}`, 400, "validation", "source"},
		{"bad destination", `{"name":"a","source":{"type":"inline"},"destination":"../etc"}`, 400, "validation", ""},
		{"bad destination type", `{"name":"a","source":{"type":"inline"},"destination":{"type":"s3","name":"d"}}`, 400, "validation", ""},
		{"bad schedule", `{"name":"a","source":{"type":"inline"},"destination":"d","schedule":"soon"}`, 400, "validation", ""},
		{"short schedule", `{"name":"a","source":{"type":"inline"},"destination":"d","schedule":"10ms"}`, 400, "validation", ""},
	}
	for _, c := range rejected {
		code, body := do(t, h, "POST", "/pipelines", c.body)
		e := decode[map[string]string](t, body)
		if code != c.code || e["error"] != c.errCode || !strings.Contains(e["message"], c.contains) {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
	}

	code, body = do(t, h, "POST", "/pipelines", `{"name":"f","source":{"type":"inline"},"destination":{"type":"file","name":"out"}}`)
	if code != 201 || decode[Pipeline](t, body).ID != 2 {
		t.Errorf("file destination: %d %s", code, body)
	}
	if _, body = do(t, h, "GET", "/pipelines", ""); len(decode[[]Pipeline](t, body)) != 2 {
		t.Errorf("list: %s", body)
	}
}

func TestReadCSV(t *testing.T) {
	s := newStore(t.TempDir())
	rows, rejected, read, err := s.readSource(Source{Type: "csv", Data: "a,b\n1,2\n3\n4,5\n"})
	if err != nil || read != 3 || len(rows) != 2 || rows[0].N != 1 || rows[1].N != 3 || rows[1].F["b"] != "5" {
		t.Errorf("rows: %v read=%d err=%v", rows, read, err)
	}
	if len(rejected) != 1 || rejected[0].Row != 2 || rejected[0].Op != "source" {
		t.Errorf("rejected: %+v", rejected)
	}
}
