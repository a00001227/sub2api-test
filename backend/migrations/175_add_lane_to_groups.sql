-- Add lane (workload tier / 号池) to groups for demand-side pool routing (护号).
-- Central edge-forward routes a consumer's request only to cells of the group's
-- lane. Values: normal|batch|distillation. Legacy rows default to normal (no
-- behavior change until a group is tagged non-normal).
ALTER TABLE groups ADD COLUMN IF NOT EXISTS lane VARCHAR(20) NOT NULL DEFAULT 'normal';
