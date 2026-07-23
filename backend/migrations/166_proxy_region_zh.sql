-- Localized (zh) region/city display name, auto-filled from the proxy geo-probe
-- at create time. Display only; provider-connect matching/grouping always use
-- the English `region`. Forward-only; additive.
ALTER TABLE proxies ADD COLUMN IF NOT EXISTS region_zh varchar(40);
