package repository

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// anthropic / openai 接入默认:拦截预热开 + 三条临时不可调度规则;且经 JSON 往返
// (模拟 ent 落库→读回:int→float64、[]string→[]any)后仍能被 getter 正确解析。
func TestDefaultProviderAccountConfig_Anthropic(t *testing.T) {
	testDefaultsApplied(t, service.PlatformAnthropic)
}

func TestDefaultProviderAccountConfig_OpenAI(t *testing.T) {
	testDefaultsApplied(t, service.PlatformOpenAI)
}

func testDefaultsApplied(t *testing.T, platform string) {
	t.Helper()
	extra, creds := defaultProviderAccountConfig(platform)
	require.Empty(t, extra, "默认不写 extra(TLS/掩码刻意不设)")
	require.Equal(t, true, creds["intercept_warmup_requests"])
	require.Equal(t, true, creds["temp_unschedulable_enabled"])

	// JSON 往返,复刻生产序列化路径。
	raw, err := json.Marshal(creds)
	require.NoError(t, err)
	var rt map[string]any
	require.NoError(t, json.Unmarshal(raw, &rt))
	acc := &service.Account{Credentials: rt}

	require.True(t, acc.IsInterceptWarmupEnabled())
	require.True(t, acc.IsTempUnschedulableEnabled())

	rules := acc.GetTempUnschedulableRules()
	require.Len(t, rules, 3, "529/429/503 三条规则应全部通过 getter 校验(code>0 且 duration>0 且 keywords 非空)")
	byCode := map[int]service.TempUnschedulableRule{}
	for _, r := range rules {
		byCode[r.ErrorCode] = r
		require.NotEmpty(t, r.Keywords)
	}
	// 关键词与时长沿用 sub2api admin 原预设(注意 429 是带空格的 "rate limit")。
	require.Contains(t, byCode[529].Keywords, "overloaded")
	require.Contains(t, byCode[429].Keywords, "rate limit")
	require.Contains(t, byCode[503].Keywords, "unavailable")
	require.Equal(t, 60, byCode[529].DurationMinutes)
	require.Equal(t, 10, byCode[429].DurationMinutes)
	require.Equal(t, 30, byCode[503].DurationMinutes)
}

// gemini / antigravity 暂不预置(交给编辑器按需配)。
func TestDefaultProviderAccountConfig_Unsupported(t *testing.T) {
	for _, p := range []string{service.PlatformGemini, "antigravity"} {
		extra, creds := defaultProviderAccountConfig(p)
		require.Empty(t, extra, p)
		require.Empty(t, creds, p)
	}
}
