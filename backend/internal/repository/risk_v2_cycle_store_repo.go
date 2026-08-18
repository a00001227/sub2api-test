package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// 切片 4.1：周期状态 + 失败重试的 Redis 实现。
//
//	riskv2:<schema>:fp:<fpVer>:scoring:cycle:<cycleEnd>   HASH 周期状态（owner-only 写，TTL）
//	riskv2:<schema>:fp:<fpVer>:scoring:retry:c:<cycleEnd> ZSET  member=user_id, score=attempts（TTL, 有界）
//
// 只存内部 user_id 与计数；绝不含 Prompt/HMAC/API Key/请求 ID。

const (
	riskV2RetryMaxMembers  = 5000 // retry 集容量上限；超出 → overflow → cycle incomplete
	riskV2RetryMaxAttempts = 3    // 单用户最大重试次数；超过永久放弃
)

// saveCycleStateSrc：lease + owner 双重原子校验后写入。
// KEYS[1]=cycleKey, KEYS[2]=leaseKey；ARGV[1]=owner_hash, ARGV[10]=lease token(明文)，其余为字段。
// lease 不属于本 token（丢失/被接管/过期）→ 返回 -1；owner_hash 非空且 != 传入 → 返回 0；否则写全字段 + EXPIRE，返回 1。
const riskV2SaveCycleStateSrc = `
if redis.call('GET', KEYS[2]) ~= ARGV[10] then
  return -1
end
local cur = redis.call('HGET', KEYS[1], 'owner_hash')
if cur and cur ~= '' and cur ~= ARGV[1] then
  return 0
end
redis.call('HSET', KEYS[1],
  'owner_hash', ARGV[1],
  'status', ARGV[2],
  'cursor', ARGV[3],
  'users_seen', ARGV[4],
  'users_scored', ARGV[5],
  'users_failed', ARGV[6],
  'updated_at', ARGV[7],
  'incomplete_reason', ARGV[8])
redis.call('EXPIRE', KEYS[1], ARGV[9])
return 1
`

// claimCycleSrc：当前 lease owner 接管周期。先校验 lease 仍属本 token（KEYS[2]=leaseKey, ARGV[3]=token 明文），
// 否则返回 -1（Acquire 后 Claim 前 lease 已过期/被接管 → 拒绝，旧 worker 不得接管）。
// 通过则强制 owner_hash + status=RUNNING，保留既有 cursor（不存在则初始化空）。
const riskV2ClaimCycleSrc = `
if redis.call('GET', KEYS[2]) ~= ARGV[3] then
  return -1
end
redis.call('HSET', KEYS[1], 'owner_hash', ARGV[1], 'status', 'RUNNING')
if redis.call('HEXISTS', KEYS[1], 'cursor') == 0 then
  redis.call('HSET', KEYS[1], 'cursor', '')
end
redis.call('EXPIRE', KEYS[1], ARGV[2])
return 1
`

// addRetryUserSrc：单用户加入 retry ZSET。ARGV=[member, attempts, maxAttempts, maxMembers, ttlSec]。
// attempts>maxAttempts → 丢弃(返回 2)；已存在 → 更新 score(返回 1)；容量满 → overflow(返回 -1)；新增(返回 0)。
const riskV2AddRetrySrc = `
if tonumber(ARGV[2]) > tonumber(ARGV[3]) then
  return 2
end
if redis.call('ZSCORE', KEYS[1], ARGV[1]) then
  redis.call('ZADD', KEYS[1], ARGV[2], ARGV[1])
  redis.call('EXPIRE', KEYS[1], ARGV[5])
  return 1
end
if tonumber(redis.call('ZCARD', KEYS[1])) >= tonumber(ARGV[4]) then
  return -1
end
redis.call('ZADD', KEYS[1], ARGV[2], ARGV[1])
redis.call('EXPIRE', KEYS[1], ARGV[5])
return 0
`

type riskV2CycleStore struct {
	rdb      *redis.Client
	schema   string
	fpVer    string
	ttl      time.Duration // cycle-state TTL
	retryTTL time.Duration // retry 集 TTL（独立公式，见 config.RetryTTL）
	now      func() time.Time
	leaseKey string // 与 cycle/retry 同 Slot（{coord} tag），供原子 lease 校验
}

// NewRiskV2CycleStore 构造周期状态/重试存储。cycleTTL/retryTTL 分别覆盖 catch-up 与最大重试周期。
// rdb 为 nil → nil（Worker 回退纯内存 cursor）。
func NewRiskV2CycleStore(rdb *redis.Client, schema, fpVer string, cycleTTL, retryTTL time.Duration, now func() time.Time) service.RiskV2CycleStore {
	if rdb == nil {
		return nil
	}
	if schema == "" {
		schema = service.RiskV2AggSchemaVersion
	}
	if fpVer == "" {
		fpVer = "v1"
	}
	if cycleTTL <= 0 {
		cycleTTL = 2 * time.Hour
	}
	if retryTTL <= 0 {
		retryTTL = 2 * time.Hour
	}
	if now == nil {
		now = time.Now
	}
	return &riskV2CycleStore{rdb: rdb, schema: schema, fpVer: fpVer, ttl: cycleTTL, retryTTL: retryTTL, now: now, leaseKey: riskV2LeaseKey(schema, fpVer)}
}

func (s *riskV2CycleStore) cycleKey(cycleEnd int64) string {
	return riskV2ScoringCoordPrefix(s.schema, s.fpVer) + ":cycle:" + strconv.FormatInt(cycleEnd, 10)
}
func (s *riskV2CycleStore) retryKey(cycleEnd int64) string {
	return riskV2ScoringCoordPrefix(s.schema, s.fpVer) + ":retry:c:" + strconv.FormatInt(cycleEnd, 10)
}

