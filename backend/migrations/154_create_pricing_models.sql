-- 统一 Pricing Display System 数据表
-- 单表设计，支持 text/image 两种 model_type
-- image 模型使用 image_pricing_json (JSON) 存储动态 resolution key-value

CREATE TABLE IF NOT EXISTS pricing_models (
    id                   BIGSERIAL PRIMARY KEY,
    model                VARCHAR(200) NOT NULL,
    model_type           VARCHAR(20)  NOT NULL CHECK (model_type IN ('text', 'image')),
    user_type            VARCHAR(20)  NOT NULL DEFAULT 'end_user' CHECK (user_type IN ('end_user', 'channel_user')),
    enabled              BOOLEAN      NOT NULL DEFAULT TRUE,

    -- TEXT MODEL fields (nullable for image models)
    input_price          DOUBLE PRECISION,
    output_price         DOUBLE PRECISION,
    cache_read_price     DOUBLE PRECISION,
    cache_write_price    DOUBLE PRECISION,
    official_input_price DOUBLE PRECISION,
    official_output_price DOUBLE PRECISION,

    -- IMAGE MODEL field (nullable for text models)
    -- JSON structure: {"1k": 0.005, "2k": 0.01, "4k": 0.02}
    image_pricing_json   TEXT,

    -- System fields
    saving_percent       DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_pricing_models_model_user_type
    ON pricing_models (model, user_type);

CREATE INDEX IF NOT EXISTS idx_pricing_models_enabled
    ON pricing_models (enabled);
