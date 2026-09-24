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

// thinking disabled → adaptive 时,强制工具 tool_choice(any/tool)必须同步降成 auto,否则第二次
// 请求撞 "tool_choice: type tool and any are not supported for this model"。
func TestRewriteThinkingDisabledToAdaptive_DowngradesForcedToolChoice(t *testing.T) {
	body := []byte(`{"model":"m","thinking":{"type":"disabled"},"tool_choice":{"type":"tool","name":"get_weather","disable_parallel_tool_use":true},"messages":[]}`)
	out, changed := RewriteThinkingDisabledToAdaptive(body)
	require.True(t, changed)
	require.Equal(t, "adaptive", gjson.GetBytes(out, "thinking.type").String())
	require.Equal(t, "auto", gjson.GetBytes(out, "tool_choice.type").String())
	require.False(t, gjson.GetBytes(out, "tool_choice.name").Exists(), "auto 不带 name")
	require.True(t, gjson.GetBytes(out, "tool_choice.disable_parallel_tool_use").Bool(), "其它字段保留")

	body = []byte(`{"model":"m","thinking":{"type":"disabled"},"tool_choice":{"type":"any"}}`)
	out, _ = RewriteThinkingDisabledToAdaptive(body)
	require.Equal(t, "auto", gjson.GetBytes(out, "tool_choice.type").String())

	// tool_choice auto / 缺省:不动。
	body = []byte(`{"model":"m","thinking":{"type":"disabled"},"tool_choice":{"type":"auto"}}`)
	out, _ = RewriteThinkingDisabledToAdaptive(body)
	require.Equal(t, "auto", gjson.GetBytes(out, "tool_choice.type").String())

	// 客户自己就开着 thinking + 强制工具:不是我们改写的,不兜底。
	body = []byte(`{"model":"m","thinking":{"type":"enabled","budget_tokens":1024},"tool_choice":{"type":"any"}}`)
	out, changed = RewriteThinkingDisabledToAdaptive(body)
	require.False(t, changed)
	require.Equal(t, "any", gjson.GetBytes(out, "tool_choice.type").String())
}
