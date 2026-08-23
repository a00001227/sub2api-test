package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// CellState is the agent's record of a cell it created (phase 2). In phase 1
// this may be empty — lifecycle ops fall back to `docker compose ls` discovery.
type CellState struct {
	Project       string `json:"project"`
	Port          int    `json:"port"`
	AdvertiseAddr string `json:"advertiseAddr"`
	Region        string `json:"region"`
	Node          string `json:"node"`
	EnvPath       string `json:"envPath"`
}

// State is a small JSON-file-backed store of agent-created cells. Concurrency is
// guarded by a mutex; writes are atomic (temp file + rename).
type State struct {
	mu    sync.Mutex
	path  string
	cells map[string]CellState
}

type statePayload struct {
	Cells map[string]CellState `json:"cells"`
}

func LoadState(path string) *State {
	s := &State{path: path, cells: map[string]CellState{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return s // missing/unreadable → empty store
	}
	var p statePayload
	if json.Unmarshal(b, &p) == nil && p.Cells != nil {
		s.cells = p.Cells
	}
	return s
}

func (s *State) Get(project string) (CellState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs, ok := s.cells[project]
	return cs, ok
}

func (s *State) Put(cs CellState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cells[cs.Project] = cs
	return s.flushLocked()
}

func (s *State) Delete(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cells, project)
	return s.flushLocked()
}

func (s *State) List() []CellState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CellState, 0, len(s.cells))
	for _, cs := range s.cells {
		out = append(out, cs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Project < out[j].Project })
	return out
}

// UsedPorts returns the ports the agent has recorded (phase 2 allocation).
func (s *State) UsedPorts() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	ports := make([]int, 0, len(s.cells))
	for _, cs := range s.cells {
		if cs.Port > 0 {
			ports = append(ports, cs.Port)
		}
	}
	sort.Ints(ports)
	return ports
}

func (s *State) flushLocked() error {
	b, err := json.MarshalIndent(statePayload{Cells: s.cells}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
