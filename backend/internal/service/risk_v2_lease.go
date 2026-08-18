package service

import (
	"context"
	"time"
)

// 切片 4：Leader Lease —— 多实例下只允许一个 Scoring Worker 主动扫描。
//
// 语义（Redis 实现，唯一随机 owner token）：
//   - Acquire：SET NX PX 获取；已被他人持有 → ok=false（lease_contended）。
//   - Renew：compare-token（GET==token 才 PEXPIRE），token 不符 → ok=false（lease_lost）。
//   - Release：compare-token（GET==token 才 DEL），绝不 DEL 他人 lease。
//
// 禁止无 token 的 DEL；禁止释放其他实例的 lease；lease 丢失后调用方必须立即 cancel 当前周期且不写库。
type RiskV2Lease interface {
	Acquire(ctx context.Context, ttl time.Duration) (token string, ok bool, err error)
	Renew(ctx context.Context, token string, ttl time.Duration) (ok bool, err error)
	Release(ctx context.Context, token string) error
}
