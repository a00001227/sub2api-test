package service

import (
	"bytes"
	"testing"
)

func TestEdgeUsageEnvelope_SSERoundTrip(t *testing.T) {
	src := BuildEdgeUsageEnvelope(&ForwardResult{
		Model:         "claude-sonnet-4-6",
		UpstreamModel: "claude-sonnet-4-6-20260101",
		Stream:        true,
		ImageCount:    0,
		Usage: ClaudeUsage{
			InputTokens:              372,
			OutputTokens:             8,
			CacheCreationInputTokens: 5,
			CacheReadInputTokens:     10,
		},
	})

	sse, err := src.SSEBytes()
	if err != nil {
		t.Fatalf("SSEBytes: %v", err)
	}
	// 必须是可被中央识别的 event 名 + data 行。
	if !bytes.HasPrefix(sse, []byte("event: "+EdgeUsageEventName+"\ndata: ")) {
		t.Fatalf("unexpected SSE framing: %q", sse)
	}
	if !bytes.HasSuffix(sse, []byte("\n\n")) {
		t.Fatalf("SSE event must end with blank line: %q", sse)
	}

	// 取 data 行 JSON 还原,字段必须无损。
	dataStart := bytes.Index(sse, []byte("data: ")) + len("data: ")
	dataEnd := bytes.Index(sse[dataStart:], []byte("\n")) + dataStart
	got, err := ParseEdgeUsageEnvelope(sse[dataStart:dataEnd])
	if err != nil {
		t.Fatalf("ParseEdgeUsageEnvelope: %v", err)
	}
	if got.Model != src.Model || got.UpstreamModel != src.UpstreamModel || got.Stream != src.Stream {
		t.Errorf("scalar mismatch: got %+v want %+v", got, src)
	}
	if got.Usage != src.Usage {
		t.Errorf("usage mismatch: got %+v want %+v", got.Usage, src.Usage)
	}
}

func TestEdgeUsageEnvelope_OpenAIRoundTrip(t *testing.T) {
	tier := "priority"
	src := BuildEdgeUsageEnvelopeOpenAI(&OpenAIForwardResult{
		Model:         "gpt-5-codex",
		UpstreamModel: "gpt-5-codex-20260101",
		Stream:        true,
		ServiceTier:   &tier,
		Usage: OpenAIUsage{
			InputTokens:              300,
			ImageInputTokens:         12,
			OutputTokens:             40,
			CacheCreationInputTokens: 3,
			CacheReadInputTokens:     20,
			ImageOutputTokens:        7,
		},
	})
	if !src.IsOpenAI() {
		t.Fatalf("platform must be openai, got %q", src.Platform)
	}
	// 还原成 OpenAI result:token 桶 + image_input + service_tier 必须无损。
	got := src.ToOpenAIForwardResult()
	if got.Model != "gpt-5-codex" || got.Usage.InputTokens != 300 || got.Usage.ImageInputTokens != 12 ||
		got.Usage.OutputTokens != 40 || got.Usage.CacheReadInputTokens != 20 || got.Usage.ImageOutputTokens != 7 {
		t.Errorf("openai result round-trip mismatch: %+v / usage %+v", got, got.Usage)
	}
	if got.ServiceTier == nil || *got.ServiceTier != "priority" {
		t.Errorf("service_tier lost: %v", got.ServiceTier)
	}
}

func TestEdgeUsageEnvelope_HeaderRoundTrip(t *testing.T) {
	src := BuildEdgeUsageEnvelope(&ForwardResult{
		Model:  "claude-opus-4-8",
		Stream: false,
		Usage:  ClaudeUsage{InputTokens: 100, OutputTokens: 50},
	})
	hv, err := src.HeaderValue()
	if err != nil {
		t.Fatalf("HeaderValue: %v", err)
	}
	got, err := ParseEdgeUsageEnvelope([]byte(hv))
	if err != nil {
		t.Fatalf("parse header: %v", err)
	}
	if got.Model != src.Model || got.Usage != src.Usage || got.Stream {
		t.Errorf("header round-trip mismatch: got %+v want %+v", got, src)
	}
}
