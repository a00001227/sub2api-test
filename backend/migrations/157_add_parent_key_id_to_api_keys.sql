-- Add parent_key_id to api_keys to distinguish account keys (NULL) from sub keys (NOT NULL).
ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS parent_key_id BIGINT NULL;

CREATE INDEX IF NOT EXISTS api_keys_parent_key_id_idx ON api_keys (parent_key_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS api_keys_user_id_parent_key_id_idx ON api_keys (user_id, parent_key_id) WHERE deleted_at IS NULL;
