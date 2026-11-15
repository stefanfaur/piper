package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
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

func define(t *testing.T, h http.Handler, body string) {
	t.Helper()
	if code, resp := do(t, h, "POST", "/pipelines", body); code != 201 {
		t.Fatalf("define: %d %s", code, resp)
	}
}

func runPipeline(t *testing.T, h http.Handler, id string) Report {
	t.Helper()
	code, body := do(t, h, "POST", "/pipelines/"+id+"/run", "")
	if code != 200 {
		t.Fatalf("run %s: %d %s", id, code, body)
	}
	return decode[Report](t, body)
}

func rowsOf(t *testing.T, h http.Handler, dest string) []map[string]any {
	t.Helper()
	code, body := do(t, h, "GET", "/data/"+dest, "")
	if code != 200 {
		t.Fatalf("data %s: %d %s", dest, code, body)
	}
	return decode[[]map[string]any](t, body)
}

func TestExampleRun(t *testing.T) {
	h := newStore(t.TempDir()).routes()
	define(t, h, example)

	run := runPipeline(t, h, "1")
	if run.RunID != 1 || run.Status != "completed" || run.Read != 3 || run.Written != 1 || run.Rejected != 1 {
		t.Errorf("run: %+v", run)
	}

	_, body := do(t, h, "GET", "/runs/1", "")
	rep := decode[Report](t, body)
	if len(rep.Rejections) != 1 {
		t.Fatalf("report: %s", body)
	}
	rej := rep.Rejections[0]
	if rej.Row != 3 || rej.Step != 2 || rej.Op != "compute" || rej.Reason != `qty: "x" is not a number` || rej.Data["product"] != "chair" {
		t.Errorf("rejection: %+v", rej)
	}

	rows := rowsOf(t, h, "sales_clean")
	if len(rows) != 1 || rows[0]["product"] != "desk" || rows[0]["total"] != 300.0 {
		t.Errorf("data: %v", rows)
	}

	if again := runPipeline(t, h, "1"); again.RunID != 2 || again.Written != 1 {
		t.Errorf("second run: %+v", again)
	}
	if rows = rowsOf(t, h, "sales_clean"); len(rows) != 1 {
		t.Errorf("data after second run: %v", rows)
	}
	if _, body = do(t, h, "GET", "/runs", ""); len(decode[[]Report](t, body)) != 2 {
		t.Errorf("runs: %s", body)
	}
}

func TestAbortLeavesDestinationUntouched(t *testing.T) {
	h := newStore(t.TempDir()).routes()
	define(t, h, `{"name":"a","source":{"type":"inline","rows":[{"n":1}]},"destination":"out"}`)
	define(t, h, `{"name":"b","source":{"type":"inline","rows":[{"n":5},{"n":-1}]},
		"steps":[{"op":"validate","rule":"n > 0","on_fail":"abort"}],"destination":"out"}`)
	runPipeline(t, h, "1")

	if run := runPipeline(t, h, "2"); run.Status != "failed" || run.Written != 0 {
		t.Errorf("run: %+v", run)
	}
	_, body := do(t, h, "GET", "/runs/2", "")
	if rep := decode[Report](t, body); !strings.Contains(rep.Error, "step 1 (validate): row 2") {
		t.Errorf("report: %s", body)
	}
	if rows := rowsOf(t, h, "out"); len(rows) != 1 || rows[0]["n"] != 1.0 {
		t.Errorf("destination changed: %v", rows)
	}
}

