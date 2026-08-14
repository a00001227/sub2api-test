-- 为 pricing_models 增加官方缓存读/写参考价，用于按字段计算折扣。
-- 与 official_input_price / official_output_price 对齐，均为 USD-per-token 浮点数。
-- 幂等（IF NOT EXISTS），可重复执行。

ALTER TABLE pricing_models
    ADD COLUMN IF NOT EXISTS official_cache_read_price  DOUBLE PRECISION;

ALTER TABLE pricing_models
    ADD COLUMN IF NOT EXISTS official_cache_write_price DOUBLE PRECISION;
