package service

import (
	"bytes"
	"testing"
	"time"
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

// 时延必须经边信道无损带回中央,否则中央使用记录「首 TOKEN / 耗时」恒为空。
func TestEdgeUsageEnvelope_TimingRoundTrip(t *testing.T) {
	ftt := 137
	// claude 流式路径:SSE 往返后还原 ForwardResult。
	src := BuildEdgeUsageEnvelope(&ForwardResult{
		Model:        "claude-sonnet-4-6",
		Stream:       true,
		FirstTokenMs: &ftt,
		Duration:     2500 * time.Millisecond,
		Usage:        ClaudeUsage{InputTokens: 10, OutputTokens: 5},
	})
	if src.FirstTokenMs == nil || *src.FirstTokenMs != 137 || src.DurationMs != 2500 {
		t.Fatalf("envelope did not carry timing: first=%v dur=%d", src.FirstTokenMs, src.DurationMs)
	}
	sse, err := src.SSEBytes()
	if err != nil {
		t.Fatalf("SSEBytes: %v", err)
	}
	dataStart := bytes.Index(sse, []byte("data: ")) + len("data: ")
	dataEnd := bytes.Index(sse[dataStart:], []byte("\n")) + dataStart
	env, err := ParseEdgeUsageEnvelope(sse[dataStart:dataEnd])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res := env.ToForwardResult()
	if res.FirstTokenMs == nil || *res.FirstTokenMs != 137 {
		t.Errorf("FirstTokenMs lost: %v", res.FirstTokenMs)
	}
	if res.Duration != 2500*time.Millisecond {
		t.Errorf("Duration lost: got %v want 2.5s", res.Duration)
	}

	// OpenAI 路径同样无损。
	oai := BuildEdgeUsageEnvelopeOpenAI(&OpenAIForwardResult{
		Model:        "gpt-5-codex",
		Stream:       true,
		FirstTokenMs: &ftt,
		Duration:     900 * time.Millisecond,
		Usage:        OpenAIUsage{InputTokens: 1, OutputTokens: 1},
	})
	ores := oai.ToOpenAIForwardResult()
	if ores.FirstTokenMs == nil || *ores.FirstTokenMs != 137 || ores.Duration != 900*time.Millisecond {
		t.Errorf("openai timing lost: first=%v dur=%v", ores.FirstTokenMs, ores.Duration)
	}

	// 非流式(FirstTokenMs=nil)时 omitempty 生效,还原后仍为 nil,Duration 保留。
	nonStream := BuildEdgeUsageEnvelope(&ForwardResult{
		Model:    "claude-opus-4-8",
		Duration: 400 * time.Millisecond,
		Usage:    ClaudeUsage{InputTokens: 2, OutputTokens: 2},
	})
	nres := nonStream.ToForwardResult()
	if nres.FirstTokenMs != nil {
		t.Errorf("non-stream FirstTokenMs should be nil, got %v", *nres.FirstTokenMs)
	}
	if nres.Duration != 400*time.Millisecond {
		t.Errorf("non-stream Duration lost: %v", nres.Duration)
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
