package service

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

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
		{"request_too_large by msg on 400", 400, "request_too_large: prompt is too long", UpstreamCauseRequestTooLarge},
		{"exceeds maximum size by msg", 400, "input length exceeds the maximum size", UpstreamCauseRequestTooLarge},

		// 状态码兜底(文案未命中)
		{"transport error → proxy_down", 0, "", UpstreamCauseProxyDown},
		{"negative status → proxy_down", -1, "read: connection reset by peer", UpstreamCauseProxyDown},
		{"context canceled → client_canceled", 0, "context canceled", UpstreamCauseClientCanceled},
		{"context cancelled (British) → client_canceled", -1, "Post ...: context cancelled", UpstreamCauseClientCanceled},
		{"deadline exceeded stays proxy_down", 0, "context deadline exceeded", UpstreamCauseProxyDown},
		{"413 → request_too_large", 413, "", UpstreamCauseRequestTooLarge},
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

// 摘要头:编码/解析往返、限长脱敏、从 ops 事件补齐账号信息、没信息时不发头。
func TestEdgeUpstreamDetail_RoundTrip(t *testing.T) {
	in := &EdgeUpstreamDetail{Status: 403, Message: `Overloaded "中文" | x`, Platform: "anthropic",
		AccountID: 9, AccountName: "pa_169da279", RequestID: "req_011", Kind: "stream_error"}
	hv := EncodeEdgeUpstreamDetail(in)
	for _, r := range hv {
		if r > 0x7e || r < 0x21 {
			t.Fatalf("header value must be printable ASCII, got %q", hv)
		}
	}
	out := ParseEdgeUpstreamDetailHeader(hv)
	if out == nil || *out != *in {
		t.Fatalf("round trip mismatch: %+v vs %+v", out, in)
	}
	if ParseEdgeUpstreamDetailHeader("") != nil || ParseEdgeUpstreamDetailHeader("%zz") != nil || ParseEdgeUpstreamDetailHeader("{}") != nil {
		t.Fatal("empty/garbage/blank must parse to nil")
	}
}

func TestSanitizeEdgeDetailText_StripsControlAndTruncates(t *testing.T) {
	got := sanitizeEdgeDetailText("a\r\nb\tc", 10)
	if got != "a  b c" {
		t.Fatalf("control chars should become spaces, got %q", got)
	}
	long := sanitizeEdgeDetailText("0123456789ABCDEF", 10)
	if long != "0123456789…" {
		t.Fatalf("truncate at max runes with ellipsis, got %q", long)
	}
}

func TestBuildEdgeUpstreamDetail_FillsFromLastOpsEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if BuildEdgeUpstreamDetail(c, 0, "") != nil {
		t.Fatal("nothing known → nil")
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{Platform: "anthropic", AccountID: 3, AccountName: "pa_first", UpstreamStatusCode: 429, Message: "first"})
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{Platform: "anthropic", AccountID: 7, AccountName: "pa_last", UpstreamStatusCode: 403, UpstreamRequestID: "req_x", Kind: "stream_error", Message: "Overloaded"})

	d := BuildEdgeUpstreamDetail(c, 0, "")
	if d == nil || d.Status != 403 || d.Message != "Overloaded" || d.AccountName != "pa_last" || d.AccountID != 7 || d.RequestID != "req_x" || d.Kind != "stream_error" {
		t.Fatalf("should fill from last event, got %+v", d)
	}
	// 显式传入的状态码/文案优先,账号仍取自事件。
	d2 := BuildEdgeUpstreamDetail(c, 502, "explicit")
	if d2.Status != 502 || d2.Message != "explicit" || d2.AccountName != "pa_last" {
		t.Fatalf("explicit args must win, got %+v", d2)
	}
}

// SetEdgeUpstreamCauseHeader:无 slug 的 401/403 也要发摘要头;流已开始则两个头都不发。
func TestSetEdgeUpstreamCauseHeader_DetailWithoutSlug(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{AccountName: "pa_x", UpstreamStatusCode: 401, Message: "invalid token"})
	SetEdgeUpstreamCauseHeader(c, false, 401, "invalid token")
	if c.Writer.Header().Get(EdgeUpstreamCauseHeader) != "" {
		t.Fatal("401 has no slug → no cause header")
	}
	d := ParseEdgeUpstreamDetailHeader(c.Writer.Header().Get(EdgeUpstreamDetailHeader))
	if d == nil || d.Status != 401 || d.Message != "invalid token" || d.AccountName != "pa_x" {
		t.Fatalf("detail header expected, got %+v", d)
	}

	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	SetEdgeUpstreamCauseHeader(c2, true, 529, "Overloaded")
	if c2.Writer.Header().Get(EdgeUpstreamCauseHeader) != "" || c2.Writer.Header().Get(EdgeUpstreamDetailHeader) != "" {
		t.Fatal("stream started → no headers")
	}
	if v, _ := c2.Get(OpsUpstreamCauseSlugKey); v != "overloaded" {
		t.Fatalf("slug key must still be set locally, got %v", v)
	}
}
