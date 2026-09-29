package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type memCLIVersionCache struct {
	mu  sync.Mutex
	v   string
	ttl time.Duration
}

func (m *memCLIVersionCache) Get(context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.v, nil
}

func (m *memCLIVersionCache) Set(_ context.Context, v string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.v, m.ttl = v, ttl
	return nil
}

func TestFetchLatestClaudeCLIVersion(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		want    string
		wantErr bool
	}{
		{"npm latest manifest", 200, `{"name":"@anthropic-ai/claude-code","version":"2.1.301","dist":{}}`, "2.1.301", false},
		{"prerelease rejected", 200, `{"version":"2.1.302-beta.1"}`, "", true},
		{"missing version", 200, `{"name":"x"}`, "", true},
		{"non-200", 503, `{}`, "", true},
		{"garbage body", 200, `<html>`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodGet, r.Method)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			got, err := fetchLatestClaudeCLIVersion(context.Background(), srv.Client(), srv.URL)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestClaudeCLIVersionUpdater_RefreshAppliesAndCaches(t *testing.T) {
	t.Cleanup(claude.ResetCurrentCLIVersionForTest)
	claude.ResetCurrentCLIVersionForTest()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"2.1.399"}`))
	}))
	defer srv.Close()

	cache := &memCLIVersionCache{}
	cfg := &config.Config{}
	cfg.Gateway.ClaudeCLIVersion = config.ClaudeCLIVersionConfig{AutoUpdate: true, SourceURL: srv.URL, RefreshInterval: time.Hour}
	u := NewClaudeCLIVersionUpdater(cfg, cache)
	u.client = srv.Client()

	u.refresh(zap.NewNop())
	require.Equal(t, "2.1.399", claude.CurrentCLIVersion())
	require.Equal(t, "2.1.399", cache.v)
	require.Equal(t, claudeCLIVersionCacheTTL, cache.ttl)
	require.Contains(t, claude.DefaultUserAgent(), "claude-cli/2.1.399")
}

func TestClaudeCLIVersionUpdater_LowerThanConstantNotApplied(t *testing.T) {
	t.Cleanup(claude.ResetCurrentCLIVersionForTest)
	claude.ResetCurrentCLIVersionForTest()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"1.0.0"}`))
	}))
	defer srv.Close()
	cache := &memCLIVersionCache{}
	cfg := &config.Config{}
	cfg.Gateway.ClaudeCLIVersion = config.ClaudeCLIVersionConfig{AutoUpdate: true, SourceURL: srv.URL, RefreshInterval: time.Hour}
	u := NewClaudeCLIVersionUpdater(cfg, cache)
	u.client = srv.Client()

	u.refresh(zap.NewNop())
	require.Equal(t, claude.CLICurrentVersion, claude.CurrentCLIVersion(), "常量是下限")
	require.Empty(t, cache.v, "未采纳的值不写缓存")
}

func TestClaudeCLIVersionUpdater_StartAppliesCacheAndStops(t *testing.T) {
	t.Cleanup(claude.ResetCurrentCLIVersionForTest)
	claude.ResetCurrentCLIVersionForTest()

	// 拉取端点故意 500:只靠缓存兜底
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	cache := &memCLIVersionCache{v: "2.1.350"}
	cfg := &config.Config{}
	cfg.Gateway.ClaudeCLIVersion = config.ClaudeCLIVersionConfig{AutoUpdate: true, SourceURL: srv.URL, RefreshInterval: time.Hour}
	u := NewClaudeCLIVersionUpdater(cfg, cache)
	u.client = srv.Client()

	u.Start()
	u.Start() // 幂等
	require.Equal(t, "2.1.350", claude.CurrentCLIVersion(), "启动先用缓存兜底")
	u.Stop()
	u.Stop() // 幂等
}

func TestClaudeCLIVersionUpdater_Disabled(t *testing.T) {
	t.Cleanup(claude.ResetCurrentCLIVersionForTest)
	claude.ResetCurrentCLIVersionForTest()
	cache := &memCLIVersionCache{v: "2.1.350"}
	cfg := &config.Config{}
	cfg.Gateway.ClaudeCLIVersion = config.ClaudeCLIVersionConfig{AutoUpdate: false}
	u := NewClaudeCLIVersionUpdater(cfg, cache)
	u.Start()
	require.Equal(t, claude.CLICurrentVersion, claude.CurrentCLIVersion(), "关闭时连缓存也不用")
	u.Stop()
}
