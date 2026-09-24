package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCacheControlTTLLayout(t *testing.T) {
	body := []byte(`{"tools":[{"name":"a"},{"name":"b","cache_control":{"type":"ephemeral","ttl":"5m"}}],"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"},{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)
	require.Equal(t, "tools[1]=5m system[0]=- messages[1].0=1h", cacheControlTTLLayout(body))
	require.Equal(t, "", cacheControlTTLLayout(nil))
	require.True(t, isCacheTTLOrderingError(`messages.348.content.0.cache_control.ttl: a ttl='1h' cache_control block must not come after a ttl='5m' cache_control block.`))
	require.False(t, isCacheTTLOrderingError("tool_choice: type tool not supported"))
}
