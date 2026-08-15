-- 反蒸馏/账号保护 Phase 0（仅观测/影子模式）：每用户风险评分表。
-- 由评分 worker（约每 5 分钟）根据 Redis 特征草图 + 免费计数器 + usage_logs
-- 24h 聚合计算 score/tier/features 后 upsert。Phase 0 绝不据此执行任何拦截。
-- 幂等（IF NOT EXISTS），可重复执行。

CREATE TABLE IF NOT EXISTS user_risk (
    user_id     BIGINT PRIMARY KEY,
    score       SMALLINT     NOT NULL DEFAULT 0,
    tier        VARCHAR(8)   NOT NULL DEFAULT 'watch',
    features    JSONB,
    allowlisted BOOLEAN      NOT NULL DEFAULT false,
    manual_tier VARCHAR(8),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_user_risk_score ON user_risk (score DESC);
