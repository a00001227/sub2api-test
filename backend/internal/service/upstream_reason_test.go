package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWithUpstreamReason(t *testing.T) {
	require.Equal(t, "Upstream request failed", WithUpstreamReason("Upstream request failed", 0, ""))
	require.Equal(t, "Upstream request failed (upstream 522)", WithUpstreamReason("Upstream request failed", 522, "  "))
	require.Equal(t, "Upstream request failed (upstream 422) — Invalid schema", WithUpstreamReason("Upstream request failed", 422, "Invalid schema"))
	require.Equal(t, "Upstream rate limit exceeded — rate_limit_error: too many", WithUpstreamReason("Upstream rate limit exceeded", 0, "rate_limit_error: too many"))
	long := strings.Repeat("x", 400)
	out := WithUpstreamReason("F", 0, long)
	require.True(t, strings.HasSuffix(out, "…"))
	require.LessOrEqual(t, len([]rune(out)), len([]rune("F — "))+upstreamReasonMaxRunes+1)
}
