package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

const composeTimeout = 5 * time.Minute

func (a *Agent) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"version":      a.cfg.Version,
		"hostPublicIp": a.cfg.PublicIP,
		"portRange":    map[string]int{"start": a.cfg.PortRangeStart, "end": a.cfg.PortRangeEnd},
		"usedPorts":    a.state.UsedPorts(),
		"cellCount":    len(a.state.List()),
	})
}

// handleListCells lists cell compose projects docker knows about, marking which
// are running. Merges live discovery with agent-created state.
func (a *Agent) handleListCells(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	statuses, err := a.listProjects(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "docker compose ls failed: "+firstLine(err.Error()))
		return
	}
	type item struct {
		Project       string `json:"project"`
		Running       bool   `json:"running"`
		Status        string `json:"status"`
		Port          int    `json:"port,omitempty"`
		AdvertiseAddr string `json:"advertiseAddr,omitempty"`
		Region        string `json:"region,omitempty"`
		Node          string `json:"node,omitempty"`
	}
	seen := map[string]*item{}
	for name, status := range statuses {
		seen[name] = &item{Project: name, Status: status, Running: strings.Contains(strings.ToLower(status), "running")}
	}
	for _, cs := range a.state.List() {
		it, ok := seen[cs.Project]
		if !ok {
			it = &item{Project: cs.Project}
			seen[cs.Project] = it
		}
		it.Port, it.AdvertiseAddr, it.Region, it.Node = cs.Port, cs.AdvertiseAddr, cs.Region, cs.Node
	}
	// Authoritative host port per project from running containers (Portal's
	// "adopt cells" matches Portal cells to projects by this port).
	for project, port := range a.containerPorts(ctx) {
		if it, ok := seen[project]; ok {
			it.Port = port
		}
	}
	out := make([]item, 0, len(seen))
	for _, it := range seen {
		out = append(out, *it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"cells": out})
}

func (a *Agent) handleCellStatus(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if !validProject(project) {
		writeErr(w, http.StatusBadRequest, "invalid project name")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out, err := a.compose(ctx, project, "ps", "--format", "json")
	if err != nil {
		writeErr(w, http.StatusBadGateway, "docker compose ps failed: "+firstLine(err.Error()))
		return
	}
	running := strings.Contains(out, `"State":"running"`) || strings.Contains(out, `"running"`)
	writeJSON(w, http.StatusOK, map[string]any{"project": project, "running": running, "raw": strings.TrimSpace(out)})
}

func (a *Agent) handleCellStart(w http.ResponseWriter, r *http.Request) {
	a.lifecycle(w, r, "start", []string{"up", "-d"}, true)
}

func (a *Agent) handleCellStop(w http.ResponseWriter, r *http.Request) {
	// `stop` (NOT `down`) — recoverable, never removes containers/volumes.
	a.lifecycle(w, r, "stop", []string{"stop"}, false)
}

func (a *Agent) handleCellRestart(w http.ResponseWriter, r *http.Request) {
	a.lifecycle(w, r, "restart", []string{"restart"}, false)
}

// handleCellDown tears a cell fully down: `docker compose down -v` (stop + remove
// containers, networks AND volumes), then removes the agent-rendered .env.<project>
// and its state entry so the port/project frees up. Used by the Portal's one-shot
// delete — a stopped-but-present cell would keep self-registering. Destructive by
// design (the Portal confirms + guards); still a FIXED verb against a validated
// cellN project.
func (a *Agent) handleCellDown(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if !validProject(project) {
		writeErr(w, http.StatusBadRequest, "invalid project name")
		return
	}

	lock := a.projectLock(project)
	lock.Lock()
	defer lock.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), composeTimeout)
	defer cancel()

	// Resolve the env file BEFORE `down` removes nothing on disk — down still needs
	// it for interpolation. Capture it now, delete after a successful teardown.
	envPath := a.envPathFor(project)

	out, err := a.compose(ctx, project, "down", "-v")
	if err != nil {
		slog.Warn("cell-agent down failed", "project", project, "err", firstLine(err.Error()))
		writeErr(w, http.StatusBadGateway, "docker compose down failed: "+firstLine(out+" "+err.Error()))
		return
	}

	// Best-effort cleanup: drop the rendered env + state so nothing lingers.
	if envPath != "" && strings.HasPrefix(envPath, a.cfg.DeployDir) {
		if rmErr := os.Remove(envPath); rmErr != nil && !os.IsNotExist(rmErr) {
			slog.Warn("cell-agent down: env cleanup failed", "project", project, "err", rmErr)
		}
	}
	_ = a.state.Delete(project)
	slog.Info("cell-agent down ok", "project", project)
	writeJSON(w, http.StatusOK, map[string]any{"project": project, "action": "down"})
}

// lifecycle runs a FIXED compose verb against a validated project, serialized
// per-project. `verb` args are a constant list; the request body is never used.
func (a *Agent) lifecycle(w http.ResponseWriter, r *http.Request, action string, verb []string, running bool) {
	project := r.PathValue("project")
	if !validProject(project) {
		writeErr(w, http.StatusBadRequest, "invalid project name")
		return
	}

	lock := a.projectLock(project)
	lock.Lock()
	defer lock.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), composeTimeout)
	defer cancel()

	out, err := a.compose(ctx, project, verb...)
	if err != nil {
		slog.Warn("cell-agent lifecycle failed", "action", action, "project", project, "err", firstLine(err.Error()))
		writeErr(w, http.StatusBadGateway, "docker compose "+action+" failed: "+firstLine(out+" "+err.Error()))
		return
	}
	slog.Info("cell-agent lifecycle ok", "action", action, "project", project)
	writeJSON(w, http.StatusOK, map[string]any{"project": project, "action": action, "running": running})
}

// ── response helpers ────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
