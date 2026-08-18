package repository

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// Model Extraction Risk V2 Shadow — 切片 1/1.1：多时间窗口聚合的 Redis 实现（独立命名空间 riskv2:*）。
//
// Key 用户级 hash tag {u:<uid>} 保证同一用户及其 API Key 同 slot（cluster 安全）；含 fpVer=观测 epoch：
//
//	riskv2:s1:{u:<uid>}:fp:<fpVer>:m|h:<ts>:c            HASH 计数
//	riskv2:s1:{u:<uid>}:fp:<fpVer>:m|h:<ts>:xe|xp|scf|scs SET distinct(exact/exact+paramSig/full·sampled scaffold)
//	riskv2:s1:{u:<uid>}:fp:<fpVer>:m|h:<ts>:mc          HASH model→count（bounded + other）
//	riskv2:s1:{u:<uid>}:fp:<fpVer>:m|h:<ts>:ak          SET api_key（bounded）
//	riskv2:s1:{u:<uid>}:fp:<fpVer>:h:<hts>:bm           STRING 小时 60-bit active-minute bitmap
//	riskv2:s1:{u:<uid>}:fp:<fpVer>:k:<kid>:m|h:...       每 API Key（同 slot）
//	riskv2:s1:fp:<fpVer>:active:h:<hts>                  全局活跃用户集合（单独 slot，有 TTL，含 epoch）
//
// distinct/model/ak 的写入用 Lua 原子脚本（同 slot），保证并发下：已存在识别、达上限不写新成员且置 overflow、
// 绝不随机 SPOP。计数用 HINCRBY。跨桶 distinct 用客户端有界并集。只用全版本命令 + Lua（miniredis 支持）。

const (
	riskV2AggUpdateTimeout = 500 * time.Millisecond
	riskV2AggReadTimeout   = 3 * time.Second

	riskV2MinuteBucketTTL = 2 * time.Hour
	riskV2HourBucketTTL   = 30 * time.Hour

	riskV2Window5mMinutes = 5
	riskV2Window1hMinutes = 60
	riskV2Window24hHours  = 24
	riskV2ActiveSetTTL    = 26 * time.Hour

	riskV2AggSetMax      = 2000 // distinct SET 成员上限（不淘汰,超则 overflow）
	riskV2APIKeySetMax   = 512  // ak SET 成员上限（写侧硬上限）
	riskV2ModelHashMax   = 32   // model 直方图基数上限（超出归 other）
	riskV2ShortOutputMax = 64
	riskV2LongOutputMin  = 2000

	riskV2MaxAPIKeysPerRead    = 64   // 读窗口每用户最多展开的 api_key 数
	riskV2MaxUnionMembers      = 8000 // 客户端跨桶并集上限
	riskV2MaxMembersPerReadKey = 4096 // 单次读单个 Set 最多纳入并集的成员数
)

// boundedSAddSrc：原子有界集合准入。KEYS=[set,counterHash]；ARGV=[member,cap,ttlSec,ovField,ovCountField]。
// 返回 1=已存在,0=新增,-1=达上限拒绝（置 overflow + overflow_count）。绝不淘汰已有成员。
const boundedSAddSrc = `
if redis.call('SISMEMBER', KEYS[1], ARGV[1]) == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[3])
  return 1
end
if tonumber(redis.call('SCARD', KEYS[1])) >= tonumber(ARGV[2]) then
  redis.call('HSET', KEYS[2], ARGV[4], 1)
  redis.call('HINCRBY', KEYS[2], ARGV[5], 1)
  redis.call('EXPIRE', KEYS[2], ARGV[3])
  return -1
end
redis.call('SADD', KEYS[1], ARGV[1])
redis.call('EXPIRE', KEYS[1], ARGV[3])
return 0
`

// boundedHIncrSrc：原子有界直方图。KEYS=[hash]；ARGV=[field,cap,ttlSec]。超上限归入 'other'。
const boundedHIncrSrc = `
if redis.call('HEXISTS', KEYS[1], ARGV[1]) == 1 then
  redis.call('HINCRBY', KEYS[1], ARGV[1], 1)
elseif tonumber(redis.call('HLEN', KEYS[1])) >= tonumber(ARGV[2]) then
  redis.call('HINCRBY', KEYS[1], 'other', 1)
else
  redis.call('HINCRBY', KEYS[1], ARGV[1], 1)
end
redis.call('EXPIRE', KEYS[1], ARGV[3])
return 1
`

