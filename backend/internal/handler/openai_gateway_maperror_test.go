package handler

import (
	"net/http"
	"strings"
	"testing"
)

// TestMapUpstreamError_OverloadMirrorsClaude 校验:OpenAI 过载(5xx + "overloaded" body)
// 与 Anthropic 过载(529)都被归成与 Claude 一致的 overloaded_error/503,并把真实原因带回客户端。
func TestMapUpstreamError_OverloadMirrorsClaude(t *testing.T) {
	h := &OpenAIGatewayHandler{}

	// OpenAI 过载:HTTP 502 + "Our servers are currently overloaded" → overloaded_error/503。
	status, errType, msg := h.mapUpstreamError(502, "Our servers are currently overloaded, please try again later.")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("过载应回 503; got %d", status)
	}
	if errType != "overloaded_error" {
		t.Fatalf("过载应回 overloaded_error(同 Claude); got %q", errType)
	}
	if !strings.Contains(msg, "overloaded") {
		t.Fatalf("客户端消息应含真实原因; got %q", msg)
	}

	// Anthropic 语义过载:529(即使无 body)→ overloaded_error/503。
	status, errType, _ = h.mapUpstreamError(529, "")
	if status != http.StatusServiceUnavailable || errType != "overloaded_error" {
		t.Fatalf("529 应回 503/overloaded_error; got %d/%q", status, errType)
	}
}

// TestMapUpstreamError_SurfacesRealReason 校验:普通 5xx / 未知错误把上游真实原因拼回客户端,
// 不再是笼统的 "temporarily unavailable" / "Upstream request failed"。
func TestMapUpstreamError_SurfacesRealReason(t *testing.T) {
	h := &OpenAIGatewayHandler{}

	_, errType, msg := h.mapUpstreamError(502, "Invalid 'input[52].id': expected an ID that begins with 'msg'")
	if errType != "upstream_error" {
		t.Fatalf("普通 5xx 应保持 upstream_error; got %q", errType)
	}
	if !strings.Contains(msg, "input[52].id") {
		t.Fatalf("应把真实原因回给客户端; got %q", msg)
	}

	// 无上游文案 → 只回兜底话术(不拼空原因)。
	_, _, msg = h.mapUpstreamError(500, "")
	if strings.Contains(msg, "—") {
		t.Fatalf("无原因时不应带分隔符; got %q", msg)
	}

	// 429 → rate_limit_error + 真实原因。
	_, errType, msg = h.mapUpstreamError(429, "rate limit reached for gpt-5")
	if errType != "rate_limit_error" || !strings.Contains(msg, "rate limit reached") {
		t.Fatalf("429 应回 rate_limit_error + 原因; got %q / %q", errType, msg)
	}
}

// TestMapUpstreamError_AuthNoReason 校验:401/403 认证类不回上游文案(可能含敏感提示)。
func TestMapUpstreamError_AuthNoReason(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	for _, code := range []int{401, 403} {
		_, _, msg := h.mapUpstreamError(code, "secret hint token=abc")
		if strings.Contains(msg, "secret hint") || strings.Contains(msg, "token=") {
			t.Fatalf("认证类错误不应下发上游文案; code=%d msg=%q", code, msg)
		}
	}
}
