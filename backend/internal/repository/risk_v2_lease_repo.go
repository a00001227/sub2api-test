package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// 切片 4：Leader Lease 的 Redis 实现（compare-token renew/release，绝不 DEL 他人）。
// key：riskv2:<schema>:fp:<fpVer>:scoring:leader （单键，含 epoch，避免跨版本互抢）。

// renewSrc：GET==token 才 PEXPIRE。返回 1=续租成功，0=token 不符（lease 已丢失/被抢）。
const riskV2LeaseRenewSrc = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  return 1
end
return 0
`

// releaseSrc：GET==token 才 DEL（幂等：非本 owner 或已过期 → 0）。
const riskV2LeaseReleaseSrc = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`

type riskV2Lease struct {
	rdb    *redis.Client
	key    string
	newTok func() (string, error)
}

// riskV2ScoringCoordPrefix 是「评分协调键」的公共前缀，含固定 hash tag {coord}，
// 使 leader lease / cycle state / retry 等协调键同 Slot（Redis Cluster 下可在单个 Lua 内原子校验 lease + cycle）。
func riskV2ScoringCoordPrefix(schema, fpVer string) string {
	return "riskv2:" + schema + ":fp:" + fpVer + ":scoring:{coord}"
}

// riskV2LeaseKey 是 Leader Lease 键（与 cycle/retry 同 Slot）。
func riskV2LeaseKey(schema, fpVer string) string {
	return riskV2ScoringCoordPrefix(schema, fpVer) + ":leader"
}

// NewRiskV2Lease 构造 Scoring Worker 的 Leader Lease。rdb 为 nil → 返回 nil（上层视为 lease 不可用 → 不启动）。
func NewRiskV2Lease(rdb *redis.Client, schema, fpVer string) service.RiskV2Lease {
	if rdb == nil {
		return nil
	}
	if schema == "" {
		schema = service.RiskV2AggSchemaVersion
	}
	if fpVer == "" {
		fpVer = "v1"
	}
	return &riskV2Lease{
		rdb:    rdb,
		key:    riskV2LeaseKey(schema, fpVer),
		newTok: randomLeaseToken,
	}
}

func randomLeaseToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (l *riskV2Lease) Acquire(ctx context.Context, ttl time.Duration) (string, bool, error) {
	tok, err := l.newTok()
	if err != nil {
		return "", false, err
	}
	ok, err := l.rdb.SetNX(ctx, l.key, tok, ttl).Result()
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, nil // 已被他人持有（lease_contended）
	}
	return tok, true, nil
}

func (l *riskV2Lease) Renew(ctx context.Context, token string, ttl time.Duration) (bool, error) {
	if token == "" {
		return false, nil
	}
	res, err := l.rdb.Eval(ctx, riskV2LeaseRenewSrc, []string{l.key}, token, ttl.Milliseconds()).Int64()
	if err != nil && err != redis.Nil {
		return false, err
	}
	return res == 1, nil
}

func (l *riskV2Lease) Release(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	_, err := l.rdb.Eval(ctx, riskV2LeaseReleaseSrc, []string{l.key}, token).Result()
	if err != nil && err != redis.Nil {
		return err
	}
	return nil
}
