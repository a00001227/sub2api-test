-- 提示词审计（Prompt Audit）：独立于内容审核（content_moderation_logs）的原文留存。
-- 内容审核只存 240 字摘要用于风控判定；此表按需留存用户提示词全文供事后审计/取证。
-- 设计：
--   1. 与内容审核解耦，单独开关（settings: prompt_audit_config）、单独保留期清理。
--   2. full_prompt 存请求中抽取到的用户提示词文本（各协议复用内容审核抽取逻辑，去除审核用截断）。
--   3. 列结构对齐升级版 securityaudit.prompt_audit_events 的子集，将来接入 AI 护栏可平滑扩列。
CREATE TABLE IF NOT EXISTS prompt_audit_events (
    id                    BIGSERIAL PRIMARY KEY,
    request_id            VARCHAR(128) NOT NULL DEFAULT '',
    user_id               BIGINT REFERENCES users(id) ON DELETE SET NULL,
    user_email            VARCHAR(320) NOT NULL DEFAULT '',
    api_key_id            BIGINT REFERENCES api_keys(id) ON DELETE SET NULL,
    api_key_name          VARCHAR(255) NOT NULL DEFAULT '',
    group_id              BIGINT REFERENCES groups(id) ON DELETE SET NULL,
    group_name            VARCHAR(255) NOT NULL DEFAULT '',
    provider              VARCHAR(64) NOT NULL DEFAULT '',
    endpoint              VARCHAR(128) NOT NULL DEFAULT '',
    protocol              VARCHAR(64) NOT NULL DEFAULT '',
    model                 VARCHAR(255) NOT NULL DEFAULT '',
    prompt_hash           VARCHAR(64) NOT NULL DEFAULT '',
    prompt_length         INT NOT NULL DEFAULT 0,
    message_count         INT NOT NULL DEFAULT 0,
    full_prompt           TEXT NOT NULL DEFAULT '',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_prompt_audit_events_nonnegative
        CHECK (prompt_length >= 0 AND message_count >= 0)
);

CREATE INDEX IF NOT EXISTS idx_prompt_audit_events_created
    ON prompt_audit_events (created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_prompt_audit_events_user_created
    ON prompt_audit_events (user_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_prompt_audit_events_api_key_created
    ON prompt_audit_events (api_key_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_prompt_audit_events_group_created
    ON prompt_audit_events (group_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_prompt_audit_events_prompt_hash
    ON prompt_audit_events (prompt_hash);
CREATE INDEX IF NOT EXISTS idx_prompt_audit_events_request
    ON prompt_audit_events (request_id);
