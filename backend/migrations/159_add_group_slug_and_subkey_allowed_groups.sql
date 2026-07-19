-- Group URL slug: lets a group be selected per-request via URI prefix
-- (e.g. /claude-fast/v1/messages). Uniqueness via partial index so soft-deleted
-- groups release their slug (same pattern as groups.name).
ALTER TABLE groups ADD COLUMN IF NOT EXISTS slug VARCHAR(64);
CREATE UNIQUE INDEX IF NOT EXISTS idx_groups_slug_unique
    ON groups (slug)
    WHERE slug IS NOT NULL AND slug <> '' AND deleted_at IS NULL;

-- Sub key channel whitelist: extra groups (beyond the bound group) a client
-- key may select via URI prefix. NULL/empty = locked to the bound group.
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS allowed_group_ids JSONB;
