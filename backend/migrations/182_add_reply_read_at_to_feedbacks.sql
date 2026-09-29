-- 工单回复已读标记：用户查看管理员回复的时间。
-- NULL 或早于 replied_at = 有未读回复（管理员再次回复会刷新 replied_at，自动重新变为未读）。
-- 供 Portal 全局"工单中心"角标：GET /feedback/unread-count 计数、POST /feedback/read 标记已读。
ALTER TABLE feedbacks ADD COLUMN IF NOT EXISTS reply_read_at timestamptz NULL;

COMMENT ON COLUMN feedbacks.reply_read_at IS '用户查看管理员回复的时间；NULL 或早于 replied_at 表示有未读回复';
