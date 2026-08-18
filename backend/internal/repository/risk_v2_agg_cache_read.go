package repository

import (
	"context"
	"math/bits"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// ReadForAdminDetail 读取某用户的多窗口完整契约（**仅供管理端单用户详情**，含有界 PerAPIKey 展开）。
// 跨桶 distinct 用客户端有界并集，active minutes 用小时 bitmap popcount，peak_rpm 由每分钟计数取 max（读时，无存储 max）。
// 读路径有界：max_api_keys_per_read / max_union_members / max_members_per_read_key，达上限置 incomplete。
// 成本随用户 API Key 数量增长（O(keys)），故评分 Worker 绝不调用本方法，只用 ReadForScoring。
func (c *riskV2AggCache) ReadForAdminDetail(ctx context.Context, userID int64) (service.RiskV2WindowMetrics, error) {
	out := service.RiskV2WindowMetrics{
		UserID:                userID,
		SchemaVersion:         c.schema,
		FingerprintKeyVersion: c.fpVer,
		AssessedAtUnix:        c.now().Unix(),
		PerAPIKey:             map[int64]service.RiskV2EntityWindows{},
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

	user, e1 := c.readEntityWindows(ctx, ub, minNow, hrNow, hourBitmaps, true)
	out.User = user
	if e1 {
		out.Degraded = true
	}

	keyIDs, apiKeyOverflow := c.discoverAPIKeys(ctx, ub, hrNow)
	for _, kid := range keyIDs {
		kw, ke := c.readEntityWindows(ctx, c.keyBase(userID, kid), minNow, hrNow, hourBitmaps, false)
		out.PerAPIKey[kid] = kw
		if ke {
			out.Degraded = true
		}
	}

	out.MultiKey = c.multiKeyRollup(ctx, userID, keyIDs, apiKeyOverflow, minNow, hrNow)

	// incomplete 汇总。
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

func (c *riskV2AggCache) readHourBitmaps(ctx context.Context, ub string, hrNow int64) (map[int64][]byte, bool) {
	pipe := c.rdb.Pipeline()
	cmds := make([]*redis.StringCmd, riskV2Window24hHours)
	for i := 0; i < riskV2Window24hHours; i++ {
		cmds[i] = pipe.Get(ctx, ub+":h:"+strconv.FormatInt(hrNow-int64(i), 10)+":bm")
	}
	hadErr := false
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		hadErr = true
	}
	out := map[int64][]byte{}
	for i := 0; i < riskV2Window24hHours; i++ {
		if b, err := cmds[i].Bytes(); err == nil {
			out[hrNow-int64(i)] = b
		}
	}
	return out, hadErr
}

// discoverAPIKeys 取 24h 小时 ak 集合并集（确定性：小时新→旧、成员字典序），有界。返回 (keys, overflow)。
func (c *riskV2AggCache) discoverAPIKeys(ctx context.Context, ub string, hrNow int64) ([]int64, bool) {
	seen := map[int64]struct{}{}
	var ids []int64
	pipe := c.rdb.Pipeline()
	cmds := make([]*redis.StringSliceCmd, riskV2Window24hHours)
	for i := 0; i < riskV2Window24hHours; i++ {
		cmds[i] = pipe.SMembers(ctx, ub+":h:"+strconv.FormatInt(hrNow-int64(i), 10)+":ak")
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return ids, false
	}
	for _, cmd := range cmds {
		for _, m := range cmd.Val() {
			id, perr := strconv.ParseInt(m, 10, 64)
			if perr != nil || id <= 0 {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
			if len(ids) >= riskV2MaxAPIKeysPerRead {
				return ids, true // overflow：达读取上限，不再展开
			}
		}
	}
	return ids, false
}

func (c *riskV2AggCache) readEntityWindows(ctx context.Context, base string, minNow, hrNow int64, hourBitmaps map[int64][]byte, isUser bool) (service.RiskV2EntityWindows, bool) {
	var ew service.RiskV2EntityWindows
	w5, e5 := c.readWindow(ctx, "5m", minuteRange(base, minNow, riskV2Window5mMinutes), false)
	w1, e1 := c.readWindow(ctx, "1h", minuteRange(base, minNow, riskV2Window1hMinutes), false)
	w24, e24 := c.readWindow(ctx, "24h", hourRange(base, hrNow, riskV2Window24hHours), true)

	w5.ActiveMinutes = activeMinutesForMinutes(minNow, riskV2Window5mMinutes, hourBitmaps)
	w1.ActiveMinutes = activeMinutesForMinutes(minNow, riskV2Window1hMinutes, hourBitmaps)
	w24.ActiveMinutes = activeMinutesForHours(hrNow, riskV2Window24hHours, hourBitmaps)
	w5.Available.ActiveMinutes, w1.Available.ActiveMinutes, w24.Available.ActiveMinutes = true, true, true

	w5.RequestsPerMinute = float64(w5.RequestCount) / float64(riskV2Window5mMinutes)
	w1.RequestsPerMinute = float64(w1.RequestCount) / float64(riskV2Window1hMinutes)
	w24.RequestsPerMinute = float64(w24.RequestCount) / float64(riskV2Window24hHours*60)

	if isUser {
		w5.UniqueAPIKeyCount = c.uniqueAPIKeys(ctx, base, minNow, riskV2Window5mMinutes, false, hrNow, 0)
		w1.UniqueAPIKeyCount = c.uniqueAPIKeys(ctx, base, minNow, riskV2Window1hMinutes, false, hrNow, 0)
		w24.UniqueAPIKeyCount = c.uniqueAPIKeys(ctx, base, 0, 0, true, hrNow, riskV2Window24hHours)
	}
	ew.W5m, ew.W1h, ew.W24h = w5, w1, w24
	return ew, e5 || e1 || e24
}

func minuteRange(base string, minNow int64, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, base+":m:"+strconv.FormatInt(minNow-int64(i), 10))
	}
	return out
}
func hourRange(base string, hrNow int64, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, base+":h:"+strconv.FormatInt(hrNow-int64(i), 10))
	}
	return out
}

