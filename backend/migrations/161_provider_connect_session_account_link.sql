-- Provider Connect completion: link the finished session to the created
-- Sub2API account (Phase 21E-6C-2C). Forward-only; nullable; no backfill.
ALTER TABLE provider_connect_sessions
    ADD COLUMN IF NOT EXISTS sub2api_account_id BIGINT;
