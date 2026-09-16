-- 用户 × 平台 专属并发 / RPM 上限（anthropic / openai 分开设）。
-- 复用 user_platform_quotas 的 (user_id, platform) 维度；NULL = 沿用用户全局值
-- （users.concurrency / users.rpm_limit），0 = 该平台不限，>0 = 该平台专属上限（替代全局值）。
-- 并发计数键：concurrency:user:{user_id}:{platform}；RPM 计数键：rpm:u:{user_id}:{platform}:{minute}。
ALTER TABLE user_platform_quotas ADD COLUMN IF NOT EXISTS concurrency integer NULL;
ALTER TABLE user_platform_quotas ADD COLUMN IF NOT EXISTS rpm_limit integer NULL;

COMMENT ON COLUMN user_platform_quotas.concurrency IS '平台专属并发上限；NULL 沿用 users.concurrency；0 不限；>0 替代全局值';
COMMENT ON COLUMN user_platform_quotas.rpm_limit IS '平台专属 RPM 上限；NULL 沿用 users.rpm_limit；0 不限；>0 替代全局值';