func (c *riskV2AggCache) readWindow(ctx context.Context, label string, bucketKeys []string, isHour bool) (service.RiskV2Window, bool) {
	w := service.RiskV2Window{WindowLabel: label, NearDuplicateAvailable: false}
	pipe := c.rdb.Pipeline()
	type cmds struct {
		c                       *redis.MapStringStringCmd
		xe, xp, scf, scs, model *redis.StringSliceCmd
		mc                      *redis.MapStringStringCmd
	}
	cs := make([]cmds, len(bucketKeys))
	for i, bk := range bucketKeys {
		cs[i] = cmds{
			c:   pipe.HGetAll(ctx, bk+":c"),
			xe:  pipe.SMembers(ctx, bk+":xe"),
			xp:  pipe.SMembers(ctx, bk+":xp"),
			scf: pipe.SMembers(ctx, bk+":scf"),
			scs: pipe.SMembers(ctx, bk+":scs"),
			mc:  pipe.HGetAll(ctx, bk+":mc"),
		}
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return w, true
	}

	xe, xp, scf, scs := newBoundedUnion(), newBoundedUnion(), newBoundedUnion(), newBoundedUnion()
	models := map[string]int64{}
	peak := 0
	reqPerBucket := make([]int64, len(cs))
	for i := range cs {
		m := cs[i].c.Val()
		req := atoi64Safe(m["request"])
		reqPerBucket[i] = req
		w.RequestCount += req
		w.SuccessCount += atoi64Safe(m["success"])
		w.TerminalErrorCount += atoi64Safe(m["terminal_err"])
		w.CancelledCount += atoi64Safe(m["cancelled"])
		w.UsageAvailableCount += atoi64Safe(m["usage_avail"])
		w.InputTokens += atoi64Safe(m["in_tok"])
		w.OutputTokens += atoi64Safe(m["out_tok"])
		w.ShortOutputCount += atoi64Safe(m["short_out"])
		w.LongOutputCount += atoi64Safe(m["long_out"])
		w.ExactAvailableCount += atoi64Safe(m["exact_avail"])
		w.FullScaffoldRequestCount += atoi64Safe(m["full_scaffold_req"])
		w.SampledScaffoldRequestCount += atoi64Safe(m["sampled_scaffold_req"])
		w.TruncatedInputCount += atoi64Safe(m["truncated"])
		w.ToolUseRequestCount += atoi64Safe(m["tool_use"])
		w.StructuredOutputCount += atoi64Safe(m["structured"])
		w.MultiTurnCount += atoi64Safe(m["multi_turn"])
		w.CacheApplicableCount += atoi64Safe(m["cache_applicable"])
		w.CacheAvailableCount += atoi64Safe(m["cache_available"])
		w.CacheObservedInputTokens += atoi64Safe(m["cache_obs_in"])
		w.CacheCreationInputTokens += atoi64Safe(m["cache_creation"])
		w.CacheReadInputTokens += atoi64Safe(m["cache_read"])
		if m["ov_xe"] != "" {
			w.ExactOverflow = true
		}
		if m["ov_xp"] != "" {
			w.ExactParamSigOverflow = true
		}
		if m["ov_scf"] != "" {
			w.FullScaffoldOverflow = true
		}
		if m["ov_scs"] != "" {
			w.SampledScaffoldOverflow = true
		}
		if !isHour && req > int64(peak) {
			peak = int(req)
		}
		xe.addAll(cs[i].xe.Val())
		xp.addAll(cs[i].xp.Val())
		scf.addAll(cs[i].scf.Val())
		scs.addAll(cs[i].scs.Val())
		for model, cnt := range cs[i].mc.Val() {
			models[model] += atoi64Safe(cnt)
		}
	}
	w.DistinctExactEstimate = xe.count
	w.DistinctExactParamSigEstimate = xp.count
	w.DistinctFullScaffoldEstimate = scf.count
	w.DistinctSampledScaffoldEstimate = scs.count
	w.ExactIncomplete = w.ExactOverflow || xe.overflow
	w.FullScaffoldIncomplete = w.FullScaffoldOverflow || scf.overflow
	w.SampledScaffoldIncomplete = w.SampledScaffoldOverflow || scs.overflow

	// 模型集中度。
	w.DistinctModelCount = len(models)
	var top int64
	for _, cnt := range models {
		if cnt > top {
			top = cnt
		}
	}
	w.TopModelRequestCount = top
	w.ModelConcentrationAvailable = len(models) > 0

	if !isHour {
		w.PeakRPM = peak
		w.PeakRPMAvailable = true
		// reqPerBucket[0]=进行中当前分钟（不用于加速度）；[1]=最近完整分钟；[2]=上一完整分钟。
		if len(reqPerBucket) >= 2 {
			w.LatestCompletedMinuteRequestCount = reqPerBucket[1]
		}
		if len(reqPerBucket) >= 3 {
			w.PreviousCompletedMinuteRequestCount = reqPerBucket[2]
			w.RequestAccelerationAvailable = true
		}
	}
	// MaxObservedConcurrency：当前 envelope 无真实并发信号 → unavailable，绝不由 RPM 推断。
	w.MaxObservedConcurrencyAvailable = false

	w.Available.Requests = w.RequestCount > 0
	w.Available.Fingerprint = w.ExactAvailableCount > 0 && !w.ExactIncomplete
	w.Available.Cache = w.CacheApplicableCount > 0
	w.Available.ToolUse = true          // envelope 提供 ToolsPresent
	w.Available.StructuredOutput = true // envelope 提供 ResponseFormatType
	w.Available.ModelConcentration = w.ModelConcentrationAvailable
	// 以下模式 envelope 未真实提供 → 恒 false。
	w.Available.CandidateGeneration = false
	w.Available.GradingOrRanking = false
	w.Available.RewriteOrVariation = false
	w.Available.CodeGeneration = false
	return w, false
}

