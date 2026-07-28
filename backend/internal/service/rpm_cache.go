package service

import "context"

// RPMCache RPM 计数器缓存接口
// 用于 Anthropic OAuth/SetupToken 账号的每分钟请求数限制
type RPMCache interface {
	// IncrementRPM 原子递增并返回当前分钟的计数
	// 使用 Redis 服务器时间确定 minute key，避免多实例时钟偏差
	IncrementRPM(ctx context.Context, accountID int64) (count int, err error)

	// GetRPM 获取当前分钟的 RPM 计数
	GetRPM(ctx context.Context, accountID int64) (count int, err error)

	// GetRPMBatch 批量获取多个账号的 RPM 计数（使用 Pipeline）
	GetRPMBatch(ctx context.Context, accountIDs []int64) (map[int64]int, error)

	// Phase 21I: 每小时请求数（RPH）计数，对齐 DeRouter 的每小时调度上限。
	// 与 RPM 同样的 Redis 计数模式，只是按小时分桶。

	// IncrementRPH 原子递增并返回当前小时的计数
	IncrementRPH(ctx context.Context, accountID int64) (count int, err error)

	// GetRPH 获取当前小时的 RPH 计数
	GetRPH(ctx context.Context, accountID int64) (count int, err error)

	// GetRPHBatch 批量获取多个账号的 RPH 计数
	GetRPHBatch(ctx context.Context, accountIDs []int64) (map[int64]int, error)

	// Phase 21I: 账号存活期累计成功/总请求计数（用于成功率展示）。
	// 永久累计（无小时分桶），O(1) 读写，不碰主库。

	// IncrementRequestOutcome 累计一次请求结果：success=true 记成功，
	// 同时无论成败都记一次总请求。
	IncrementRequestOutcome(ctx context.Context, accountID int64, success bool) error

	// GetRequestOutcome 返回账号存活期累计的 (成功数, 总数)。
	GetRequestOutcome(ctx context.Context, accountID int64) (success int64, total int64, err error)
}
