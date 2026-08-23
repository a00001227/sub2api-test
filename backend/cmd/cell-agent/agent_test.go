package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidProject(t *testing.T) {
	ok := []string{"cell1", "cell7", "cell15", "cell100"}
	bad := []string{"cell", "cellN", "postgres", "cell1;rm", "sub2api-central", "", "Cell1", " cell1"}
	for _, p := range ok {
		if !validProject(p) {
			t.Fatalf("expected %q valid", p)
		}
	}
	for _, p := range bad {
		if validProject(p) {
			t.Fatalf("expected %q invalid", p)
		}
	}
}

func TestLoadConfigRequiresToken(t *testing.T) {
	t.Setenv("AGENT_TOKEN", "")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected error when AGENT_TOKEN empty")
	}
	t.Setenv("AGENT_TOKEN", "short")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected error when AGENT_TOKEN too short")
	}
	t.Setenv("AGENT_TOKEN", "0123456789abcdef0123456789abcdef")
	c, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if c.PortRangeStart != 8091 || c.PortRangeEnd != 8190 {
		t.Fatalf("default port range wrong: %d-%d", c.PortRangeStart, c.PortRangeEnd)
	}
}

func TestAuthMiddleware(t *testing.T) {
	a := &Agent{cfg: &AgentConfig{Token: "0123456789abcdef0123456789abcdef"}}
	h := a.auth(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer nope", http.StatusUnauthorized},
		{"no prefix", "0123456789abcdef0123456789abcdef", http.StatusUnauthorized},
		{"correct", "Bearer 0123456789abcdef0123456789abcdef", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/agent/v1/health", nil)
			if c.header != "" {
				r.Header.Set("Authorization", c.header)
			}
			w := httptest.NewRecorder()
			h(w, r)
			if w.Code != c.want {
				t.Fatalf("got %d want %d", w.Code, c.want)
			}
		})
	}
}
