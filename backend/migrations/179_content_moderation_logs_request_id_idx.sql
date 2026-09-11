-- 提示词审计「结果」列读时按 request_id 关联风控日志（LEFT JOIN LATERAL 取最近一条），
-- 补一个 request_id 索引避免每行 seq scan。空串是历史默认值、不参与关联，用部分索引缩小体积。
CREATE INDEX IF NOT EXISTS idx_content_moderation_logs_request_id
    ON content_moderation_logs (request_id, created_at DESC)
    WHERE request_id <> '';
