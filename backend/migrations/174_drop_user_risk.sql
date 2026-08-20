-- 摘除 legacy Risk v1（反蒸馏 Phase 0）：删除 user_risk 表。
-- v1 已被 Risk V2 Shadow（user_risk_v2）取代，相关采集/评分/管理 API/前端页均已移除。
-- 无外键引用 user_risk（user_risk_v2 引用的是 users，与本表无关），删除安全。
-- 幂等（IF EXISTS），可重复执行。
-- 回滚：见 172_create_user_risk.sql 重建该表（仅结构；历史评分数据不恢复，v1 已弃用无需恢复）。
-- Redis 的 legacy risk:* 键不在此处理：有 TTL，会自动过期。

DROP TABLE IF EXISTS user_risk;
