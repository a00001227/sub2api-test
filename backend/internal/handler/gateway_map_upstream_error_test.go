package handler

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// Claude 路径与 OpenAI 路径同口径:429/529/5xx/未知状态码把上游真实原因拼给客户端;401/403 不下发。
func TestGatewayMapUpstreamError_CarriesUpstreamReason(t *testing.T) {
	h := &GatewayHandler{}
	st, typ, msg := h.mapUpstreamError(522, "origin timed out")
	require.Equal(t, http.StatusBadGateway, st)
	require.Equal(t, "upstream_error", typ)
	require.Equal(t, "Upstream request failed (upstream 522) — origin timed out", msg)

	st, typ, msg = h.mapUpstreamError(429, "This request would exceed your rate limit")
	require.Equal(t, http.StatusTooManyRequests, st)
	require.Equal(t, "rate_limit_error", typ)
	require.Equal(t, "Upstream rate limit exceeded, please retry later — This request would exceed your rate limit", msg)

	_, _, msg = h.mapUpstreamError(503, "")
	require.Equal(t, "Upstream service temporarily unavailable (upstream 503)", msg)

	_, _, msg = h.mapUpstreamError(401, "invalid x-api-key sk-ant-secret")
	require.Equal(t, "Upstream authentication failed, please contact administrator", msg, "401 不下发上游细节")
}
