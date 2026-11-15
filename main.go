package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// Overridden at build time with -ldflags "-X main.version=...".
var (
	version = "dev"
	commit  = "none"
	builtAt = "unknown"
)

const (
	app     = "piper"
	author  = "Stefan Faur"
	group   = "FILL IN GROUP"
	project = "Project 6 - ETL Pipeline Builder"
)

var errorCodes = map[int]string{
	http.StatusBadRequest:          "validation",
	http.StatusNotFound:            "not_found",
	http.StatusConflict:            "conflict",
	http.StatusUnprocessableEntity: "unprocessable",
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": errorCodes[status], "message": msg})
}

func (s *Store) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":         "ok",
			"uptime_seconds": int(time.Since(s.started).Seconds()),
		})
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"app": app, "version": version, "commit": commit, "built_at": builtAt,
		})
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!doctype html><title>%s</title><h1>%s</h1><p>%s</p><p>%s, group %s</p>",
			app, app, project, author, group)
	})
	mux.HandleFunc("POST /pipelines", s.createPipeline)
	mux.HandleFunc("GET /pipelines", s.listPipelines)
	mux.HandleFunc("POST /reset", func(w http.ResponseWriter, r *http.Request) {
		s.reset()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func main() {
	s := newStore(cmp.Or(os.Getenv("DATA_DIR"), "data"))
	slog.Info("listening", "addr", ":8080", "version", version)
	if err := http.ListenAndServe(":8080", s.routes()); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