type riskV2AggCache struct {
	rdb    *redis.Client
	schema string
	fpVer  string
	now    func() time.Time

	// 切片 4：周期活跃用户索引（ZSET active:c:<cycleEnd>）。cycleInterval<=0 关闭该写入。
	cycleInterval  time.Duration
	activeCycleTTL time.Duration
}

// SetScoringCycle 配置周期活跃索引的周期与 TTL（供 Worker 侧与聚合写入侧对齐）。
// interval 必须与 Scoring Worker 的 interval 一致；ttl 至少覆盖 max_catchup_cycles 个周期。
func (c *riskV2AggCache) SetScoringCycle(interval, ttl time.Duration) {
	if c == nil {
		return
	}
	c.cycleInterval = interval
	c.activeCycleTTL = ttl
}

// NewRiskV2AggCache 构造 V2 聚合缓存。rdb 为 nil → 返回 nil（上层回退到计数 sink）。
func NewRiskV2AggCache(rdb *redis.Client, schema, fpVer string) service.RiskV2AggCache {
	c := newRiskV2AggCacheWithClock(rdb, schema, fpVer, time.Now)
	if c == nil {
		return nil
	}
	return c
}

// NewRiskV2AggCacheWithCycle 构造聚合缓存并对齐周期活跃索引（active:c）的 interval/TTL，
// 使写侧周期归属与 Scoring Worker 的 interval 一致。rdb 为 nil → nil。
func NewRiskV2AggCacheWithCycle(rdb *redis.Client, schema, fpVer string, interval, cycleTTL time.Duration) service.RiskV2AggCache {
	c := newRiskV2AggCacheWithClock(rdb, schema, fpVer, time.Now)
	if c == nil {
		return nil
	}
	if interval > 0 {
		c.SetScoringCycle(interval, cycleTTL)
	}
	return c
}

func newRiskV2AggCacheWithClock(rdb *redis.Client, schema, fpVer string, now func() time.Time) *riskV2AggCache {
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
	return &riskV2AggCache{
		rdb: rdb, schema: schema, fpVer: fpVer, now: now,
		cycleInterval:  5 * time.Minute, // 默认与 Worker 默认 interval 对齐；可用 SetScoringCycle 覆盖
		activeCycleTTL: 2 * time.Hour,
	}
}

// riskV2CycleEnd 计算某观测（按其进入聚合系统的时间 t）应归属的评分周期结束点：
// cycleEnd = ceil(t / interval) * interval（下一个已完成周期）。
func riskV2CycleEnd(unixSec, intervalSec int64) int64 {
	if intervalSec <= 0 {
		return unixSec
	}
	return ((unixSec + intervalSec - 1) / intervalSec) * intervalSec
}

func (c *riskV2AggCache) userBase(uid int64) string {
	return "riskv2:" + c.schema + ":{u:" + strconv.FormatInt(uid, 10) + "}:fp:" + c.fpVer
}
func (c *riskV2AggCache) keyBase(uid, kid int64) string {
	return c.userBase(uid) + ":k:" + strconv.FormatInt(kid, 10)
}

type riskV2Obs struct {
	success, terminalErr, cancelled      int
	usageAvailable                       int
	inputTok, outputTok                  int64
	shortOut, longOut                    int
	exactAvail                           int
	fullScaffoldReq, sampledScaffoldReq  int
	truncated                            int
	toolUse, structured, multiTurn       int
	cacheApplicable, cacheAvailable      int
	cacheObsIn, cacheCreation, cacheRead int64
	model                                string

	exactMember, paramMember, fullScaffoldMember, sampledScaffoldMember string
}

