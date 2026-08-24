package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// perCellKeys are the env keys the agent sets per new cell. Everything ELSE in
// the secrets template (POSTGRES_PASSWORD/JWT_SECRET/tokens/webhook/registry/
// ADMIN_*…) is a shared secret carried over unchanged — the Portal never sends
// those.
var perCellKeys = map[string]bool{
	"CELL_PORT":           true,
	"CELL_ADVERTISE_ADDR": true,
	"CELL_REGION":         true,
	"CELL_NODE":           true,
	"CELL_UPSTREAM_PROXY": true,
	"CELL_MULTI_EGRESS":   true,
	"TZ":                  true,
	"EDGE_MODE":           true,
}

var envCellRe = regexp.MustCompile(`^\.env\.(cell[0-9]+)$`)

// parseEnvFile reads KEY=VALUE lines into an ordered-agnostic map (ignores
// comments and blank lines; strips optional surrounding quotes).
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	kv := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.IndexByte(line, '=')
		if i <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(line[i+1:])
		val = strings.Trim(val, `"'`)
		kv[key] = val
	}
	return kv, sc.Err()
}

// secretsTemplate returns the shared-secret env map for rendering a new cell:
// from SecretsEnvPath when set, else derived from any existing .env.cellN in
// DeployDir with the per-cell keys stripped. The big secrets never leave the host.
func (a *Agent) secretsTemplate() (map[string]string, error) {
	if a.cfg.SecretsEnvPath != "" && fileExists(a.cfg.SecretsEnvPath) {
		return parseEnvFile(a.cfg.SecretsEnvPath)
	}
	// Fall back to an existing per-cell env (the host already has real secrets in
	// each .env.cellN); take the shared keys, drop the per-cell ones.
	entries, err := os.ReadDir(a.cfg.DeployDir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && envCellRe.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no secrets template: set AGENT_SECRETS_ENV or add one .env.cellN in %s", a.cfg.DeployDir)
	}
	sort.Strings(names)
	kv, err := parseEnvFile(filepath.Join(a.cfg.DeployDir, names[0]))
	if err != nil {
		return nil, err
	}
	for k := range perCellKeys {
		delete(kv, k)
	}
	return kv, nil
}

// writeEnvFile writes KEY=VALUE lines (sorted) with 0600 perms.
func writeEnvFile(path string, kv map[string]string) error {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# rendered by cell-agent — do not edit by hand\n")
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(kv[k])
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// usedPortsAndProjects scans docker + state + .env.cell* files for taken ports
// and project names, so allocation never collides.
func (a *Agent) usedPortsAndProjects(ctx context.Context) (ports map[int]bool, projects map[string]bool) {
	ports = map[int]bool{}
	projects = map[string]bool{}
	for _, p := range a.state.UsedPorts() {
		ports[p] = true
	}
	for _, cs := range a.state.List() {
		projects[cs.Project] = true
	}
	// From running containers.
	for proj, port := range a.containerPorts(ctx) {
		projects[proj] = true
		ports[port] = true
	}
	// From compose projects docker knows (even stopped).
	if statuses, err := a.listProjects(ctx); err == nil {
		for name := range statuses {
			projects[name] = true
		}
	}
	// From on-disk .env.cellN files.
	if entries, err := os.ReadDir(a.cfg.DeployDir); err == nil {
		for _, e := range entries {
			m := envCellRe.FindStringSubmatch(e.Name())
			if m == nil {
				continue
			}
			projects[m[1]] = true
			if kv, err := parseEnvFile(filepath.Join(a.cfg.DeployDir, e.Name())); err == nil {
				if v, ok := kv["CELL_PORT"]; ok {
					if p, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
						ports[p] = true
					}
				}
			}
		}
	}
	return ports, projects
}

// allocate picks the next free cellN project name + a free host port in range.
func (a *Agent) allocate(ctx context.Context) (project string, port int, err error) {
	usedPorts, usedProjects := a.usedPortsAndProjects(ctx)
	for n := 1; n <= 100000; n++ {
		name := fmt.Sprintf("cell%d", n)
		if !usedProjects[name] {
			project = name
			break
		}
	}
	for p := a.cfg.PortRangeStart; p <= a.cfg.PortRangeEnd; p++ {
		if !usedPorts[p] {
			port = p
			break
		}
	}
	if project == "" || port == 0 {
		return "", 0, fmt.Errorf("no free project/port (range %d-%d full)", a.cfg.PortRangeStart, a.cfg.PortRangeEnd)
	}
	return project, port, nil
}
