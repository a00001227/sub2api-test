package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// These tests pin the max_tokens/budget_tokens clamp in ResponsesToAnthropicRequest.
// Without it, an effort-derived thinking budget is injected independently of the
// client's max_tokens, so a small (or defaulted) max_tokens collides with a larger
// budget and Anthropic rejects the request with 400
// "max_tokens must be greater than thinking.budget_tokens" (ops error #65053).

func intPtr(v int) *int { return &v }

// effort=high with no client max_output_tokens: max_tokens defaults to 8192, which
// is smaller than the injected high budget (10240) — the exact default self-collision.
// The clamp must raise max_tokens to budget + headroom.
func TestResponsesToAnthropic_ClampHighEffort_DefaultMaxTokens(t *testing.T) {
	out, err := ResponsesToAnthropicRequest(&ResponsesRequest{
		Model:     "claude-sonnet-4",
		Input:     json.RawMessage(`[{"role":"user","content":"Hello"}]`),
		Reasoning: &ResponsesReasoning{Effort: "high"},
	})
	require.NoError(t, err)
	require.NotNil(t, out.Thinking)
	require.Equal(t, 10240, out.Thinking.BudgetTokens)
	require.Equal(t, 10240+thinkingOutputHeadroomTokens, out.MaxTokens)
	require.Greater(t, out.MaxTokens, out.Thinking.BudgetTokens)
}

// effort=max (mapped from xhigh) with a tiny client max_output_tokens (128, the CC
// floor). Budget is 32768; the clamp must lift max_tokens above it.
func TestResponsesToAnthropic_ClampMaxEffort_TinyClientMax(t *testing.T) {
	out, err := ResponsesToAnthropicRequest(&ResponsesRequest{
		Model:           "claude-opus-4",
		Input:           json.RawMessage(`[{"role":"user","content":"Hello"}]`),
		MaxOutputTokens: intPtr(128),
		Reasoning:       &ResponsesReasoning{Effort: "xhigh"},
	})
	require.NoError(t, err)
	require.NotNil(t, out.Thinking)
	require.Equal(t, 32768, out.Thinking.BudgetTokens)
	require.Equal(t, 32768+thinkingOutputHeadroomTokens, out.MaxTokens)
	require.Greater(t, out.MaxTokens, out.Thinking.BudgetTokens)
}

// A client that already asked for more than budget + headroom keeps its value —
// the clamp only lifts, never lowers, so client intent is preserved.
func TestResponsesToAnthropic_ClampNoop_WhenClientMaxAlreadyLarge(t *testing.T) {
	out, err := ResponsesToAnthropicRequest(&ResponsesRequest{
		Model:           "claude-sonnet-4",
		Input:           json.RawMessage(`[{"role":"user","content":"Hello"}]`),
		MaxOutputTokens: intPtr(50000),
		Reasoning:       &ResponsesReasoning{Effort: "high"},
	})
	require.NoError(t, err)
	require.NotNil(t, out.Thinking)
	require.Equal(t, 50000, out.MaxTokens)
}

// effort=low disables thinking; the clamp block is skipped and max_tokens is
// left at the client value.
func TestResponsesToAnthropic_LowEffort_NoThinkingNoClamp(t *testing.T) {
	out, err := ResponsesToAnthropicRequest(&ResponsesRequest{
		Model:           "claude-sonnet-4",
		Input:           json.RawMessage(`[{"role":"user","content":"Hello"}]`),
		MaxOutputTokens: intPtr(512),
		Reasoning:       &ResponsesReasoning{Effort: "low"},
	})
	require.NoError(t, err)
	require.Nil(t, out.Thinking)
	require.Equal(t, 512, out.MaxTokens)
}

// No reasoning at all: no thinking injected, max_tokens defaults untouched.
func TestResponsesToAnthropic_NoReasoning_DefaultMaxUntouched(t *testing.T) {
	out, err := ResponsesToAnthropicRequest(&ResponsesRequest{
		Model: "claude-sonnet-4",
		Input: json.RawMessage(`[{"role":"user","content":"Hello"}]`),
	})
	require.NoError(t, err)
	require.Nil(t, out.Thinking)
	require.Equal(t, 8192, out.MaxTokens)
}

// Full production path for a Chat Completions client on an Anthropic group:
// ChatCompletionsToResponses → ResponsesToAnthropicRequest. Reproduces #65053
// (reasoning_effort=high + small max_tokens) and asserts the result is valid.
func TestCCChain_HighEffortSmallMaxTokens_NoBudgetCollision(t *testing.T) {
	respReq, err := ChatCompletionsToResponses(&ChatCompletionsRequest{
		Model:           "claude-sonnet-4",
		MaxTokens:       intPtr(512),
		ReasoningEffort: "high",
	})
	require.NoError(t, err)
	out, err := ResponsesToAnthropicRequest(respReq)
	require.NoError(t, err)
	require.NotNil(t, out.Thinking)
	require.Greater(t, out.MaxTokens, out.Thinking.BudgetTokens)
}
