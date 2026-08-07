package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// CELL_UPSTREAM_PROXY:EDGE cell 统一上游出口。
func TestEffectiveUpstreamProxy(t *testing.T) {
	const override = "socks5://user:pass@10.0.0.9:1080"

	cases := []struct {
		name     string
		cfg      *config.Config
		proxyURL string
		want     string
	}{
		{"edge + override 覆盖按账号代理", &config.Config{EdgeMode: true, EdgeUpstreamProxy: override}, "http://per-account:3128", override},
		{"edge + override 覆盖直连", &config.Config{EdgeMode: true, EdgeUpstreamProxy: override}, "", override},
		{"edge 但未配 → 用传入(直连)", &config.Config{EdgeMode: true, EdgeUpstreamProxy: ""}, "", ""},
		{"edge 但未配 → 用传入(按账号)", &config.Config{EdgeMode: true, EdgeUpstreamProxy: "  "}, "http://per-account:3128", "http://per-account:3128"},
		{"中央模式忽略 override", &config.Config{EdgeMode: false, EdgeUpstreamProxy: override}, "http://per-account:3128", "http://per-account:3128"},
		{"nil cfg → 用传入", nil, "http://per-account:3128", "http://per-account:3128"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := effectiveUpstreamProxy(c.cfg, c.proxyURL); got != c.want {
				t.Fatalf("effectiveUpstreamProxy = %q, want %q", got, c.want)
			}
		})
	}
}
