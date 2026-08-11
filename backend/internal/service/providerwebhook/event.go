package providerwebhook

import "fmt"

// Phase 21E-6C-2D-1: event builders. event_id is fixed at build time and
// reused across retries. Bodies match Portal's processor field names
// (schema_version=1, event_type + payload{external_provider_account_id,...}).

// ActivatedInput carries the data for provider.account.activated.
type ActivatedInput struct {
	EventID                   string // stable id (e.g. evt_connect_<sessionID>)
	CreatedAt                 string // RFC3339
	ExternalProviderAccountID string
	Sub2apiAccountID          string
	ProviderType              string
	Platform                  string
	Region                    string
	// Email: the upstream AI account's email address (Phase 21E-6E email-name).
	// Non-credential identifier; used by Portal as the account display name.
	// Optional — empty when unknown (Portal falls back to a sequential name).
	Email string
	// Plan: upstream subscription tier (e.g. "default_claude_max_20x"),
	// snapshotted by the Portal at activation. Optional — empty when unknown.
	Plan string
}

// BuildActivated constructs a provider.account.activated event.
func BuildActivated(in ActivatedInput) Event {
	payload := map[string]any{
		"external_provider_account_id": in.ExternalProviderAccountID,
		"sub2api_account_id":           in.Sub2apiAccountID,
		"status":                       "active",
		"provider_type":                in.ProviderType,
		"platform":                     in.Platform,
	}
	if in.Region != "" {
		payload["region"] = in.Region
	}
	if in.Email != "" {
		payload["email"] = in.Email
	}
	if in.Plan != "" {
		payload["plan"] = in.Plan
	}
	return Event{
		EventID: in.EventID,
		Body: map[string]any{
			"schema_version": 1,
			"event_id":       in.EventID,
			"event_type":     "provider.account.activated",
			"created_at":     in.CreatedAt,
			"payload":        payload,
		},
	}
}

// StatusChangedInput carries the data for provider.account.status.changed.
type StatusChangedInput struct {
	EventID                   string
	CreatedAt                 string
	ExternalProviderAccountID string
	PreviousStatus            string
	Status                    string
}

// BuildStatusChanged constructs a provider.account.status.changed event.
func BuildStatusChanged(in StatusChangedInput) Event {
	return Event{
		EventID: in.EventID,
		Body: map[string]any{
			"schema_version": 1,
			"event_id":       in.EventID,
			"event_type":     "provider.account.status.changed",
			"created_at":     in.CreatedAt,
			"payload": map[string]any{
				"external_provider_account_id": in.ExternalProviderAccountID,
				"previous_status":              in.PreviousStatus,
				"status":                       in.Status,
			},
		},
	}
}

// StatusChangedEventID derives a UNIQUE-per-occurrence event id for a status
// reflow. Unlike activation/usage (STABLE ids meant to dedup), each status
// transition is a distinct fact — a flapping account (invalid→active→invalid)
// must deliver every hop, so the id embeds a caller-supplied nonce (unix nanos)
// that stays fixed across that one occurrence's delivery retries but differs
// between occurrences. The external ref is Portal-owned and non-sensitive.
func StatusChangedEventID(externalRef, status string, nonce int64) string {
	return fmt.Sprintf("evt_status_%s_%s_%d", externalRef, status, nonce)
}

// ConnectActivatedEventID derives a STABLE event id from the connect
// session id: the same completed session always yields the same evt id, so
// a retry (or a duplicate completion) reuses it and Portal's inbox
// external_event_id idempotency collapses duplicates.
func ConnectActivatedEventID(sessionID int64) string {
	return fmt.Sprintf("evt_connect_%d", sessionID)
}

// ImportActivatedEventID derives a STABLE event id from the external provider
// account id (Phase 21E-6E-4). Credential import has no connect session, so
// the stable key is the Portal-owned pa_<uuid> reference — a retry of the same
// import reuses the same evt id and Portal's inbox idempotency collapses it.
// The value is non-sensitive (Portal owns and already knows this id).
func ImportActivatedEventID(externalProviderAccountID string) string {
	return "evt_import_" + externalProviderAccountID
}

// Phase 21E-6D-6B-2: usage.billable.completed. Sub2API sends a usage FACT
// only — no price / gross / commission (Portal prices and splits). Payload
// field names match Portal's UsageEarningService.validatePayload exactly.

// UsageBillableInput carries the data for one billable usage event. TOKEN
// billing uses input/output tokens; IMAGE billing uses size_tier + quantity.
type UsageBillableInput struct {
	EventID                   string // stable: evt_usage_<request_id>
	CreatedAt                 string // RFC3339 (emit time)
	RequestID                 string
	ExternalProviderAccountID string
	Sub2apiAccountID          string
	Model                     string
	BillingType               string // "TOKEN" | "IMAGE"
	OccurredAt                string // RFC3339 (usage_logs.created_at)
	// TOKEN
	InputTokens  int
	OutputTokens int
	// Cache token classes (Anthropic): read = cache_read_input_tokens,
	// write = cache_creation_input_tokens. Distinct from InputTokens (no overlap).
	// Portal prices them only when the model's token price sets cache prices;
	// 0/absent → cache simply not billed.
	CacheReadTokens  int
	CacheWriteTokens int
	// IMAGE
	SizeTier string
	Quantity int
}

// BuildUsageBillable constructs a usage.billable.completed event. idempotency_key
// equals event_id so Portal's inbox (external_event_id) and earning
// (idempotency_key) idempotency both key off the same stable value.
func BuildUsageBillable(in UsageBillableInput) Event {
	payload := map[string]any{
		"request_id":                   in.RequestID,
		"external_provider_account_id": in.ExternalProviderAccountID,
		"sub2api_account_id":           in.Sub2apiAccountID,
		"idempotency_key":              in.EventID,
		"model":                        in.Model,
		"billing_type":                 in.BillingType,
		"occurred_at":                  in.OccurredAt,
	}
	if in.BillingType == "IMAGE" {
		payload["size_tier"] = in.SizeTier
		payload["quantity"] = in.Quantity
	} else {
		payload["input_tokens"] = in.InputTokens
		payload["output_tokens"] = in.OutputTokens
		// Additive: cache token classes. Portal folds them into gross only when
		// the model has cache prices set; older Portals ignore unknown keys.
		payload["cache_read_tokens"] = in.CacheReadTokens
		payload["cache_write_tokens"] = in.CacheWriteTokens
	}
	return Event{
		EventID: in.EventID,
		Body: map[string]any{
			"schema_version": 1,
			"event_id":       in.EventID,
			"event_type":     "usage.billable.completed",
			"created_at":     in.CreatedAt,
			"payload":        payload,
		},
	}
}

// UsageBillableEventID derives a STABLE event id from the usage request id:
// the same charged usage row always yields the same evt id, so an outbox
// resend reuses it and Portal's inbox idempotency collapses duplicates.
func UsageBillableEventID(requestID string) string {
	return fmt.Sprintf("evt_usage_%s", requestID)
}
