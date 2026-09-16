package repository

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 平台专属槽位键带平台后缀；空平台保持旧键，且都落在启动清理的通配前缀内。
func TestUserSlotKeys_PlatformSuffix(t *testing.T) {
	require.Equal(t, "concurrency:user:7", userSlotKey(7, ""))
	require.Equal(t, "concurrency:user:7:openai", userSlotKey(7, "openai"))
	require.Equal(t, "concurrency:wait:7", waitQueueKey(7, ""))
	require.Equal(t, "concurrency:wait:7:anthropic", waitQueueKey(7, "anthropic"))
}
