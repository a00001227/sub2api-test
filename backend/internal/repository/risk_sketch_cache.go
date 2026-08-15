package repository

import (
	"context"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// Risk Phase 0（仅观测/影子模式）：每用户风险特征草图的 Redis 实现。
//
// 键（均 24h TTL，聚合到 user_id）：
//
//	risk:sim:{uid}    — 近期 simhash 的有界 SET（去重/总量比）
//	risk:turns:{uid}  — HASH {single,total}
//	risk:io:{uid}     — HASH {input,output}
//	risk:model:{uid}  — HASH model→count
//	risk:count:{uid}  — 请求总数（STRING 计数器）
//	risk:active       — 活跃用户 SET（评分 worker 遍历用）
//	risk:user:{uid}   — 评分结果缓存（评分 worker 写）
//
// 全部更新在响应后异步执行（pipeline + 短超时），绝不进入请求热路径。

const (
	riskSimKeyPrefix     = "risk:sim:"
	riskTurnsKeyPrefix   = "risk:turns:"
	riskIOKeyPrefix      = "risk:io:"
	riskModelKeyPrefix   = "risk:model:"
	riskCountKeyPrefix   = "risk:count:"
	riskUserKeyPrefix    = "risk:user:"
	riskActiveSetKey     = "risk:active"
	riskSimSetMaxMembers = 2000 // sim SET 成员上限，防止内存膨胀（超过则截断，比值仍近似）
	riskUpdateTimeout    = 500 * time.Millisecond
	riskReadTimeout      = 2 * time.Second
)

type riskSketchCache struct {
	rdb *redis.Client
}

// NewRiskSketchCache 构造 Risk Phase 0 特征草图缓存。rdb 为 nil 时返回 nil。
func NewRiskSketchCache(rdb *redis.Client) service.RiskSketchCache {
	if rdb == nil {
		return nil
	}
	return &riskSketchCache{rdb: rdb}
}

func riskKey(prefix string, uid int64) string {
	return prefix + strconv.FormatInt(uid, 10)
}

// UpdateSketch 合入一次请求的增量（best-effort：错误仅日志，绝不影响调用方）。
func (c *riskSketchCache) UpdateSketch(ctx context.Context, u service.RiskSketchUpdate) {
	if c == nil || c.rdb == nil || u.UserID <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, riskUpdateTimeout)
	defer cancel()

	ttl := service.RiskSketchTTL
	uid := u.UserID
	simKey := riskKey(riskSimKeyPrefix, uid)
	turnsKey := riskKey(riskTurnsKeyPrefix, uid)
	ioKey := riskKey(riskIOKeyPrefix, uid)
	modelKey := riskKey(riskModelKeyPrefix, uid)
	countKey := riskKey(riskCountKeyPrefix, uid)

	pipe := c.rdb.Pipeline()

	// 请求总数 + 活跃集合。
	pipe.Incr(ctx, countKey)
	pipe.Expire(ctx, countKey, ttl)
	pipe.SAdd(ctx, riskActiveSetKey, uid)
	pipe.Expire(ctx, riskActiveSetKey, ttl)

	// 轮次 hash。
	pipe.HIncrBy(ctx, turnsKey, "total", 1)
	if u.SingleTurn {
		pipe.HIncrBy(ctx, turnsKey, "single", 1)
	}
	pipe.Expire(ctx, turnsKey, ttl)

	// IO token hash。
	if u.InputTokens > 0 {
		pipe.HIncrBy(ctx, ioKey, "input", u.InputTokens)
	}
	if u.OutputTokens > 0 {
		pipe.HIncrBy(ctx, ioKey, "output", u.OutputTokens)
	}
	pipe.Expire(ctx, ioKey, ttl)

	// 模型直方图。
	if u.Model != "" {
		pipe.HIncrBy(ctx, modelKey, u.Model, 1)
		pipe.Expire(ctx, modelKey, ttl)
	}

	// simhash SET（有界）：total 用 hash 里的计数近似，SET 只存去重成员。
	if u.Simhash != 0 {
		pipe.SAdd(ctx, simKey, strconv.FormatUint(u.Simhash, 10))
		pipe.Expire(ctx, simKey, ttl)
		// 总量（含重复）单独计数，用于 distinct/total 比值。
		pipe.HIncrBy(ctx, turnsKey, "sim_total", 1)
	}

	if _, err := pipe.Exec(ctx); err != nil {
		logger.LegacyPrintf("repository.risk_sketch", "[risk] update sketch failed uid=%d: %v", uid, err)
		return
	}

	// SET 超上限则截断（廉价的 SCARD 判定 + SPOP，容忍偶尔超一点）。
	if u.Simhash != 0 {
		if n, err := c.rdb.SCard(ctx, simKey).Result(); err == nil && n > riskSimSetMaxMembers {
			_ = c.rdb.SPopN(ctx, simKey, n-riskSimSetMaxMembers).Err()
		}
	}
}