func classifyRiskV2Obs(env service.RiskFeatureEnvelope) riskV2Obs {
	o := riskV2Obs{}
	switch {
	case env.TerminalStatus == "ok":
		o.success = 1
	case containsCancel(env.TerminalStatus):
		o.cancelled = 1
	default:
		o.terminalErr = 1
	}
	if env.UsageAvailable {
		o.usageAvailable = 1
		o.inputTok = env.InputTokens
		o.outputTok = env.OutputTokens
		if env.OutputTokens <= int64(riskV2ShortOutputMax) {
			o.shortOut = 1
		}
		if env.OutputTokens >= int64(riskV2LongOutputMin) {
			o.longOut = 1
		}
	}
	if env.InputTruncated {
		o.truncated = 1
	}
	if env.ToolsPresent {
		o.toolUse = 1
	}
	if env.ResponseFormatType != "" {
		o.structured = 1
	}
	if env.HistoryCount > 0 {
		o.multiTurn = 1
	}
	if env.CacheUsageApplicable {
		o.cacheApplicable = 1
		if env.CacheUsageAvailable {
			o.cacheAvailable = 1
			o.cacheObsIn = env.InputTokens // 仅 applicable&available 请求的普通 input tokens（cache 分母用）
			if env.CacheCreationTokens != nil {
				o.cacheCreation = *env.CacheCreationTokens
			}
			if env.CacheReadTokens != nil {
				o.cacheRead = *env.CacheReadTokens
			}
		}
	}
	if env.ExactFingerprintAvailable && env.ExactHMAC != "" {
		o.exactAvail = 1
		o.exactMember = env.ExactHMAC
		o.paramMember = env.ExactHMAC + "|" + riskV2ParamSig(env)
	}
	if env.ScaffoldHMAC != "" {
		if env.ScaffoldFingerprintSampled {
			o.sampledScaffoldReq = 1
			o.sampledScaffoldMember = env.ScaffoldHMAC
		} else {
			o.fullScaffoldReq = 1
			o.fullScaffoldMember = env.ScaffoldHMAC
		}
	}
	o.model = canonicalModel(env.Model)
	return o
}

// canonicalModel 归一化模型名（lower/trim）；空 → ""（不入直方图）。高基数由 boundedHIncr 归 other。
func canonicalModel(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if len(m) > 64 {
		m = m[:64]
	}
	return m
}

func containsCancel(s string) bool {
	return strings.Contains(s, "cancel") || s == "terminal_status_499"
}

func riskV2ParamSig(env service.RiskFeatureEnvelope) string {
	temp := "na"
	if env.Temperature != nil {
		temp = strconv.FormatFloat(*env.Temperature, 'f', 3, 64)
	}
	top := "na"
	if env.TopP != nil {
		top = strconv.FormatFloat(*env.TopP, 'f', 3, 64)
	}
	seed := "0"
	if env.HasSeed {
		seed = "1"
	}
	return "t=" + temp + ";p=" + top + ";m=" + strconv.Itoa(env.MaxTokens) + ";s=" + seed + ";rf=" + env.ResponseFormatType
}

