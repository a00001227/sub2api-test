package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// AgentConfig is the cell-agent's host-local configuration, loaded from env.
// It never receives secrets from the Portal — the big shared secrets
// (POSTGRES_PASSWORD/JWT_SECRET/tokens) live in SecretsEnvPath on the host and
// are merged into a rendered .env.cellN only at cell-create time (phase 2).
type AgentConfig struct {
	// Token is the per-host bearer secret the Portal must present. Required.
	Token string
	// BindAddr is the listen address. Default loopback — the agent must NOT be
	// exposed publicly; the Portal reaches it over the private network.
	BindAddr string
	// PublicIP is this host's address that cells advertise (phase 2). Optional in
	// phase 0/1.
	PublicIP string
	// PortRangeStart/End bound the host ports the agent may allocate to new cells
	// (phase 2). The host firewall/security-group must pre-open this range.
	PortRangeStart int
	PortRangeEnd   int
	// DeployDir holds docker-compose.cell.yml + the per-cell .env files.
	DeployDir string
	// ComposeFile is the cell compose file name (within DeployDir).
	ComposeFile string
	// SecretsEnvPath is the host-only shared-secret template (phase 2 render).
	SecretsEnvPath string
	// StateFile persists agent-created cell metadata (project→port/addr/…).
	StateFile string
	Version   string
}

func LoadConfig() (*AgentConfig, error) {
	c := &AgentConfig{
		Token:          strings.TrimSpace(os.Getenv("AGENT_TOKEN")),
		BindAddr:       envOr("AGENT_BIND_ADDR", "127.0.0.1:9099"),
		PublicIP:       strings.TrimSpace(os.Getenv("AGENT_PUBLIC_IP")),
		PortRangeStart: envIntOr("AGENT_PORT_RANGE_START", 8091),
		PortRangeEnd:   envIntOr("AGENT_PORT_RANGE_END", 8190),
		DeployDir:      envOr("AGENT_DEPLOY_DIR", "."),
		ComposeFile:    envOr("AGENT_COMPOSE_FILE", "docker-compose.cell.yml"),
		SecretsEnvPath: strings.TrimSpace(os.Getenv("AGENT_SECRETS_ENV")),
		Version:        "0.1.0",
	}
	if s := strings.TrimSpace(os.Getenv("AGENT_STATE_FILE")); s != "" {
		c.StateFile = s
	} else {
		c.StateFile = filepath.Join(c.DeployDir, "agent-state.json")
	}

	if c.Token == "" {
		return nil, fmt.Errorf("AGENT_TOKEN is required")
	}
	if len(c.Token) < 16 {
		return nil, fmt.Errorf("AGENT_TOKEN too short (need >= 16 chars)")
	}
	if c.PortRangeStart <= 0 || c.PortRangeEnd < c.PortRangeStart {
		return nil, fmt.Errorf("invalid AGENT_PORT_RANGE_* (start=%d end=%d)", c.PortRangeStart, c.PortRangeEnd)
	}
	return c, nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
