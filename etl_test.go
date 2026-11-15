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
