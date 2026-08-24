package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// createTimeout covers `up -d --build` (image build can be slow on first run).
const createTimeout = 15 * time.Minute

// createCellRequest is the NON-SECRET per-cell profile the Portal sends. The big
// shared secrets never appear here — they come from the host-local secrets
// template (AGENT_SECRETS_ENV or an existing .env.cellN).
type createCellRequest struct {
	Region        string `json:"region"`
	Node          string `json:"node"`
	MultiEgress   *bool  `json:"multiEgress"`   // default true
	UpstreamProxy string `json:"upstreamProxy"` // fallback egress; optional
	TZ            string `json:"tz"`            // optional
}

// createMu serializes allocation+create so two concurrent creates never pick the
// same project/port.
func (a *Agent) handleCellCreate(w http.ResponseWriter, r *http.Request) {
	if a.cfg.PublicIP == "" {
		writeErr(w, http.StatusPreconditionFailed, "AGENT_PUBLIC_IP not set — cannot compute advertise address")
		return
	}

	var req createCellRequest
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	req.Region = strings.TrimSpace(req.Region)
	req.Node = strings.TrimSpace(req.Node)
	if req.Region == "" {
		writeErr(w, http.StatusBadRequest, "region is required")
		return
	}

	a.createMu.Lock()
	defer a.createMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), createTimeout)
	defer cancel()

	project, port, err := a.allocate(ctx)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}

	// Render .env.<project> = host secrets template + non-secret per-cell profile.
	kv, err := a.secretsTemplate()
	if err != nil {
		writeErr(w, http.StatusPreconditionFailed, err.Error())
		return
	}
	advertise := fmt.Sprintf("http://%s:%d", a.cfg.PublicIP, port)
	kv["CELL_PORT"] = strconv.Itoa(port)
	kv["CELL_ADVERTISE_ADDR"] = advertise
	kv["CELL_REGION"] = strings.ToUpper(req.Region)
	kv["CELL_NODE"] = req.Node
	kv["EDGE_MODE"] = "1"
	multi := req.MultiEgress == nil || *req.MultiEgress // default on
	if multi {
		kv["CELL_MULTI_EGRESS"] = "1"
	} else {
		kv["CELL_MULTI_EGRESS"] = ""
	}
	kv["CELL_UPSTREAM_PROXY"] = strings.TrimSpace(req.UpstreamProxy)
	if tz := strings.TrimSpace(req.TZ); tz != "" {
		kv["TZ"] = tz
	}

	envPath := filepath.Join(a.cfg.DeployDir, ".env."+project)
	if err := writeEnvFile(envPath, kv); err != nil {
		writeErr(w, http.StatusInternalServerError, "write env failed: "+firstLine(err.Error()))
		return
	}

	// Bring it up (build on first run). envPathFor() picks up the file we wrote.
	out, err := a.compose(ctx, project, "up", "-d", "--build")
	if err != nil {
		slog.Warn("cell-agent create failed", "project", project, "err", firstLine(err.Error()))
		writeErr(w, http.StatusBadGateway, "docker compose up failed: "+firstLine(out+" "+err.Error()))
		return
	}

	cs := CellState{
		Project:       project,
		Port:          port,
		AdvertiseAddr: advertise,
		Region:        strings.ToUpper(req.Region),
		Node:          req.Node,
		EnvPath:       envPath,
	}
	if err := a.state.Put(cs); err != nil {
		slog.Warn("cell-agent state persist failed (cell is up)", "project", project, "err", err)
	}
	slog.Info("cell-agent created cell", "project", project, "port", port, "region", cs.Region)

	writeJSON(w, http.StatusOK, map[string]any{
		"project":       project,
		"port":          port,
		"advertiseAddr": advertise,
		"region":        cs.Region,
		"node":          cs.Node,
	})
}
