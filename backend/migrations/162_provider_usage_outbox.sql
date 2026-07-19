-- Phase 21E-6D-6B-2: Provider Usage Billable Outbox.
--
-- Transactional outbox for usage.billable.completed events. A row is enqueued
-- INSIDE the usage-billing debit transaction (usage_billing_repo.Apply), right
-- after the usage_billing_dedup claim succeeds — so "user was charged" and
-- "provider earning event queued" commit atomically. A separate worker claims
-- pending rows (FOR UPDATE SKIP LOCKED) and delivers them via the existing
-- providerwebhook.Sender.
--
-- Only accounts linked to a Portal provider (accounts.external_provider_account_id
-- IS NOT NULL) produce rows — ordinary accounts are not provider revenue.
-- The row carries a payload snapshot (NOT a usage_logs FK) because usage_logs is
-- written best-effort/async and may not exist yet.

CREATE TABLE IF NOT EXISTS provider_usage_outbox (
    id                            BIGSERIAL PRIMARY KEY,
    event_id                      TEXT NOT NULL,
    request_id                    TEXT NOT NULL,
    external_provider_account_id  VARCHAR(64) NOT NULL,
    sub2api_account_id            BIGINT NOT NULL,
    payload                       JSONB NOT NULL,
    status                        TEXT NOT NULL DEFAULT 'pending',
    retry_count                   INTEGER NOT NULL DEFAULT 0,
    next_retry_at                 TIMESTAMPTZ,
    sent_at                       TIMESTAMPTZ,
    last_error                    TEXT,
    created_at                    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- event_id = evt_usage_<request_id>: stable per charged usage row. UNIQUE makes
-- the in-transaction enqueue idempotent (ON CONFLICT DO NOTHING) — a retried
-- billing attempt (same request_id) never double-queues.
CREATE UNIQUE INDEX IF NOT EXISTS uq_provider_usage_outbox_event_id
    ON provider_usage_outbox (event_id);

-- Worker claim path: oldest pending/failed rows whose backoff has elapsed.
CREATE INDEX IF NOT EXISTS idx_provider_usage_outbox_status_next_retry
    ON provider_usage_outbox (status, next_retry_at);
