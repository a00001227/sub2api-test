-- 为 pricing_models 增加可调展示顺序 sort_order。
-- 后台管理端可拖拽排序，前台公开价格展示按 sort_order ASC, model ASC 排序。
-- 幂等（IF NOT EXISTS），可重复执行。

ALTER TABLE pricing_models
    ADD COLUMN IF NOT EXISTS sort_order INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_pricing_models_sort_order
    ON pricing_models (sort_order);
