package repository

import (
	"context"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// 切片 4：Ingestion Health 的 Redis 实现。
//
//	riskv2:<schema>:fp:<fpVer>:health:<instanceID>:m:<minute>  HASH 分钟增量计数（TTL）
//	riskv2:<schema>:fp:<fpVer>:health:instances               ZSET instanceID→最后心跳 unix 秒（TTL）
//
// instanceID 是服务启动实例 ID（绝非 user/api_key/账号）；只写计数，绝不写原文/HMAC/请求 ID/秘密。

const (
	riskV2HealthMinuteTTL   = 15 * time.Minute
	riskV2HealthInstanceTTL = 15 * time.Minute
	// 聚合时，心跳早于 (cycleEnd - riskV2HealthStaleWindow) 的实例视为「当前未上报」（reporting 判定）。
	riskV2HealthStaleWindow = int64(120) // 秒
	// Expected Set 窗口：最近 riskV2HealthExpectedWindow 内心跳过的实例视为「已知应上报」。
	riskV2HealthExpectedWindow = int64(300) // 秒
)

type riskV2HealthRepo struct {
	rdb        *redis.Client
	schema     string
	fpVer      string
	instanceID string
	now        func() time.Time
}

// NewRiskV2HealthReporter 构造某实例的健康上报器。rdb/instanceID 无效 → nil。
func NewRiskV2HealthReporter(rdb *redis.Client, schema, fpVer, instanceID string, now func() time.Time) service.RiskV2HealthReporter {
	r := newRiskV2HealthRepo(rdb, schema, fpVer, instanceID, now)
	if r == nil {
		return nil
	}
	return r
}

// NewRiskV2IngestionHealthReader 构造 Worker 侧健康聚合器。rdb 无效 → nil。
func NewRiskV2IngestionHealthReader(rdb *redis.Client, schema, fpVer string, now func() time.Time) service.RiskV2IngestionHealthReader {
	r := newRiskV2HealthRepo(rdb, schema, fpVer, "", now)
	if r == nil {
		return nil
	}
	return r
}

func newRiskV2HealthRepo(rdb *redis.Client, schema, fpVer, instanceID string, now func() time.Time) *riskV2HealthRepo {
	if rdb == nil {
		return nil
	}
	if schema == "" {
		schema = service.RiskV2AggSchemaVersion
	}
	if fpVer == "" {
		fpVer = "v1"
	}
	if now == nil {
		now = time.Now
	}
	return &riskV2HealthRepo{rdb: rdb, schema: schema, fpVer: fpVer, instanceID: instanceID, now: now}
}

func (r *riskV2HealthRepo) base() string {
	return "riskv2:" + r.schema + ":fp:" + r.fpVer + ":health"
}
func (r *riskV2HealthRepo) instancesKey() string { return r.base() + ":instances" }

// minuteKey / seqKey 使用 instance 级 hash tag {<instanceID>}，保证「序号状态」与「分钟计数 hash」同 Slot，
// 幂等 Lua 可原子操作两者（Redis Cluster 安全）。registry ZADD 独立、无需同 Slot。
func (r *riskV2HealthRepo) minuteKey(instanceID string, minute int64) string {
	return r.base() + ":{" + instanceID + "}:m:" + strconv.FormatInt(minute, 10)
}
func (r *riskV2HealthRepo) seqKey(instanceID string) string {
	return r.base() + ":{" + instanceID + "}:seq"
}

// riskV2HealthReportSrc：按单调序号幂等累加。KEYS=[seqKey, minuteKey]；
// ARGV=[seq, ttlSec, enqueued, processed, dropped, panics, aggErrors]。
// last_applied_sequence >= seq → 返回 0（ALREADY_APPLIED，不重复累加）；否则累加 + 保存序号 + TTL，返回 1（APPLIED）。
const riskV2HealthReportSrc = `
local last = tonumber(redis.call('GET', KEYS[1]) or '0')
if tonumber(ARGV[1]) <= last then
  return 0
end
redis.call('HINCRBY', KEYS[2], 'enqueued', ARGV[3])
redis.call('HINCRBY', KEYS[2], 'processed', ARGV[4])
redis.call('HINCRBY', KEYS[2], 'dropped', ARGV[5])
redis.call('HINCRBY', KEYS[2], 'panics', ARGV[6])
redis.call('HINCRBY', KEYS[2], 'agg_errors', ARGV[7])
redis.call('SET', KEYS[1], ARGV[1])
redis.call('EXPIRE', KEYS[1], ARGV[2])
redis.call('EXPIRE', KEYS[2], ARGV[2])
return 1
`

// Report 按序号幂等写入某实例当前分钟的增量，并刷新心跳（registry ZADD，独立幂等）。
func (r *riskV2HealthRepo) Report(ctx context.Context, sequence uint64, d service.RiskV2HealthDelta) (service.RiskV2HealthReportResult, error) {
	if r == nil || r.rdb == nil || r.instanceID == "" {
		return service.RiskV2HealthApplied, nil
	}
	now := r.now()
	minute := now.Unix() / 60
	ttl := int64(riskV2HealthMinuteTTL.Seconds())
	res, err := r.rdb.Eval(ctx, riskV2HealthReportSrc,
		[]string{r.seqKey(r.instanceID), r.minuteKey(r.instanceID, minute)},
		strconv.FormatUint(sequence, 10), ttl,
		d.Enqueued, d.Processed, d.Dropped, d.Panics, d.AggErrors,
	).Int64()
	if err != nil && err != redis.Nil {
		return service.RiskV2HealthApplied, err
	}
	// 心跳（registry）独立更新，不与计数同 Slot 强绑定。
	hbPipe := r.rdb.Pipeline()
	hbPipe.ZAdd(ctx, r.instancesKey(), redis.Z{Score: float64(now.Unix()), Member: r.instanceID})
	hbPipe.Expire(ctx, r.instancesKey(), riskV2HealthInstanceTTL)
	if _, herr := hbPipe.Exec(ctx); herr != nil && herr != redis.Nil {
		return service.RiskV2HealthApplied, herr
	}
	if res == 0 {
		return service.RiskV2HealthAlreadyApplied, nil
	}
	return service.RiskV2HealthApplied, nil
}

// Deregister 优雅缩容：从 Registry ZSET 移除本实例。
func (r *riskV2HealthRepo) Deregister(ctx context.Context) error {
	if r == nil || r.rdb == nil || r.instanceID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return r.rdb.ZRem(ctx, r.instancesKey(), r.instanceID).Err()
}

// ReadIngestionHealth 聚合窗口 [cycleEnd-window, cycleEnd) 内所有活跃实例的健康，并计算覆盖率。
// Expected = 最近 expectedWindow 内心跳过的实例；Reporting = 当前仍新鲜（>= cycleEnd-staleWindow）的实例。
// 有实例缺报 / 覆盖不完整 / Registry 读失败 / 无观测证据 → HealthAvailable=false（绝不假设健康）。
func (r *riskV2HealthRepo) ReadIngestionHealth(ctx context.Context, cycleEnd int64, windowMinutes int) (service.RiskV2IngestionHealth, error) {
	var h service.RiskV2IngestionHealth // 默认全零：HealthAvailable=false（不假设健康）
	if r == nil || r.rdb == nil {
		return h, nil
	}
	if windowMinutes <= 0 {
		windowMinutes = 5
	}
	ctx, cancel := context.WithTimeout(ctx, riskV2AggReadTimeout)
	defer cancel()

	// Expected Set：最近 expectedWindow 内心跳过的实例（已知应上报）。
	expected, eerr := r.rdb.ZRangeByScore(ctx, r.instancesKey(), &redis.ZRangeBy{
		Min: strconv.FormatInt(cycleEnd-riskV2HealthExpectedWindow, 10), Max: "+inf",
	}).Result()
	if eerr != nil && eerr != redis.Nil {
		return h, eerr // Registry 读失败 → HealthAvailable=false
	}
	// Reporting Set：当前仍新鲜的实例。
	minScore := strconv.FormatInt(cycleEnd-riskV2HealthStaleWindow, 10)
	instances, err := r.rdb.ZRangeByScore(ctx, r.instancesKey(), &redis.ZRangeBy{
		Min: minScore, Max: "+inf",
	}).Result()
	if err != nil && err != redis.Nil {
		return h, err // 读失败 → HealthAvailable 保持 false
	}
	// 覆盖率。
	h.ExpectedInstanceCount = len(expected)
	h.ReportingInstanceCount = len(instances)
	h.MissingInstanceCount = len(expected) - len(instances)
	if h.MissingInstanceCount < 0 {
		h.MissingInstanceCount = 0
	}
	h.CoverageAvailable = len(expected) > 0
	if len(expected) > 0 {
		h.HealthCoverageRatio = float64(len(instances)) / float64(len(expected))
	}
	if len(instances) == 0 {
		return h, nil // 无活跃实例 → 健康未知
	}
	// 覆盖不完整（已知实例缺报）→ 不声称全局健康。
	if h.MissingInstanceCount > 0 || !h.CoverageAvailable {
		return h, nil // HealthAvailable 保持 false
	}

	// 窗口分钟集合：cycleEnd 所在分钟往前 windowMinutes 个。
	endMinute := cycleEnd / 60
	pipe := r.rdb.Pipeline()
	type key struct {
		inst string
		cmd  *redis.MapStringStringCmd
	}
	var cmds []key
	for _, inst := range instances {
		for i := 0; i < windowMinutes; i++ {
			cmds = append(cmds, key{inst: inst, cmd: pipe.HGetAll(ctx, r.minuteKey(inst, endMinute-int64(i)))})
		}
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return h, err
	}

	var enqueued, processed, dropped, panics, aggErrors int64
	for _, k := range cmds {
		m := k.cmd.Val()
		enqueued += atoi64Safe(m["enqueued"])
		processed += atoi64Safe(m["processed"])
		dropped += atoi64Safe(m["dropped"])
		panics += atoi64Safe(m["panics"])
		aggErrors += atoi64Safe(m["agg_errors"])
	}

	// 无任何观测证据 → 不能确认健康 → HealthAvailable=false（保守，阻止 HIGH）。
	if enqueued == 0 && processed == 0 {
		return h, nil
	}

	h.HealthAvailable = true
	h.AggregationErrorObserved = aggErrors > 0
	h.DispatcherDegraded = panics > 0
	if enqueued > 0 {
		h.ObservationDropRatioAvailable = true
		h.ObservationDropRatio = float64(dropped) / float64(enqueued)
		if h.ObservationDropRatio < 0 {
			h.ObservationDropRatio = 0
		}
		if h.ObservationDropRatio > 1 {
			h.ObservationDropRatio = 1
		}
	}
	// 聚合健康：无 agg 错误、无 panic、丢弃比率不高。
	h.AggregationHealthy = !h.AggregationErrorObserved && !h.DispatcherDegraded &&
		(!h.ObservationDropRatioAvailable || h.ObservationDropRatio <= 0.05)
	return h, nil
}
