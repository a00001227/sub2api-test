package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestIsThinkingDisabledUnsupportedError(t *testing.T) {
	require.True(t, isThinkingDisabledUnsupportedError(`"thinking.type.disabled" is not supported for this model. Use "thinking.type.adaptive" and "output_config.effort"`))
	require.False(t, isThinkingDisabledUnsupportedError(`messages.3.content.0: Invalid `+"`signature`"+` in `+"`thinking`"+` block`))
	require.False(t, isThinkingDisabledUnsupportedError(`thinking.budget_tokens: Input should be greater than or equal to 1024`))
	require.False(t, isThinkingDisabledUnsupportedError(""))
}

// disabled → 整体替换成 {type:adaptive}(budget_tokens 一并丢掉),缺 effort 时补 low;其它字段原样。
func TestRewriteThinkingDisabledToAdaptive(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","max_tokens":1024,"thinking":{"type":"disabled","budget_tokens":0},"messages":[{"role":"user","content":"hi"}]}`)
	out, applied := RewriteThinkingDisabledToAdaptive(body)
	require.True(t, applied)
	require.Equal(t, "adaptive", gjson.GetBytes(out, "thinking.type").String())
	require.False(t, gjson.GetBytes(out, "thinking.budget_tokens").Exists())
	require.Equal(t, "low", gjson.GetBytes(out, "output_config.effort").String())
	require.Equal(t, "claude-opus-4-8", gjson.GetBytes(out, "model").String())
	require.Equal(t, "hi", gjson.GetBytes(out, "messages.0.content").String())

	// 客户端已带 effort → 尊重,不覆盖
	body = []byte(`{"thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`)
	out, applied = RewriteThinkingDisabledToAdaptive(body)
	require.True(t, applied)
	require.Equal(t, "high", gjson.GetBytes(out, "output_config.effort").String())

	// 非 disabled(enabled / adaptive / 缺省)→ 不动
	for _, raw := range []string{
		`{"thinking":{"type":"enabled","budget_tokens":2048}}`,
		`{"thinking":{"type":"adaptive"}}`,
		`{"messages":[]}`,
	} {
		out, applied := RewriteThinkingDisabledToAdaptive([]byte(raw))
		require.False(t, applied, raw)
		require.Equal(t, raw, string(out))
	}
}

func TestThinkingDisabledUnsupportedMemo(t *testing.T) {
	const model = "claude-test-memo-model-xyz"
	require.False(t, modelRejectsThinkingDisabled(model))
	markThinkingDisabledUnsupported("  Claude-Test-Memo-Model-XYZ ")
	require.True(t, modelRejectsThinkingDisabled(model)) // 大小写 / 空白不敏感
	require.False(t, modelRejectsThinkingDisabled(""))
	markThinkingDisabledUnsupported("")
	require.False(t, modelRejectsThinkingDisabled(""))
}
