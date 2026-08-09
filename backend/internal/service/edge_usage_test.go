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
