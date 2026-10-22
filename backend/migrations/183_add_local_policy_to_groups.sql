-- Group-level local-channel policy (central): how a group's requests split between
-- edge cells and the group's locally bound accounts (e.g. third-party relays).
-- inherit = legacy env-list behavior; cell_only | local_only | cell_first | local_first | balanced.
ALTER TABLE groups ADD COLUMN IF NOT EXISTS local_policy VARCHAR(20) NOT NULL DEFAULT 'inherit';
ALTER TABLE groups ADD COLUMN IF NOT EXISTS local_weight INTEGER NOT NULL DEFAULT 50;
