-- Add display_multiplier to api_keys for derouter-style virtual budget display.
-- Account keys keep 1; sub keys store budgetVirtual / paidAmount. Legacy rows default to 1.
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS display_multiplier DECIMAL(20,8) NOT NULL DEFAULT 1;
