// Command cell-agent is a small authenticated daemon that runs on each cell
// HOST. The Provider Portal calls it over HTTP (per-host bearer token) to drive
// `docker compose` for cells on that host — so admins never SSH in. It never
// exposes the raw Docker API: every operation is a fixed compose verb against a
// validated `cellN` project. Big secrets stay on the host; the Portal only sends
// non-secret per-cell profile (phase 2). Bind it to a private interface only.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type Agent struct {
	cfg   *AgentConfig
	state *State

	lockMu sync.Mutex
	locks  map[string]*sync.Mutex

	// createMu serializes cell creation (allocation + up) so concurrent creates
	// never race on the same project name / host port.
	createMu sync.Mutex
}

func (a *Agent) projectLock(project string) *sync.Mutex {
	a.lockMu.Lock()
	defer a.lockMu.Unlock()
	m, ok := a.locks[project]
	if !ok {
		m = &sync.Mutex{}
		a.locks[project] = m
	}
	return m
}

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		slog.Error("cell-agent config error", "err", err)
		os.Exit(1)
	}
	a := &Agent{cfg: cfg, state: LoadState(cfg.StateFile), locks: map[string]*sync.Mutex{}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/v1/health", a.auth(a.handleHealth))
	mux.HandleFunc("GET /agent/v1/cells", a.auth(a.handleListCells))
	mux.HandleFunc("POST /agent/v1/cells", a.auth(a.handleCellCreate))
	mux.HandleFunc("GET /agent/v1/cells/{project}/status", a.auth(a.handleCellStatus))
	mux.HandleFunc("POST /agent/v1/cells/{project}/start", a.auth(a.handleCellStart))
	mux.HandleFunc("POST /agent/v1/cells/{project}/stop", a.auth(a.handleCellStop))
	mux.HandleFunc("POST /agent/v1/cells/{project}/restart", a.auth(a.handleCellRestart))
	mux.HandleFunc("POST /agent/v1/cells/{project}/down", a.auth(a.handleCellDown))

	srv := &http.Server{
		Addr:              cfg.BindAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("cell-agent listening", "addr", cfg.BindAddr, "deployDir", cfg.DeployDir, "portRange", []int{cfg.PortRangeStart, cfg.PortRangeEnd})
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("cell-agent server error", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("cell-agent shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}