func TestRejectRatio(t *testing.T) {
	h := newStore(t.TempDir()).routes()
	steps := `"steps":[{"op":"compute","field":"d","expr":"n * 2"}]`
	define(t, h, `{"name":"most","source":{"type":"inline","rows":[{"n":"x"},{"n":"y"},{"n":1}]},`+steps+`,"destination":"most"}`)
	define(t, h, `{"name":"half","source":{"type":"inline","rows":[{"n":"x"},{"n":1}]},`+steps+`,"destination":"half"}`)

	if run := runPipeline(t, h, "1"); run.Status != "failed" || run.Read != 3 || run.Written != 0 || run.Rejected != 2 {
		t.Errorf("above the limit: %+v", run)
	}
	if code, _ := do(t, h, "GET", "/data/most", ""); code != 404 {
		t.Errorf("failed run created its destination: %d", code)
	}
	if run := runPipeline(t, h, "2"); run.Status != "completed" || run.Written != 1 {
		t.Errorf("exactly at the limit: %+v", run)
	}
}

func TestFileDestinationAndTableSource(t *testing.T) {
	dir := t.TempDir()
	h := newStore(dir).routes()
	define(t, h, example)
	define(t, h, `{"name":"to-file","source":{"type":"table","name":"sales_clean"},"destination":{"type":"file","name":"f"}}`)
	define(t, h, `{"name":"ghost","source":{"type":"table","name":"ghost"},"destination":"g"}`)
	runPipeline(t, h, "1")

	if run := runPipeline(t, h, "2"); run.Status != "completed" || run.Read != 1 || run.Written != 1 {
		t.Errorf("table to file: %+v", run)
	}
	if rows := rowsOf(t, h, "f"); len(rows) != 1 || rows[0]["product"] != "desk" {
		t.Errorf("file data: %v", rows)
	}
	if files, _ := os.ReadDir(dir); len(files) != 1 || files[0].Name() != "f.json" {
		t.Errorf("data dir: %v", files)
	}

	run := runPipeline(t, h, "3")
	_, body := do(t, h, "GET", "/runs/3", "")
	if run.Status != "failed" || !strings.Contains(decode[Report](t, body).Error, "ghost") {
		t.Errorf("missing table: %s", body)
	}

	do(t, h, "POST", "/reset", "")
	if code, _ := do(t, h, "GET", "/data/f", ""); code != 404 {
		t.Errorf("file survived reset: %d", code)
	}
	if _, body = do(t, h, "GET", "/runs", ""); strings.TrimSpace(string(body)) != "[]" {
		t.Errorf("runs after reset: %s", body)
	}
	if code, body := do(t, h, "POST", "/pipelines", example); code != 201 || decode[Pipeline](t, body).ID != 1 {
		t.Errorf("ids after reset: %s", body)
	}
}

func TestRunRouteErrors(t *testing.T) {
	h := newStore(t.TempDir()).routes()
	for path, want := range map[string]int{"/pipelines/9/run": 404, "/pipelines/abc/run": 400} {
		if code, _ := do(t, h, "POST", path, ""); code != want {
			t.Errorf("POST %s: %d", path, code)
		}
	}
	for path, want := range map[string]int{"/runs/9": 404, "/runs/abc": 400, "/data/nope": 404} {
		if code, _ := do(t, h, "GET", path, ""); code != want {
			t.Errorf("GET %s: %d", path, code)
		}
	}
}

func TestSchedule(t *testing.T) {
	s := newStore(t.TempDir())
	h := s.routes()
	define(t, h, example)

	s.mu.Lock()
	s.schedule(s.pipelines[0], 10*time.Millisecond)
	s.mu.Unlock()

	var runs []Summary
	for deadline := time.Now().Add(2 * time.Second); len(runs) < 2 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		_, body := do(t, h, "GET", "/runs", "")
		runs = decode[[]Summary](t, body)
	}
	if len(runs) < 2 || runs[0].Trigger != "schedule" || runs[0].Status != "completed" {
		t.Fatalf("scheduled runs: %+v", runs)
	}

	do(t, h, "POST", "/reset", "")
	time.Sleep(50 * time.Millisecond)
	if _, body := do(t, h, "GET", "/runs", ""); strings.TrimSpace(string(body)) != "[]" {
		t.Errorf("scheduler survived reset: %s", body)
	}
}
