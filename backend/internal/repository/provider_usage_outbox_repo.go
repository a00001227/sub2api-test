package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/service/providerwebhook"
)

// Phase 21E-6D-6B-2: transactional-outbox enqueue for usage.billable.completed.
//
// Runs INSIDE the usage-billing transaction (see usageBillingRepository.Apply),
// after the dedup claim, so it is atomic with the user charge. Steps:
//  1. resolve accounts.external_provider_account_id for cmd.AccountID (same tx).
//  2. NULL / empty  → ordinary account, not provider revenue → NO row (skip).
//  3. otherwise snapshot the usage FACT into provider_usage_outbox with a
//     stable event_id (evt_usage_<request_id>); ON CONFLICT DO NOTHING makes a
//     retried charge idempotent. NO price / gross / commission is stored.
func (r *usageBillingRepository) enqueueProviderUsageOutbox(
	ctx context.Context, tx *sql.Tx, cmd *service.UsageBillingCommand,
) error {
	if cmd == nil {
		return nil
	}

	// (1) forward lookup account_id → external_provider_account_id, in-tx.
	var externalRef sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT external_provider_account_id FROM accounts WHERE id = $1`,
		cmd.AccountID,
	).Scan(&externalRef)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil // account gone — nothing to attribute
		}
		return err
	}
	// (2) not a Portal-linked account → no provider revenue event.
	if !externalRef.Valid || strings.TrimSpace(externalRef.String) == "" {
		return nil
	}

	// (3) build the fact payload + stable event id.
	eventID := providerwebhook.UsageBillableEventID(cmd.RequestID)
	payload := buildUsageOutboxPayload(cmd, eventID, externalRef.String)
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO provider_usage_outbox
			(event_id, request_id, external_provider_account_id, sub2api_account_id,
			 payload, status, retry_count, next_retry_at)
		VALUES ($1, $2, $3, $4, $5, 'pending', 0, NOW())
		ON CONFLICT (event_id) DO NOTHING
	`, eventID, cmd.RequestID, externalRef.String, cmd.AccountID, payloadJSON)
	if err != nil {
		logger.LegacyPrintf("repository.usage_billing",
			"[ProviderUsageOutbox] enqueue failed: request_id=%s err=%v", cmd.RequestID, err)
		return err
	}
	return nil
}

// billingTypeForOutbox maps the internal billing_mode (token/image/per_request)
// onto Portal's billing_type axis. Only "image" is IMAGE; everything else is
// TOKEN. NOT the balance/subscription BillingType int8.
func billingTypeForOutbox(billingMode string) string {
	if strings.EqualFold(strings.TrimSpace(billingMode), "image") {
		return "IMAGE"
	}
	return "TOKEN"
}

// buildUsageOutboxPayload assembles the persisted event body. Field names match
// providerwebhook.BuildUsageBillable / Portal's validatePayload exactly. Stored
// so the worker delivers a byte-stable payload without re-reading usage data.
func buildUsageOutboxPayload(cmd *service.UsageBillingCommand, eventID, externalRef string) map[string]any {
	billingType := billingTypeForOutbox(cmd.BillingMode)
	occurred := cmd.UsageOccurredAt.UTC().Format("2006-01-02T15:04:05Z07:00")

	payload := map[string]any{
		"request_id":                   cmd.RequestID,
		"external_provider_account_id": externalRef,
		"sub2api_account_id":           strconv.FormatInt(cmd.AccountID, 10),
		"idempotency_key":              eventID,
		"model":                        cmd.Model,
		"billing_type":                 billingType,
		"occurred_at":                  occurred,
	}
	if billingType == "IMAGE" {
		payload["size_tier"] = cmd.ImageSizeTier
		payload["quantity"] = cmd.ImageCount
	} else {
		payload["input_tokens"] = cmd.InputTokens
		payload["output_tokens"] = cmd.OutputTokens
	}
	return payload
}
