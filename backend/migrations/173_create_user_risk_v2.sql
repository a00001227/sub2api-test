-- Model Extraction Risk V2 Shadow（切片 3/3.1）：每用户「最新一条」V2 Assessment（仅观测/影子）。
-- additive：不修改 legacy user_risk、不删除旧字段、不需要 backfill；空表时现有系统正常运行。
-- 绝不据此执行任何限速/封禁/路由动作（effective_action 恒 NONE，且有 CHECK 强制）。
-- 幂等（IF NOT EXISTS），可重复执行。
-- 时间以 unix 秒（BIGINT）存储：确定性 stale-guard/cycle 版本比较，跨库一致。
-- 隐私：绝不保存 raw prompt/response、指纹摘要(hmac)/相似度(simhash)、密钥、client/server 请求 id、
--       完整 api key、tool schema、Redis 成员、原始请求体。JSON 列仅存白名单化后的评分明细。
-- assessment_digest：白名单化+稳定排序后 Assessment 的 SHA-256，用于同 cycle 同/异内容判定（不含任何原文/敏感值）。
-- 回滚：DROP TABLE IF EXISTS user_risk_v2;（见 rollback 说明）。

CREATE TABLE IF NOT EXISTS user_risk_v2 (
    user_id                 BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    risk_index              DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (risk_index >= 0 AND risk_index <= 100),
    risk_tier               VARCHAR(20)      NOT NULL DEFAULT 'INSUFFICIENT_DATA'
                                CHECK (risk_tier IN ('INSUFFICIENT_DATA','WATCH','MEDIUM','HIGH')),
    confidence              DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (confidence >= 0 AND confidence <= 1),
    data_sufficient         BOOLEAN          NOT NULL DEFAULT false,

    -- 不可用平面分数写 NULL（绝不用 0 冒充低风险）；available 布尔独立；一致性由 CHECK 强制。
    automation_score        DOUBLE PRECISION CHECK (automation_score IS NULL OR (automation_score >= 0 AND automation_score <= 100)),
    automation_available    BOOLEAN          NOT NULL DEFAULT false,
    harvest_score           DOUBLE PRECISION CHECK (harvest_score IS NULL OR (harvest_score >= 0 AND harvest_score <= 100)),
    harvest_available       BOOLEAN          NOT NULL DEFAULT false,
    campaign_score          DOUBLE PRECISION CHECK (campaign_score IS NULL OR (campaign_score >= 0 AND campaign_score <= 100)),
    campaign_available      BOOLEAN          NOT NULL DEFAULT false,
    exposure_score          DOUBLE PRECISION CHECK (exposure_score IS NULL OR (exposure_score >= 0 AND exposure_score <= 100)),
    exposure_available      BOOLEAN          NOT NULL DEFAULT false,

    feature_availability    JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(feature_availability) = 'object'),
    evidence_families       JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(evidence_families) = 'array'),
    evidence_groups         JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(evidence_groups) = 'array'),
    reason_codes            JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(reason_codes) = 'array'),
    incomplete_reasons      JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(incomplete_reasons) = 'array'),

    degraded                BOOLEAN     NOT NULL DEFAULT false,
    incomplete              BOOLEAN     NOT NULL DEFAULT false,
    health_available        BOOLEAN     NOT NULL DEFAULT false,
    effective_action        VARCHAR(8)  NOT NULL DEFAULT 'NONE' CHECK (effective_action = 'NONE'),

    assessment_digest       CHAR(64)    NOT NULL CHECK (assessment_digest ~ '^[0-9a-f]{64}$'),
    feature_version         VARCHAR(32) NOT NULL CHECK (feature_version <> ''),
    policy_version          VARCHAR(48) NOT NULL CHECK (policy_version <> ''),
    fingerprint_key_version VARCHAR(32) NOT NULL DEFAULT '',

    assessed_at             BIGINT NOT NULL DEFAULT 0 CHECK (assessed_at >= 0),
    created_at              BIGINT NOT NULL CHECK (created_at > 0),
    updated_at              BIGINT NOT NULL CHECK (updated_at > 0),

    -- available / score 一致性：available=false ↔ score IS NULL；available=true ↔ score IS NOT NULL。
    CONSTRAINT chk_user_risk_v2_automation CHECK ((automation_available AND automation_score IS NOT NULL) OR (NOT automation_available AND automation_score IS NULL)),
    CONSTRAINT chk_user_risk_v2_harvest    CHECK ((harvest_available AND harvest_score IS NOT NULL)       OR (NOT harvest_available AND harvest_score IS NULL)),
    CONSTRAINT chk_user_risk_v2_campaign   CHECK ((campaign_available AND campaign_score IS NOT NULL)      OR (NOT campaign_available AND campaign_score IS NULL)),
    CONSTRAINT chk_user_risk_v2_exposure   CHECK ((exposure_available AND exposure_score IS NOT NULL)      OR (NOT exposure_available AND exposure_score IS NULL))
);

-- 索引（列表/过滤用）；不建高成本 JSONB GIN 索引。
CREATE INDEX IF NOT EXISTS idx_user_risk_v2_tier_index ON user_risk_v2 (risk_tier, risk_index DESC);
CREATE INDEX IF NOT EXISTS idx_user_risk_v2_assessed_at ON user_risk_v2 (assessed_at DESC);
CREATE INDEX IF NOT EXISTS idx_user_risk_v2_degraded ON user_risk_v2 (degraded);
CREATE INDEX IF NOT EXISTS idx_user_risk_v2_updated_at ON user_risk_v2 (updated_at DESC);
