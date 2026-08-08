package handler

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// #95: "系统自动识别" —— 粘整条回调 URL 抽 code+state;粘裸 code 原样;显式 state 优先。
func TestExtractConnectCodeState(t *testing.T) {
	// 整条回调 URL → 解析 code + state
	code, state := extractConnectCodeState("http://localhost:1455/auth/callback?code=abc123&state=xyz789", "")
	require.Equal(t, "abc123", code)
	require.Equal(t, "xyz789", state)

	// 裸 code + 显式 state
	code, state = extractConnectCodeState("rawcode", "explicitstate")
	require.Equal(t, "rawcode", code)
	require.Equal(t, "explicitstate", state)

	// 显式 state 优先于 URL 里的 state
	code, state = extractConnectCodeState("https://x/cb?code=a&state=fromurl", "fromfield")
	require.Equal(t, "a", code)
	require.Equal(t, "fromfield", state)

	// 前后空白清理
	code, state = extractConnectCodeState("  plaincode  ", "  st  ")
	require.Equal(t, "plaincode", code)
	require.Equal(t, "st", state)
}