func (c *riskV2AggCache) uniqueAPIKeys(ctx context.Context, base string, minNow int64, nMin int, hourly bool, hrNow int64, nHour int) int {
	u := newBoundedUnion()
	pipe := c.rdb.Pipeline()
	var cmds []*redis.StringSliceCmd
	if hourly {
		for i := 0; i < nHour; i++ {
			cmds = append(cmds, pipe.SMembers(ctx, base+":h:"+strconv.FormatInt(hrNow-int64(i), 10)+":ak"))
		}
	} else {
		for i := 0; i < nMin; i++ {
			cmds = append(cmds, pipe.SMembers(ctx, base+":m:"+strconv.FormatInt(minNow-int64(i), 10)+":ak"))
		}
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return 0
	}
	for _, cmd := range cmds {
		u.addAll(cmd.Val())
	}
	return u.count
}

// multiKeyRollup 在 reader 内部汇总同一用户多 API Key 信号，仅输出数值与 availability（不暴露 HMAC/成员）。
func (c *riskV2AggCache) multiKeyRollup(ctx context.Context, userID int64, keyIDs []int64, apiKeyOverflow bool, minNow, hrNow int64) service.RiskV2MultiKeyRollup {
	r := service.RiskV2MultiKeyRollup{
		ActiveAPIKeyCount24h: len(keyIDs),
		APIKeyOverflow:       apiKeyOverflow,
	}
	if apiKeyOverflow {
		r.PerAPIKeyIncomplete = true
		r.MultiKeyIncomplete = true
	}
	if len(keyIDs) < 2 {
		r.MultiKeyAvailable = false // 单 Key：无多 Key 情形
		return r
	}
	r.MultiKeyAvailable = true

	// 同步窗口 + handoff（1h，按分钟统计每 key 活跃）。
	perMinuteKeys := make([]map[int64]bool, riskV2Window1hMinutes)
	for i := range perMinuteKeys {
		perMinuteKeys[i] = map[int64]bool{}
	}
	pipe := c.rdb.Pipeline()
	type kmCmd struct {
		ki  int
		mi  int
		cmd *redis.StringCmd
	}
	var kmCmds []kmCmd
	for ki, kid := range keyIDs {
		kb := c.keyBase(userID, kid)
		for mi := 0; mi < riskV2Window1hMinutes; mi++ {
			cmd := pipe.HGet(ctx, kb+":m:"+strconv.FormatInt(minNow-int64(mi), 10)+":c", "request")
			kmCmds = append(kmCmds, kmCmd{ki: ki, mi: mi, cmd: cmd})
		}
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		r.MultiKeyIncomplete = true
	}
	for _, kc := range kmCmds {
		if v, err := kc.cmd.Int64(); err == nil && v > 0 {
			perMinuteKeys[kc.mi][keyIDs[kc.ki]] = true
		}
	}
	sync := 0
	handoff := 0
	var prev map[int64]bool
	for mi := 0; mi < riskV2Window1hMinutes; mi++ {
		cur := perMinuteKeys[mi]
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

	// Cross-key 完整 scaffold overlap（1h）：仅用完整 scaffold（scf）的分钟桶。
	overlap, ovAvail := c.crossKeyFullScaffoldOverlap1h(ctx, userID, keyIDs, minNow)
	r.CrossKeyFullScaffoldOverlapEstimate1h = overlap
	r.CrossKeyFullScaffoldOverlapAvailable1h = ovAvail
	return r
}

func disjointKeys(a, b map[int64]bool) bool {
	for k := range a {
		if b[k] {
			return false
		}
	}
	return true
}

// crossKeyFullScaffoldOverlap1h 统计 1h 内出现在 ≥2 个 key 下的完整 scaffold 成员数（有界）。
func (c *riskV2AggCache) crossKeyFullScaffoldOverlap1h(ctx context.Context, userID int64, keyIDs []int64, minNow int64) (int, bool) {
	memberKeyCount := map[string]int{}
	for _, kid := range keyIDs {
		kb := c.keyBase(userID, kid)
		u := newBoundedUnion()
		pipe := c.rdb.Pipeline()
		cmds := make([]*redis.StringSliceCmd, riskV2Window1hMinutes)
		for i := 0; i < riskV2Window1hMinutes; i++ {
			cmds[i] = pipe.SMembers(ctx, kb+":m:"+strconv.FormatInt(minNow-int64(i), 10)+":scf")
		}
		if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
			return 0, false
		}
		for _, cmd := range cmds {
			u.addAll(cmd.Val())
		}
		for m := range u.seen {
			memberKeyCount[m]++
		}
		if len(memberKeyCount) >= riskV2MaxUnionMembers {
			break // 有界
		}
	}
	overlap := 0
	for _, cnt := range memberKeyCount {
		if cnt >= 2 {
			overlap++
		}
	}
	return overlap, true
}