// Aggregate 把观测合入用户/每 Key 的分钟与小时桶。计数 HINCRBY + distinct/model/ak 原子 Lua，
// 均在用户 slot 单 pipeline（1 RTT）；全局 active-set 另 1 RTT。返回错误由 sink 计数（不重试→不重复计数）。
func (c *riskV2AggCache) Aggregate(ctx context.Context, env service.RiskFeatureEnvelope) error {
	if c == nil || c.rdb == nil || env.UserID <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, riskV2AggUpdateTimeout)
	defer cancel()

	now := c.now()
	mts := strconv.FormatInt(now.Unix()/60, 10)
	hts := strconv.FormatInt(now.Unix()/3600, 10)
	minuteOfHour := int64(now.Minute())
	o := classifyRiskV2Obs(env)

	ub := c.userBase(env.UserID)
	kb := c.keyBase(env.UserID, env.APIKeyID)
	type eb struct {
		base   string
		bucket string
		ttlSec int
		isUser bool
		isHour bool
	}
	ebs := []eb{
		{ub, ":m:" + mts, int(riskV2MinuteBucketTTL.Seconds()), true, false},
		{ub, ":h:" + hts, int(riskV2HourBucketTTL.Seconds()), true, true},
		{kb, ":m:" + mts, int(riskV2MinuteBucketTTL.Seconds()), false, false},
		{kb, ":h:" + hts, int(riskV2HourBucketTTL.Seconds()), false, true},
	}

	pipe := c.rdb.Pipeline()
	sadd := func(setKey, counterHash, member string, capN, ttlSec int, ovField, ovCountField string) {
		if member == "" {
			return
		}
		pipe.Eval(ctx, boundedSAddSrc, []string{setKey, counterHash}, member, capN, ttlSec, ovField, ovCountField)
	}
	for _, e := range ebs {
		cbase := e.base + e.bucket + ":c"
		ttl := time.Duration(e.ttlSec) * time.Second
		pipe.HIncrBy(ctx, cbase, "request", 1)
		hinc := func(field string, v int64) {
			if v != 0 {
				pipe.HIncrBy(ctx, cbase, field, v)
			}
		}
		hinc("success", int64(o.success))
		hinc("terminal_err", int64(o.terminalErr))
		hinc("cancelled", int64(o.cancelled))
		hinc("usage_avail", int64(o.usageAvailable))
		hinc("in_tok", o.inputTok)
		hinc("out_tok", o.outputTok)
		hinc("short_out", int64(o.shortOut))
		hinc("long_out", int64(o.longOut))
		hinc("exact_avail", int64(o.exactAvail))
		hinc("full_scaffold_req", int64(o.fullScaffoldReq))
		hinc("sampled_scaffold_req", int64(o.sampledScaffoldReq))
		hinc("truncated", int64(o.truncated))
		hinc("tool_use", int64(o.toolUse))
		hinc("structured", int64(o.structured))
		hinc("multi_turn", int64(o.multiTurn))
		hinc("cache_applicable", int64(o.cacheApplicable))
		hinc("cache_available", int64(o.cacheAvailable))
		hinc("cache_obs_in", o.cacheObsIn)
		hinc("cache_creation", o.cacheCreation)
		hinc("cache_read", o.cacheRead)
		pipe.Expire(ctx, cbase, ttl)

		sadd(e.base+e.bucket+":xe", cbase, o.exactMember, riskV2AggSetMax, e.ttlSec, "ov_xe", "ovc_xe")
		sadd(e.base+e.bucket+":xp", cbase, o.paramMember, riskV2AggSetMax, e.ttlSec, "ov_xp", "ovc_xp")
		sadd(e.base+e.bucket+":scf", cbase, o.fullScaffoldMember, riskV2AggSetMax, e.ttlSec, "ov_scf", "ovc_scf")
		sadd(e.base+e.bucket+":scs", cbase, o.sampledScaffoldMember, riskV2AggSetMax, e.ttlSec, "ov_scs", "ovc_scs")
		if o.model != "" {
			pipe.Eval(ctx, boundedHIncrSrc, []string{e.base + e.bucket + ":mc"}, o.model, riskV2ModelHashMax, e.ttlSec)
		}
		if e.isUser {
			if env.APIKeyID > 0 {
				sadd(e.base+e.bucket+":ak", cbase, strconv.FormatInt(env.APIKeyID, 10), riskV2APIKeySetMax, e.ttlSec, "ov_ak", "ovc_ak")
				// 用户级紧凑 cross-key「完整 scaffold @ api_key」摘要（仅分钟桶，供 ReadForScoring 亚线性读取
				// cross-key overlap，避免展开每个 key 的窗口）。仅完整 scaffold；sampled 不入；内部 ID，不含原文。
				if !e.isHour && o.fullScaffoldMember != "" {
					sadd(e.base+e.bucket+":xkscf", cbase,
						o.fullScaffoldMember+"@"+strconv.FormatInt(env.APIKeyID, 10),
						riskV2AggSetMax, e.ttlSec, "ov_xkscf", "ovc_xkscf")
				}
			}
			if e.isHour {
				bmKey := e.base + e.bucket + ":bm"
				pipe.SetBit(ctx, bmKey, minuteOfHour, 1)
				pipe.Expire(ctx, bmKey, ttl)
			}
		}
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return err
	}

	// 全局活跃用户集合（含 epoch，单独 slot，有 TTL）。best-effort。
	activeKey := "riskv2:" + c.schema + ":fp:" + c.fpVer + ":active:h:" + hts
	pipeC := c.rdb.Pipeline()
	pipeC.SAdd(ctx, activeKey, env.UserID)
	pipeC.Expire(ctx, activeKey, riskV2ActiveSetTTL)
	// 切片 4：周期活跃用户索引（ZSET，稳定 lex 分页）。归属「下一个已完成评分周期」，
	// 让在旧周期开始、晚到才聚合完成的长流请求不会永远漏评分。member=内部 user_id，无任何客户原文。
	if c.cycleInterval > 0 {
		cycleEnd := riskV2CycleEnd(now.Unix(), int64(c.cycleInterval.Seconds()))
		cKey := "riskv2:" + c.schema + ":fp:" + c.fpVer + ":active:c:" + strconv.FormatInt(cycleEnd, 10)
		pipeC.ZAdd(ctx, cKey, redis.Z{Score: 0, Member: strconv.FormatInt(env.UserID, 10)})
		pipeC.Expire(ctx, cKey, c.activeCycleTTL)
	}
	_, _ = pipeC.Exec(ctx)
	return nil
}
