ALTER TABLE feedbacks ADD COLUMN IF NOT EXISTS admin_reply TEXT DEFAULT NULL;
ALTER TABLE feedbacks ADD COLUMN IF NOT EXISTS replied_at TIMESTAMPTZ DEFAULT NULL;

COMMENT ON COLUMN feedbacks.admin_reply IS '管理员回复内容';
COMMENT ON COLUMN feedbacks.replied_at IS '回复时间';
