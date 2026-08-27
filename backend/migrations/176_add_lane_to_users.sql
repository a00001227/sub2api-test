-- Add lane (workload tier / 号池) to users for demand-side pool confinement (护号).
-- A user may only use groups whose lane matches theirs; cross-lane is 403 + hidden.
-- Values: normal|batch|distillation. Legacy rows default to normal (no behavior
-- change until a user is tagged non-normal).
ALTER TABLE users ADD COLUMN IF NOT EXISTS lane VARCHAR(20) NOT NULL DEFAULT 'normal';
