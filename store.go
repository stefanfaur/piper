package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Pipeline struct{}

type Report struct{}

type Store struct {
	mu        sync.Mutex
	pipelines []*Pipeline
	runs      []*Report
	tables    map[string][]map[string]any
	dataDir   string
	ctx       context.Context
	cancel    context.CancelFunc
	started   time.Time
}

func newStore(dataDir string) *Store {
	s := &Store{dataDir: dataDir, started: time.Now()}
	s.clear()
	return s
}

func (s *Store) clear() {
	s.pipelines = []*Pipeline{}
	s.runs = []*Report{}
	s.tables = map[string][]map[string]any{}
	s.ctx, s.cancel = context.WithCancel(context.Background())
}

func (s *Store) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel()
	s.clear()
	for _, pattern := range []string{"*.json", "*.tmp"} {
		files, _ := filepath.Glob(filepath.Join(s.dataDir, pattern))
		for _, f := range files {
			os.Remove(f)
		}
	}
}
