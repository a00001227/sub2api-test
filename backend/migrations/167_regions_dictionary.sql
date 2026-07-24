-- Egress-region dictionary refactor.
--
-- region names now live in ONE place: the `regions` table (code/name_en/name_zh).
-- proxies.region stores a Region.code (upper-case IATA-style, e.g. SJC/NRT/ORD).
-- The old per-proxy `region_zh` column and the geo-probe zh auto-fill are removed.
--
-- Dev-stage note: existing proxies.region values were free-text placeholder city
-- names (fake data), so they are cleared here — re-pick each proxy's region from
-- the new dropdown after deploy.

-- 1) Region dictionary table.
CREATE TABLE IF NOT EXISTS regions (
    id          BIGSERIAL PRIMARY KEY,
    code        VARCHAR(20)  NOT NULL,
    name_en     VARCHAR(60)  NOT NULL,
    name_zh     VARCHAR(60)  NOT NULL,
    sort_order  INTEGER      NOT NULL DEFAULT 0,
    enabled     BOOLEAN      NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS region_code       ON regions (code);
CREATE INDEX        IF NOT EXISTS region_enabled    ON regions (enabled);
CREATE INDEX        IF NOT EXISTS region_sort_order ON regions (sort_order);

-- 2) Drop the old per-proxy Chinese display column (names come from `regions`).
ALTER TABLE proxies DROP COLUMN IF EXISTS region_zh;

-- 3) Clear placeholder region values so proxies are re-pointed at real codes.
UPDATE proxies SET region = NULL WHERE region IS NOT NULL;
