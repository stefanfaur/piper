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