// riskV2HashOwner 返回 lease token 的 sha256 hex（存 hash 而非明文 token）。
func riskV2HashOwner(token string) string {
	if token == "" {
		return ""
	}
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func (s *riskV2CycleStore) LoadCycleState(ctx context.Context, cycleEnd int64) (service.RiskV2CycleState, bool, error) {
	var st service.RiskV2CycleState
	ctx, cancel := context.WithTimeout(ctx, riskV2AggReadTimeout)
	defer cancel()
	m, err := s.rdb.HGetAll(ctx, s.cycleKey(cycleEnd)).Result()
	if err != nil && err != redis.Nil {
		return st, false, err
	}
	if len(m) == 0 {
		return st, false, nil
	}
	st.Status = m["status"]
	st.Cursor = m["cursor"]
	st.UsersSeen = atoi64Safe(m["users_seen"])
	st.UsersScored = atoi64Safe(m["users_scored"])
	st.UsersFailed = atoi64Safe(m["users_failed"])
	st.UpdatedAtUnix = atoi64Safe(m["updated_at"])
	st.OwnerTokenHash = m["owner_hash"]
	st.IncompleteReason = m["incomplete_reason"]
	return st, true, nil
}

// ErrRiskV2LeaseNotHeld：写周期状态时 lease 已不属于本 token（丢失/被接管/过期）。
var ErrRiskV2LeaseNotHeld = errors.New("risk_v2: lease not held by this token")

func (s *riskV2CycleStore) ClaimCycleState(ctx context.Context, cycleEnd int64, ownerToken string) error {
	ctx, cancel := context.WithTimeout(ctx, riskV2AggReadTimeout)
	defer cancel()
	res, err := s.rdb.Eval(ctx, riskV2ClaimCycleSrc, []string{s.cycleKey(cycleEnd), s.leaseKey},
		riskV2HashOwner(ownerToken),
		strconv.FormatInt(int64(s.ttl.Seconds()), 10),
		ownerToken, // 明文 token 校验 lease
	).Int64()
	if err != nil && err != redis.Nil {
		return err
	}
	if res == -1 {
		return ErrRiskV2LeaseNotHeld // Acquire 后 Claim 前 lease 已过期/被接管
	}
	return nil
}

func (s *riskV2CycleStore) SaveCycleState(ctx context.Context, cycleEnd int64, st service.RiskV2CycleState, ownerToken string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, riskV2AggReadTimeout)
	defer cancel()
	res, err := s.rdb.Eval(ctx, riskV2SaveCycleStateSrc, []string{s.cycleKey(cycleEnd), s.leaseKey},
		riskV2HashOwner(ownerToken),
		st.Status, st.Cursor,
		strconv.FormatInt(st.UsersSeen, 10),
		strconv.FormatInt(st.UsersScored, 10),
		strconv.FormatInt(st.UsersFailed, 10),
		strconv.FormatInt(s.now().Unix(), 10),
		st.IncompleteReason,
		strconv.FormatInt(int64(s.ttl.Seconds()), 10),
		ownerToken, // ARGV[10] 明文 token 校验 lease
	).Int64()
	if err != nil && err != redis.Nil {
		return false, err
	}
	// res: 1=APPLIED, 0=owner 不匹配, -1=lease 不属于本 token；后两者均返回 ok=false（不写、不完成）。
	return res == 1, nil
}

func (s *riskV2CycleStore) AddRetryUsers(ctx context.Context, targetCycleEnd int64, users []service.RiskV2RetryUser) (bool, error) {
	if len(users) == 0 {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, riskV2AggReadTimeout)
	defer cancel()
	key := s.retryKey(targetCycleEnd)
	ttlSec := strconv.FormatInt(int64(s.retryTTL.Seconds()), 10)
	overflow := false
	for _, u := range users {
		if u.UserID <= 0 {
			continue
		}
		res, err := s.rdb.Eval(ctx, riskV2AddRetrySrc, []string{key},
			strconv.FormatInt(u.UserID, 10),
			strconv.Itoa(u.Attempts),
			strconv.Itoa(riskV2RetryMaxAttempts),
			strconv.Itoa(riskV2RetryMaxMembers),
			ttlSec,
		).Int64()
		if err != nil && err != redis.Nil {
			return overflow, err
		}
		if res == -1 {
			overflow = true
		}
	}
	return overflow, nil
}

func (s *riskV2CycleStore) LoadRetryUsers(ctx context.Context, cycleEnd int64, limit int) ([]service.RiskV2RetryUser, error) {
	if limit <= 0 {
		limit = riskV2ActiveUserScanDefault
	}
	if limit > riskV2RetryMaxMembers {
		limit = riskV2RetryMaxMembers
	}
	ctx, cancel := context.WithTimeout(ctx, riskV2AggReadTimeout)
	defer cancel()
	zs, err := s.rdb.ZRangeWithScores(ctx, s.retryKey(cycleEnd), 0, int64(limit-1)).Result()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	out := make([]service.RiskV2RetryUser, 0, len(zs))
	for _, z := range zs {
		member, _ := z.Member.(string)
		uid, perr := strconv.ParseInt(member, 10, 64)
		if perr != nil || uid <= 0 {
			continue
		}
		out = append(out, service.RiskV2RetryUser{UserID: uid, Attempts: int(z.Score)})
	}
	return out, nil
}
