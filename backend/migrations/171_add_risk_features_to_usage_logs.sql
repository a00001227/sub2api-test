-- 反蒸馏/账号保护 Phase 0（仅观测/影子模式）：为 usage_logs 增加请求特征列。
-- 仅记录计数与 64 位 simhash，绝不存储 prompt/message 文本内容（隐私）。
-- 特征在响应后的用量记录路径（RecordUsage）异步写入，不进入请求热路径。
-- 幂等（IF NOT EXISTS），可重复执行。

ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS message_count SMALLINT,
    ADD COLUMN IF NOT EXISTS max_tokens INT,
    ADD COLUMN IF NOT EXISTS temperature REAL,
    ADD COLUMN IF NOT EXISTS prompt_simhash BIGINT;