// ReadSketch 读取某用户草图聚合（评分 worker 用）。
func (c *riskSketchCache) ReadSketch(ctx context.Context, userID int64) (service.RiskSketchSnapshot, error) {
	var snap service.RiskSketchSnapshot
	if c == nil || c.rdb == nil || userID <= 0 {
		return snap, nil
	}
	ctx, cancel := context.WithTimeout(ctx, riskReadTimeout)
	defer cancel()

	simKey := riskKey(riskSimKeyPrefix, userID)
	turnsKey := riskKey(riskTurnsKeyPrefix, userID)
	ioKey := riskKey(riskIOKeyPrefix, userID)
	modelKey := riskKey(riskModelKeyPrefix, userID)
	countKey := riskKey(riskCountKeyPrefix, userID)

	pipe := c.rdb.Pipeline()
	distinctCmd := pipe.SCard(ctx, simKey)
	turnsCmd := pipe.HGetAll(ctx, turnsKey)
	ioCmd := pipe.HGetAll(ctx, ioKey)
	modelCmd := pipe.HGetAll(ctx, modelKey)
	countCmd := pipe.Get(ctx, countKey)
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return snap, err
	}

	snap.DistinctSim = int(distinctCmd.Val())

	turns := turnsCmd.Val()
	snap.TotalTurns = atoiSafe(turns["total"])
	snap.SingleTurn = atoiSafe(turns["single"])
	snap.TotalSim = atoiSafe(turns["sim_total"])

	io := ioCmd.Val()
	snap.InputTokens = atoi64Safe(io["input"])
	snap.OutputTokens = atoi64Safe(io["output"])

	models := modelCmd.Val()
	snap.ModelVariety = len(models)
	top := 0
	for _, v := range models {
		n := atoiSafe(v)
		if n > top {
			top = n
		}
	}
	snap.TopModelCount = top

	snap.RequestCount = atoiSafe(countCmd.Val())
	return snap, nil
}

// ActiveUsers 返回窗口内有活动的用户 ID。
func (c *riskSketchCache) ActiveUsers(ctx context.Context) ([]int64, error) {
	if c == nil || c.rdb == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, riskReadTimeout)
	defer cancel()

	members, err := c.rdb.SMembers(ctx, riskActiveSetKey).Result()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	ids := make([]int64, 0, len(members))
	for _, m := range members {
		if id, perr := strconv.ParseInt(m, 10, 64); perr == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// StoreScore 缓存评分结果到 risk:user:{uid}。
func (c *riskSketchCache) StoreScore(ctx context.Context, userID int64, payload []byte, ttl time.Duration) error {
	if c == nil || c.rdb == nil || userID <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, riskUpdateTimeout)
	defer cancel()
	return c.rdb.Set(ctx, riskKey(riskUserKeyPrefix, userID), payload, ttl).Err()
}

func atoiSafe(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

func atoi64Safe(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
