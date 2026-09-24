package service

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// 账号「近期故障」进程内统计(按账号,EWMA + 时间衰减),用于两件事:
//
//  1. 调度降权:Claude 选号在同一排序组内做加权随机(weightedShuffleByPacing),权重乘上
//     SelectionWeight() —— 刚报过 429/529/5xx/传输错的号被选中的概率下降,但仍留在池里,
//     后续成功或时间推移会自动回升,无需人工恢复。以前只有配额降权,报错的号下一请求照样选中。
//  2. 面板评分:ProviderAccountMetrics.RecentErrorRate → computePacingScore 的成功率项改用它
//     (有样本时),让「综合评分」和调度真正少选它是同一件事;存活期累计成功率对近期错误几乎无反应。
//
// 只算账号级上游错误(HandleUpstreamError 的 401/403/429/529/5xx + Anthropic 传输层失败),
// 400/413/客户端断开不算。OpenAI 调度器自有 EWMA(openAIAccountRuntimeStats),这里不重复。
// 进程内、重启清零(代价只是短暂回到不降权)。
const (
	recentFailureAlpha       = 0.2             // 每个样本的权重(与 OpenAI 调度器一致)
	recentFailureHalfLife    = 5 * time.Minute // 无新样本时错误率的半衰期
	recentFailureMaxPenalty  = 0.75            // 错误率 100% 时权重降到 1-0.75=0.25
	recentFailureNoiseFloor  = 0.02            // 衰减到这以下视为无样本
	recentFailureStatMaxSize = 100000          // 防御:账号数远小于此,超出时不再新增
)

type recentFailureStat struct {
	rateBits atomic.Uint64 // float64 bits
	lastAt   atomic.Int64  // unix nano
}

// AccountRecentFailureStats 进程内按账号的近期故障率。零值可用、并发安全。
type AccountRecentFailureStats struct {
	m    sync.Map // int64 → *recentFailureStat
	size atomic.Int64
	now  func() time.Time
}

var accountRecentFailures = &AccountRecentFailureStats{}

func (s *AccountRecentFailureStats) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *AccountRecentFailureStats) load(accountID int64, create bool) *recentFailureStat {
	if v, ok := s.m.Load(accountID); ok {
		return v.(*recentFailureStat)
	}
	if !create || s.size.Load() >= recentFailureStatMaxSize {
		return nil
	}
	v, loaded := s.m.LoadOrStore(accountID, &recentFailureStat{})
	if !loaded {
		s.size.Add(1)
	}
	return v.(*recentFailureStat)
}

// Report 记一次结果:failed=true 记故障,false 记成功。先按时间衰减再吸收样本。
func (s *AccountRecentFailureStats) Report(accountID int64, failed bool) {
	if s == nil || accountID <= 0 {
		return
	}
	st := s.load(accountID, true)
	if st == nil {
		return
	}
	now := s.clock()
	sample := 0.0
	if failed {
		sample = 1.0
	}
	for {
		oldBits := st.rateBits.Load()
		old := decayedRate(math.Float64frombits(oldBits), st.lastAt.Load(), now)
		next := recentFailureAlpha*sample + (1-recentFailureAlpha)*old
		if st.rateBits.CompareAndSwap(oldBits, math.Float64bits(next)) {
			st.lastAt.Store(now.UnixNano())
			return
		}
	}
}

// ErrorRate 返回按时间衰减后的近期错误率(0-1);无样本或已衰减到噪声以下时 ok=false。
func (s *AccountRecentFailureStats) ErrorRate(accountID int64) (rate float64, ok bool) {
	if s == nil || accountID <= 0 {
		return 0, false
	}
	st := s.load(accountID, false)
	if st == nil {
		return 0, false
	}
	rate = decayedRate(math.Float64frombits(st.rateBits.Load()), st.lastAt.Load(), s.clock())
	if rate < recentFailureNoiseFloor {
		return 0, false
	}
	return rate, true
}

// SelectionWeight 调度权重因子:1 - maxPenalty×错误率,区间 [0.25, 1]。无样本 = 1。
func (s *AccountRecentFailureStats) SelectionWeight(accountID int64) float64 {
	rate, ok := s.ErrorRate(accountID)
	if !ok {
		return 1.0
	}
	return 1.0 - recentFailureMaxPenalty*clamp01(rate)
}

func decayedRate(rate float64, lastAtNano int64, now time.Time) float64 {
	if rate <= 0 || lastAtNano <= 0 {
		return 0
	}
	elapsed := now.Sub(time.Unix(0, lastAtNano))
	if elapsed <= 0 {
		return rate
	}
	return rate * math.Pow(0.5, elapsed.Seconds()/recentFailureHalfLife.Seconds())
}

// isAccountLevelUpstreamStatus 账号级上游错误(值得降权):鉴权/封禁/限流/过载/5xx。
// 400/404/413/422 等是请求侧问题,换号无用,不降权。
func isAccountLevelUpstreamStatus(status int) bool {
	switch status {
	case 401, 403, 429, 529:
		return true
	default:
		return status >= 500
	}
}
