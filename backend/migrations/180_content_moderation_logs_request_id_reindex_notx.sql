-- 179 建的是部分索引(WHERE request_id <> ''),但历史 content_moderation_logs.request_id 全为空串,
-- 该索引恒空、planner 用不上 → 提示词审计「结果」列的 LEFT JOIN LATERAL 对每条审计记录退化成全表
-- 顺扫 content_moderation_logs(6 万行/126MB),summary 查询跑到 125s 被 statement_timeout 掐断。
-- 改成非部分复合索引 (request_id, created_at DESC),让相关子查询稳定走索引 seek(即使 request_id
-- 为空也瞬时命中 0 行)。CONCURRENTLY 避免建/删索引时锁表阻塞审核日志写入。
DROP INDEX CONCURRENTLY IF EXISTS idx_content_moderation_logs_request_id;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_content_moderation_logs_request_id
    ON content_moderation_logs (request_id, created_at DESC);
