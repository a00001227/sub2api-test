package repository

import (
	"context"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// 切片 3.5：评分专用读取路径。核心不变量——**绝不逐 API Key 展开窗口**，读取成本不随 key 数量线性增长。
//
// ReadForScoring 只读：
//   - 用户级三窗口（5m/1h/24h）：复用 readEntityWindows(ub, isUser=true)，全部命中用户级桶；
//   - 紧凑多 Key 汇总（scoringMultiKeyRollup）：只读用户级集合
//     · 24 个小时 ak 集合  → ActiveAPIKeyCount24h（有界并集）
//     · 60 个分钟 ak 集合  → SynchronizedMultiKeyMinutes1h / KeyHandoffCount1h
//     · 60 个分钟 xkscf 集合 → CrossKeyFullScaffoldOverlapEstimate1h（写侧维护的「完整 scaffold@apiKey」摘要）
//     合计 144 次 SMEMBERS，**与用户 API Key 数量无关**（1 key 与 64 key 基础命令数相同）。
//
// 绝不返回 PerAPIKey；RiskV2ScoringSnapshot 结构上也无该字段。绝不暴露 HMAC/SimHash/Set 成员/原文。

const (
	// 批量评分读取：单批硬上限（默认 200）与有界并发（避免无界 goroutine / 每用户独立连接）。
	riskV2ScoringBatchMax         = 200
	riskV2ScoringBatchConcurrency = 8
	// 活跃用户游标扫描：单次 SSCAN 返回硬上限。
	riskV2ActiveUserScanDefault = 100
	riskV2ActiveUserScanMax     = 1000
)

// ReadForScoring 读取单用户评分快照（用户级三窗口 + 紧凑多 Key 汇总，无 per-key 展开）。
func (c *riskV2AggCache) ReadForScoring(ctx context.Context, userID int64) (service.RiskV2ScoringSnapshot, error) {
	out := service.RiskV2ScoringSnapshot{
		UserID:                userID,
		SchemaVersion:         c.schema,
		FingerprintKeyVersion: c.fpVer,
		AssessedAtUnix:        c.now().Unix(),
	}
	if c == nil || c.rdb == nil || userID <= 0 {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, riskV2AggReadTimeout)
	defer cancel()

	now := c.now()
	minNow := now.Unix() / 60
	hrNow := now.Unix() / 3600
	ub := c.userBase(userID)

	hourBitmaps, bmErr := c.readHourBitmaps(ctx, ub, hrNow)
	if bmErr {
		out.Degraded = true
	}

	// 用户级三窗口（不读任何 per-key 桶）。
	user, e1 := c.readEntityWindows(ctx, ub, minNow, hrNow, hourBitmaps, true)
	out.User = user
	if e1 {
		out.Degraded = true
	}

	// 紧凑多 Key 汇总：只读用户级 ak / xkscf 集合，成本与 key 数量无关。
	mk, mkErr := c.scoringMultiKeyRollup(ctx, ub, minNow, hrNow)
	out.MultiKey = mk
	if mkErr {
		out.Degraded = true
	}

	// incomplete 汇总（无 per-key reason；只汇总用户窗口 + 多 Key）。
	reasons := []string{}
	addReason := func(cond bool, r string) {
		if cond {
			reasons = append(reasons, r)
		}
	}
	for _, w := range []service.RiskV2Window{out.User.W5m, out.User.W1h, out.User.W24h} {
		addReason(w.ExactIncomplete, "exact_incomplete:"+w.WindowLabel)
		addReason(w.FullScaffoldIncomplete, "full_scaffold_incomplete:"+w.WindowLabel)
		addReason(w.SampledScaffoldIncomplete, "sampled_scaffold_incomplete:"+w.WindowLabel)
	}
	addReason(out.MultiKey.APIKeyOverflow, "apikey_overflow")
	addReason(out.MultiKey.MultiKeyIncomplete, "multikey_incomplete")
	addReason(out.Degraded, "redis_degraded")
	out.IncompleteReasons = reasons
	out.Incomplete = len(reasons) > 0
	return out, nil
}

// scoringMultiKeyRollup 只读用户级集合完成多 Key 汇总，绝不逐 key 展开窗口。
// 一次 pipeline 读取 24 小时 ak + 60 分钟 ak + 60 分钟 xkscf（均同 slot，1 RTT），命令数与 key 数量无关。
func (c *riskV2AggCache) scoringMultiKeyRollup(ctx context.Context, ub string, minNow, hrNow int64) (service.RiskV2MultiKeyRollup, bool) {
	var r service.RiskV2MultiKeyRollup

	pipe := c.rdb.Pipeline()
	hourAk := make([]*redis.StringSliceCmd, riskV2Window24hHours)
	for i := 0; i < riskV2Window24hHours; i++ {
		hourAk[i] = pipe.SMembers(ctx, ub+":h:"+strconv.FormatInt(hrNow-int64(i), 10)+":ak")
	}
	minAk := make([]*redis.StringSliceCmd, riskV2Window1hMinutes)
	minXkscf := make([]*redis.StringSliceCmd, riskV2Window1hMinutes)
	for i := 0; i < riskV2Window1hMinutes; i++ {
		mts := strconv.FormatInt(minNow-int64(i), 10)
		minAk[i] = pipe.SMembers(ctx, ub+":m:"+mts+":ak")
		minXkscf[i] = pipe.SMembers(ctx, ub+":m:"+mts+":xkscf")
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		r.MultiKeyIncomplete = true
		return r, true
	}

	// ActiveAPIKeyCount24h：24 小时 ak 有界并集。
	akUnion := newBoundedUnion()
	for _, cmd := range hourAk {
		akUnion.addAll(cmd.Val())
	}
	r.ActiveAPIKeyCount24h = akUnion.count
	if akUnion.overflow {
		r.APIKeyOverflow = true
		r.PerAPIKeyIncomplete = true
		r.MultiKeyIncomplete = true
	}
	if r.ActiveAPIKeyCount24h < 2 {
		r.MultiKeyAvailable = false // 单 Key（或无 key）：无多 Key 情形
		return r, false
	}
	r.MultiKeyAvailable = true

	// 同步分钟 + handoff：用「用户级分钟 ak 集合」= 该分钟活跃的 key 集合。O(60)，与 key 数量无关。
	sync := 0
	handoff := 0
	var prev map[int64]bool
	for i := 0; i < riskV2Window1hMinutes; i++ {
		cur := map[int64]bool{}
		for _, m := range minAk[i].Val() {
			if id, perr := strconv.ParseInt(m, 10, 64); perr == nil && id > 0 {
				cur[id] = true
			}
		}
		if len(cur) >= 2 {
			sync++
		}
		if prev != nil && len(cur) > 0 && len(prev) > 0 && disjointKeys(cur, prev) {
			handoff++
		}
		if len(cur) > 0 {
			prev = cur
		}
	}
	r.SynchronizedMultiKeyMinutes1h = sync
	r.KeyHandoffCount1h = handoff

	// Cross-key 完整 scaffold overlap（1h）：用写侧「完整 scaffold@apiKey」摘要，
	// 统计出现在 ≥2 个不同 api_key 下的完整 scaffold 数。有界（TTL + 容量），超界置 incomplete。
	overlap, ovAvail, ovIncomplete := crossKeyFullScaffoldOverlapFromXkscf(minXkscf)
	r.CrossKeyFullScaffoldOverlapEstimate1h = overlap
	r.CrossKeyFullScaffoldOverlapAvailable1h = ovAvail
	if ovIncomplete {
		r.MultiKeyIncomplete = true
	}
	return r, false
}

// crossKeyFullScaffoldOverlapFromXkscf 解析 xkscf 成员 `<scaffoldHMAC>@<apiKeyID>`，
// 统计被 ≥2 个不同 api_key 使用的完整 scaffold 数。绝不返回 HMAC/成员本身，只返回计数。
// 有界：hmac→keySet 映射总量达 riskV2MaxUnionMembers 即停止纳入并置 incomplete（估计为下界）。
func crossKeyFullScaffoldOverlapFromXkscf(minXkscf []*redis.StringSliceCmd) (overlap int, available, incomplete bool) {
	hmacKeys := map[string]map[int64]struct{}{}
	bounded := false
	for _, cmd := range minXkscf {
		for _, m := range cmd.Val() {
			at := strings.LastIndexByte(m, '@')
			if at <= 0 || at >= len(m)-1 {
				continue
			}
			hmac := m[:at]
			kid, perr := strconv.ParseInt(m[at+1:], 10, 64)
			if perr != nil || kid <= 0 {
				continue
			}
			set, ok := hmacKeys[hmac]
			if !ok {
				if len(hmacKeys) >= riskV2MaxUnionMembers {
					bounded = true
					continue // 有界：不再纳入新 scaffold
				}
				set = map[int64]struct{}{}
				hmacKeys[hmac] = set
			}
			set[kid] = struct{}{}
		}
	}
	for _, kids := range hmacKeys {
		if len(kids) >= 2 {
			overlap++
		}
	}
	// 读到即视为 available（overlap 可能为 0=无重叠，语义上仍是「可用且=0」）。
	return overlap, true, bounded
}

// ReadForScoringBatch 批量读取评分快照。分块（每块 ≤ riskV2ScoringBatchMax）+ 有界并发，
// 单用户失败以该项 Err 体现（不拖累整批）；context 取消/超时立即返回已完成部分 + ctx.Err。
// 每个用户的读取各自 pipeline（同 slot，hash tag 保证同用户 key 同 slot），不跨 slot 合并、不建每用户连接。
func (c *riskV2AggCache) ReadForScoringBatch(ctx context.Context, userIDs []int64) ([]service.RiskV2ScoringBatchItem, error) {
	items := make([]service.RiskV2ScoringBatchItem, len(userIDs))
	for i, uid := range userIDs {
		items[i] = service.RiskV2ScoringBatchItem{UserID: uid}
	}
	if c == nil || c.rdb == nil || len(userIDs) == 0 {
		return items, nil
	}

	type job struct{ idx int }
	for start := 0; start < len(userIDs); start += riskV2ScoringBatchMax {
		end := start + riskV2ScoringBatchMax
		if end > len(userIDs) {
			end = len(userIDs)
		}
		if err := ctx.Err(); err != nil {
			return items, err
		}
		jobs := make(chan job)
		done := make(chan struct{})
		workers := riskV2ScoringBatchConcurrency
		if n := end - start; workers > n {
			workers = n
		}
		for w := 0; w < workers; w++ {
			go func() {
				for j := range jobs {
					snap, err := c.ReadForScoring(ctx, userIDs[j.idx])
					items[j.idx].Snapshot = snap
					items[j.idx].Err = err
				}
				done <- struct{}{}
			}()
		}
	feed:
		for i := start; i < end; i++ {
			select {
			case <-ctx.Done():
				break feed
			case jobs <- job{idx: i}:
			}
		}
		close(jobs)
		for w := 0; w < workers; w++ {
			<-done
		}
		if err := ctx.Err(); err != nil {
			return items, err
		}
	}
	return items, nil
}

// ListActiveUserIDsForCycle 用周期活跃用户 ZSET（active:c:<cycleEnd>）+ 稳定 lex 游标分页列出活跃用户。
// 不整表加载：单次 ZRANGEBYLEX（COUNT=limit），limit 受硬上限约束；NextCursor=="" 表示遍历结束。
// 所有成员同 score(0)，lex 顺序稳定，配合 `(<lastMember>` 独占游标可保证不漏不重（周期为只读快照）。
func (c *riskV2AggCache) ListActiveUserIDsForCycle(ctx context.Context, cycleEnd int64, cursor string, limit int) (service.RiskV2ActiveUserPage, error) {
	var page service.RiskV2ActiveUserPage
	if c == nil || c.rdb == nil {
		return page, nil
	}
	if limit <= 0 {
		limit = riskV2ActiveUserScanDefault
	}
	if limit > riskV2ActiveUserScanMax {
		limit = riskV2ActiveUserScanMax
	}
	ctx, cancel := context.WithTimeout(ctx, riskV2AggReadTimeout)
	defer cancel()

	cKey := "riskv2:" + c.schema + ":fp:" + c.fpVer + ":active:c:" + strconv.FormatInt(cycleEnd, 10)
	min := "-"
	if cursor != "" {
		min = "(" + cursor // 独占：从上一页最后成员之后开始
	}
	members, err := c.rdb.ZRangeByLex(ctx, cKey, &redis.ZRangeBy{
		Min:   min,
		Max:   "+",
		Count: int64(limit),
	}).Result()
	if err != nil && err != redis.Nil {
		return page, err
	}
	for _, m := range members {
		if id, perr := strconv.ParseInt(m, 10, 64); perr == nil && id > 0 {
			page.UserIDs = append(page.UserIDs, id)
		}
	}
	// 满页 → 还有下一页，游标=最后成员；不满页 → 结束（NextCursor 留空）。
	if len(members) == limit && len(members) > 0 {
		page.NextCursor = members[len(members)-1]
	}
	return page, nil
}

// —— DI 构造：把读取面按用途拆成两个独立接口，供未来 Worker / Admin 分别注入 ——

// NewRiskV2ScoringReader 返回评分专用只读面（未来 Scoring Worker 只允许依赖它）。rdb 为 nil → nil。
func NewRiskV2ScoringReader(rdb *redis.Client, schema, fpVer string) service.RiskV2ScoringReader {
	c := newRiskV2AggCacheWithClock(rdb, schema, fpVer, nil)
	if c == nil {
		return nil
	}
	return c
}

// NewRiskV2AdminDetailReader 返回管理端单用户详情只读面（含 per-key 展开，仅 Admin 用）。rdb 为 nil → nil。
func NewRiskV2AdminDetailReader(rdb *redis.Client, schema, fpVer string) service.RiskV2AdminDetailReader {
	c := newRiskV2AggCacheWithClock(rdb, schema, fpVer, nil)
	if c == nil {
		return nil
	}
	return c
}

// NewRiskV2AdminSummaryReader 返回 Admin Live Window 专用的**轻量**用户级读取面（切片 5.1 §一）：
// 只暴露单用户 ReadForScoring（用户级三窗口 + 紧凑 Multi-Key，无 per-key 展开），不含 batch/list/Worker 能力。rdb 为 nil → nil。
func NewRiskV2AdminSummaryReader(rdb *redis.Client, schema, fpVer string) service.RiskV2AdminSummaryReader {
	c := newRiskV2AggCacheWithClock(rdb, schema, fpVer, nil)
	if c == nil {
		return nil
	}
	return c
}
