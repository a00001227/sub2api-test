CREATE TABLE IF NOT EXISTS feedbacks (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    type VARCHAR(40) NOT NULL,
    content TEXT NOT NULL,
    request_id VARCHAR(200) DEFAULT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'pending',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_feedbacks_status ON feedbacks(status);
CREATE INDEX IF NOT EXISTS idx_feedbacks_created_at ON feedbacks(created_at);
CREATE INDEX IF NOT EXISTS idx_feedbacks_user_id ON feedbacks(user_id);

COMMENT ON TABLE feedbacks IS '用户反馈/工单';
COMMENT ON COLUMN feedbacks.type IS '反馈类型: api_error, billing, key_mgmt, feature';
COMMENT ON COLUMN feedbacks.status IS '状态: pending(未处理), resolved(已处理)';
COMMENT ON COLUMN feedbacks.request_id IS '关联的请求ID（可选）';
