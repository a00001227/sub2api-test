package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// projectPattern restricts every lifecycle op to cell compose projects. The
// verb is always a fixed switch (never interpolated from the request), and the
// project name is validated against this before it ever reaches the shell.
var projectPattern = regexp.MustCompile(`^cell[0-9]+$`)

func validProject(p string) bool { return projectPattern.MatchString(p) }

// compose runs `docker compose -p <project> -f <composeFile> [--env-file X] <args...>`
// from DeployDir and returns combined output. args is a fixed verb list built by
// the caller — the request body never contributes to it.
func (a *Agent) compose(ctx context.Context, project string, args ...string) (string, error) {
	full := []string{"compose", "-p", project, "-f", a.cfg.ComposeFile}
	// Include the per-cell env file when present (needed for `up` interpolation;
	// harmless for stop/restart). Convention: .env.<project> in DeployDir, or the
	// path recorded in agent state.
	if envPath := a.envPathFor(project); envPath != "" {
		full = append(full, "--env-file", envPath)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Dir = a.cfg.DeployDir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (a *Agent) envPathFor(project string) string {
	if cs, ok := a.state.Get(project); ok && cs.EnvPath != "" {
		if fileExists(cs.EnvPath) {
			return cs.EnvPath
		}
	}
	p := filepath.Join(a.cfg.DeployDir, ".env."+project)
	if fileExists(p) {
		return p
	}
	return ""
}

// composeLsEntry is one row of `docker compose ls --format json`.
type composeLsEntry struct {
	Name   string `json:"Name"`
	Status string `json:"Status"`
}

// listProjects returns cell compose projects docker currently knows about
// (running or partially running), filtered to the cell naming pattern.
func (a *Agent) listProjects(ctx context.Context) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, "docker", "compose", "ls", "--all", "--format", "json")
	cmd.Dir = a.cfg.DeployDir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var entries []composeLsEntry
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return map[string]string{}, nil
	}
	if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
		return nil, err
	}
	res := map[string]string{}
	for _, e := range entries {
		if validProject(e.Name) {
			res[e.Name] = e.Status
		}
	}
	return res, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
