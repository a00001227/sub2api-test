package repository

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Phase 21E-6D-6B-2: pure unit tests for the outbox payload mapping (no DB).

func TestBillingTypeForOutbox(t *testing.T) {
	cases := map[string]string{
		"image":       "IMAGE",
		"IMAGE":       "IMAGE",
		" Image ":     "IMAGE",
		"token":       "TOKEN",
		"per_request": "TOKEN",
		"":            "TOKEN",
	}
	for in, want := range cases {
		if got := billingTypeForOutbox(in); got != want {
			t.Fatalf("billingTypeForOutbox(%q)=%q want %q", in, got, want)
		}
	}
}

func TestBuildUsageOutboxPayload_Token(t *testing.T) {
	cmd := &service.UsageBillingCommand{
		RequestID:       "client:r1",
		AccountID:       9001,
		Model:           "claude-sonnet-5",
		BillingMode:     "token",
		InputTokens:     1000,
		OutputTokens:    500,
		UsageOccurredAt: time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC),
	}
	p := buildUsageOutboxPayload(cmd, "evt_usage_client:r1", "pa_1")
	if p["billing_type"] != "TOKEN" {
		t.Fatalf("billing_type: %v", p["billing_type"])
	}
	if p["external_provider_account_id"] != "pa_1" || p["sub2api_account_id"] != "9001" {
		t.Fatalf("attribution wrong: %+v", p)
	}
	if p["idempotency_key"] != "evt_usage_client:r1" {
		t.Fatalf("idempotency_key: %v", p["idempotency_key"])
	}
	if p["input_tokens"].(int) != 1000 || p["output_tokens"].(int) != 500 {
		t.Fatalf("tokens: %+v", p)
	}
	if p["occurred_at"] != "2026-07-14T12:00:00Z" {
		t.Fatalf("occurred_at: %v", p["occurred_at"])
	}
	// never price/gross
	for _, forbidden := range []string{"charged_amount_usd_micros", "gross_amount_micros", "size_tier"} {
		if _, ok := p[forbidden]; ok {
			t.Fatalf("must not contain %q", forbidden)
		}
	}
}

func TestBuildUsageOutboxPayload_Image(t *testing.T) {
	cmd := &service.UsageBillingCommand{
		RequestID:       "client:r2",
		AccountID:       9002,
		Model:           "gpt-image-2",
		BillingMode:     "image",
		ImageSizeTier:   "2K",
		ImageCount:      5,
		UsageOccurredAt: time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC),
	}
	p := buildUsageOutboxPayload(cmd, "evt_usage_client:r2", "pa_2")
	if p["billing_type"] != "IMAGE" || p["size_tier"] != "2K" || p["quantity"].(int) != 5 {
		t.Fatalf("image payload wrong: %+v", p)
	}
	for _, forbidden := range []string{"input_tokens", "output_tokens"} {
		if _, ok := p[forbidden]; ok {
			t.Fatalf("image payload must not contain %q", forbidden)
		}
	}
}