// —— 客户端有界并集 ——

type boundedUnion struct {
	seen     map[string]struct{}
	count    int
	overflow bool
}

func newBoundedUnion() *boundedUnion { return &boundedUnion{seen: map[string]struct{}{}} }
func (b *boundedUnion) addAll(members []string) {
	taken := 0
	for _, m := range members {
		if taken >= riskV2MaxMembersPerReadKey {
			b.overflow = true
			break
		}
		taken++
		if _, ok := b.seen[m]; ok {
			continue
		}
		if len(b.seen) >= riskV2MaxUnionMembers {
			b.overflow = true
			break
		}
		b.seen[m] = struct{}{}
	}
	b.count = len(b.seen)
}

// —— active minutes（小时 bitmap popcount）——

func bitSet(bm []byte, minuteOfHour int) bool {
	byteIdx := minuteOfHour / 8
	if byteIdx >= len(bm) {
		return false
	}
	return bm[byteIdx]&(byte(1)<<uint(7-(minuteOfHour%8))) != 0
}

func activeMinutesForMinutes(minNow int64, n int, hourBitmaps map[int64][]byte) int {
	cnt := 0
	for i := 0; i < n; i++ {
		mt := minNow - int64(i)
		if bm, ok := hourBitmaps[mt/60]; ok && bitSet(bm, int(mt%60)) {
			cnt++
		}
	}
	return cnt
}

func activeMinutesForHours(hrNow int64, n int, hourBitmaps map[int64][]byte) int {
	cnt := 0
	for i := 0; i < n; i++ {
		if bm, ok := hourBitmaps[hrNow-int64(i)]; ok {
			for _, by := range bm {
				cnt += bits.OnesCount8(by)
			}
		}
	}
	return cnt
}
