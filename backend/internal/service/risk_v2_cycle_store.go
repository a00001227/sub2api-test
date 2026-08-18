package service

import "context"

// 切片 4.1：周期状态持久化 + 失败用户 retry（Redis）。
//
// 周期状态让 Leader 切换/进程重启后能从 cursor 恢复、不重复全量扫描；只有当前 Lease Owner 可改状态。
// 绝不保存 Prompt / HMAC / API Key / 请求 ID —— 只存内部 user_id 与计数。

// 周期状态机。
const (
	RiskV2CyclePending    = "PENDING"
	RiskV2CycleRunning    = "RUNNING"
	RiskV2CycleIncomplete = "INCOMPLETE"
	RiskV2CycleCompleted  = "COMPLETED"
)

// RiskV2CycleState 是某评分周期的持久化进度。
type RiskV2CycleState struct {
	Status           string
	Cursor           string
	UsersSeen        int64
	UsersScored      int64
	UsersFailed      int64
	UpdatedAtUnix    int64
	OwnerTokenHash   string // = hash(lease token)；只有匹配的 owner 可改状态
	IncompleteReason string
}

// RiskV2RetryUser 是一个待重试用户及其已重试次数（attempts）。
type RiskV2RetryUser struct {
	UserID   int64
	Attempts int
}

// RiskV2CycleStore 是周期状态 + 失败重试的持久化面（Redis 实现）。
type RiskV2CycleStore interface {
	// LoadCycleState 读取某周期状态；不存在返回 (zero,false,nil)。
	LoadCycleState(ctx context.Context, cycleEnd int64) (RiskV2CycleState, bool, error)
	// ClaimCycleState 由「当前 Lease Owner」在开始处理周期时调用：强制把 owner 置为本 token 并置 RUNNING，
	// 保留既有 cursor/counts（供恢复）。仅 lease 持有者可达（Acquire 成功后），故安全支持 leader 切换接管；
	// 接管后旧 leader 的 owner-only 写入会被拒（防止 lease 丢失后的陈旧写）。
	ClaimCycleState(ctx context.Context, cycleEnd int64, ownerToken string) error
	// SaveCycleState 写入周期状态：仅当已存 owner_hash 为空或等于 hash(ownerToken) 时才写（owner-only，原子）。
	// 返回 ok=false 表示 owner 不匹配（lease 已易主），调用方必须停止写库/标记完成。
	SaveCycleState(ctx context.Context, cycleEnd int64, st RiskV2CycleState, ownerToken string) (ok bool, err error)

	// AddRetryUsers 把失败用户加入「下一周期」retry 集（ZSET，score=attempts）。
	// attempts 超过 maxAttempts 的用户被丢弃（永久放弃，不无限重试）；容量满 → overflow=true。
	AddRetryUsers(ctx context.Context, targetCycleEnd int64, users []RiskV2RetryUser) (overflow bool, err error)
	// LoadRetryUsers 读取某周期的 retry 用户（有界）。
	LoadRetryUsers(ctx context.Context, cycleEnd int64, limit int) ([]RiskV2RetryUser, error)
}
