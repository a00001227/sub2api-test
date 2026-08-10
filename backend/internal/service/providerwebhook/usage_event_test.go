package providerwebhook

import (
	"encoding/json"
	"testing"
)

// Phase 21E-6D-6B-2: usage.billable.completed builder. Pure unit tests — no DB.
// Verifies the wire shape matches Portal's UsageEarningService.validatePayload
// and that TOKEN vs IMAGE emit the right fields (and no price/gross/commission).

func TestUsageBillableEventID_Stable(t *testing.T) {
	if got := UsageBillableEventID("client:abc-123"); got != "evt_usage_client:abc-123" {
		t.Fatalf("unexpected event id: %s", got)
	}
	// Same request id → same event id (idempotency anchor).
	if UsageBillableEventID("r1") != UsageBillableEventID("r1") {
		t.Fatal("event id must be deterministic")
	}
}

func TestBuildUsageBillable_Token(t *testing.T) {
	ev := BuildUsageBillable(UsageBillableInput{
		EventID:                   "evt_usage_r1",
		CreatedAt:                 "2026-07-15T00:00:00Z",
		RequestID:                 "r1",
		ExternalProviderAccountID: "pa_1",
		Sub2apiAccountID:          "9001",
		Model:                     "claude-sonnet-5",
		BillingType:               "TOKEN",
		OccurredAt:                "2026-07-14T12:00:00Z",
		InputTokens:               1000,
		OutputTokens:              500,
	})
	if ev.EventID != "evt_usage_r1" {
		t.Fatalf("event id: %s", ev.EventID)
	}
	if ev.Body["event_type"] != "usage.billable.completed" || ev.Body["schema_version"] != 1 {
		t.Fatalf("envelope wrong: %+v", ev.Body)
	}
	p := ev.Body["payload"].(map[string]any)
	for _, k := range []string{"request_id", "external_provider_account_id", "sub2api_account_id", "idempotency_key", "model", "billing_type", "occurred_at", "input_tokens", "output_tokens"} {
		if _, ok := p[k]; !ok {
			t.Fatalf("token payload missing %q", k)
		}
	}
	if p["idempotency_key"] != "evt_usage_r1" {
		t.Fatalf("idempotency_key must equal event_id, got %v", p["idempotency_key"])
	}
	if p["billing_type"] != "TOKEN" || p["input_tokens"].(int) != 1000 || p["output_tokens"].(int) != 500 {
		t.Fatalf("token fields wrong: %+v", p)
	}
	// No image fields, and never price/gross/commission.
	for _, forbidden := range []string{"size_tier", "quantity", "charged_amount_usd_micros", "gross_amount_micros", "commission_bps", "price"} {
		if _, ok := p[forbidden]; ok {
			t.Fatalf("payload must not contain %q", forbidden)
		}
	}
}

func TestBuildUsageBillable_Image(t *testing.T) {
	ev := BuildUsageBillable(UsageBillableInput{
		EventID:                   "evt_usage_r2",
		RequestID:                 "r2",
		ExternalProviderAccountID: "pa_1",
		Sub2apiAccountID:          "9001",
		Model:                     "gpt-image-2",
		BillingType:               "IMAGE",
		OccurredAt:                "2026-07-14T12:00:00Z",
		SizeTier:                  "2K",
		Quantity:                  5,
	})
	p := ev.Body["payload"].(map[string]any)
	if p["billing_type"] != "IMAGE" || p["size_tier"] != "2K" || p["quantity"].(int) != 5 {
		t.Fatalf("image fields wrong: %+v", p)
	}
	// IMAGE omits token fields.
	for _, forbidden := range []string{"input_tokens", "output_tokens", "gross_amount_micros"} {
		if _, ok := p[forbidden]; ok {
			t.Fatalf("image payload must not contain %q", forbidden)
		}
	}
}

// The body must be JSON-serializable (the outbox stores it as JSONB, the
// Sender canonicalizes it).
func TestBuildUsageBillable_JSONSerializable(t *testing.T) {
	ev := BuildUsageBillable(UsageBillableInput{EventID: "e", RequestID: "r", BillingType: "TOKEN"})
	if _, err := json.Marshal(ev.Body); err != nil {
		t.Fatalf("body not serializable: %v", err)
	}
}
