package service

import "testing"

func TestClassifyUpstreamCause(t *testing.T) {
	tests := []struct {
		name   string
		status int
		msg    string
		want   string
	}{
		// 文案命中细类(优先于状态码)
		{"overloaded by msg on 500", 500, "Our servers are currently overloaded", UpstreamCauseOverloaded},
		{"codex model not supported (gpt-5.4)", 400, "The 'gpt-5.4' model is not supported when using Codex with a ChatGPT account.", UpstreamCauseModelNotSupported},
		{"client version gate (fable)", 400, "Claude Code 2.1.161 does not support this model; version 2.1.251 or newer is required. Run 'claude update'", UpstreamCauseClientVersionGate},
		{"version gate precedence over not-supported", 400, "this model does not support this model; version 2.1.251 or newer is required", UpstreamCauseClientVersionGate},

		// 状态码兜底(文案未命中)
		{"transport error → proxy_down", 0, "", UpstreamCauseProxyDown},
		{"negative status → proxy_down", -1, "read: connection reset by peer", UpstreamCauseProxyDown},
		{"529 → overloaded", 529, "", UpstreamCauseOverloaded},
		{"generic 502 → other_5xx", 502, "", UpstreamCauseOther5xx},
		{"generic 503 → other_5xx", 503, "temporarily unavailable", UpstreamCauseOther5xx},

		// 4xx 未命中已知细类 → 不打标
		{"plain 400 no keyword → empty", 400, "bad request", ""},
		{"429 rate limit → empty (已由 mapUpstreamError 区分)", 429, "rate limit exceeded", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyUpstreamCause(tt.status, tt.msg); got != tt.want {
				t.Fatalf("ClassifyUpstreamCause(%d, %q) = %q, want %q", tt.status, tt.msg, got, tt.want)
			}
		})
	}
}

func TestParseEdgeUpstreamCauseHeader(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantStatus int
		wantSlug   string
		wantOK     bool
	}{
		{"valid", "500|overloaded", 500, UpstreamCauseOverloaded, true},
		{"valid 400 gate", "400|client_version_gate", 400, UpstreamCauseClientVersionGate, true},
		{"missing status still ok", "|other_5xx", 0, UpstreamCauseOther5xx, true},
		{"non-numeric status → 0", "abc|proxy_down", 0, UpstreamCauseProxyDown, true},
		{"empty → not ok", "", 0, "", false},
		{"no delimiter → not ok", "overloaded", 0, "", false},
		{"empty slug → not ok", "500|", 0, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, slug, ok := ParseEdgeUpstreamCauseHeader(tt.in)
			if ok != tt.wantOK || slug != tt.wantSlug || status != tt.wantStatus {
				t.Fatalf("ParseEdgeUpstreamCauseHeader(%q) = (%d, %q, %v), want (%d, %q, %v)",
					tt.in, status, slug, ok, tt.wantStatus, tt.wantSlug, tt.wantOK)
			}
		})
	}
}
