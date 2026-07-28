package service

import (
	"context"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// Phase 21E-6E account-metrics: per-account runtime metrics for the Provider
// Portal account page (DeRouter-style). Assembles DESENSITIZED metrics only —
// never proxy id / IP / host / credentials. Located by external_provider_
// account_id (Portal's pa_<uuid>), so a caller can only read metrics for an
// account it owns a reference to.
//
// Data sources are all existing services:
//   - account row: status / concurrency
//   - AccountUsageService.GetPassiveUsage: 5h / 7d utilization, quota resets,
//     subscription tier (passive = cached, no upstream call on the hot path)
//   - AccountUsageService.GetTodayStats: today's request / token counts

// providerMetricsAccountLocator 用归属引用反查账号 id。
type providerMetricsAccountLocator interface {
	FindAccountIDByExternalRef(ctx context.Context, externalRef string) (int64, bool, error)
}

// providerMetricsAccountReader 读取账号基础运行字段。
type providerMetricsAccountReader interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
}

// providerMetricsUsageReader 读取账号用量/窗口（被动缓存，不打上游）。
type providerMetricsUsageReader interface {
	GetPassiveUsage(ctx context.Context, accountID int64) (*UsageInfo, error)
	GetTodayStats(ctx context.Context, accountID int64) (*WindowStats, error)
	// GetAccountWindowStats 数 startTime 至今的请求/token（用于每小时 RPH 当前值）。
	GetAccountWindowStats(ctx context.Context, accountID int64, startTime time.Time) (*usagestats.AccountStats, error)
}

// providerMetricsConcurrencyReader 读取账号当前并发占用数。
type providerMetricsConcurrencyReader interface {
	GetAccountConcurrencyBatch(ctx context.Context, accountIDs []int64) (map[int64]int, error)
}

// providerMetricsRPMReader 读取账号当前分钟 RPM 计数及存活期成功率累计。
type providerMetricsRPMReader interface {
	GetRPM(ctx context.Context, accountID int64) (int, error)
	GetRequestOutcome(ctx context.Context, accountID int64) (success int64, total int64, err error)
}

// providerMetricsSessionReader 读取账号当前活跃会话数（SessionLimitCache
// 天然满足；可为 nil —— 缓存未装配时会话占用显示 0）。
type providerMetricsSessionReader interface {
	GetActiveSessionCount(ctx context.Context, accountID int64) (int, error)
}

// UsageWindow 是对外暴露的单个用量窗口（脱敏）。
type UsageWindow struct {
	Utilization      float64 `json:"utilization"`       // 使用率 0-100+
	ResetsAt         *string `json:"resets_at"`         // RFC3339，nil = 无
	RemainingSeconds int     `json:"remaining_seconds"` // 距重置剩余秒
}

// ProviderAccountMetrics 是返回给 Portal 的脱敏运行指标。
type ProviderAccountMetrics struct {
	Status              string       `json:"status"`
	Concurrency         int          `json:"concurrency"`            // 并发上限（兼容旧字段）
	ConcurrencyMax      int          `json:"concurrency_max"`        // 并发上限
	ConcurrencyUsed     int          `json:"concurrency_used"`       // 当前并发占用（DeRouter 的 0/2 左值）
	RPMLimit            int          `json:"rpm_limit"`              // 每分钟调度上限（DeRouter 的 0/20 右值；0 = 未设）
	RPMUsed             int          `json:"rpm_used"`               // 当前分钟已用（左值）
	RPHLimit            int          `json:"rph_limit"`              // 每小时调度上限（DeRouter 的 0/298 右值；0 = 未设）
	RPHUsed             int          `json:"rph_used"`               // 过去 1 小时请求数（DeRouter 每小时左值）
	ModelCount          int          `json:"model_count"`            // 支持的模型数（0 = 通配/全部或未知）
	SuccessRate         *float64     `json:"success_rate,omitempty"` // 存活期累计成功率 0-1（nil = 暂无样本）
	SubscriptionTier    string       `json:"subscription_tier,omitempty"`
	SubscriptionTierRaw string       `json:"subscription_tier_raw,omitempty"` // 上游原始订阅名（如 "max 20x"），展示用
	CreatedAt           string       `json:"created_at,omitempty"`            // 账号接入时间 RFC3339（前端算"托管时长"）
	FiveHour            *UsageWindow `json:"five_hour,omitempty"`
	SevenDay            *UsageWindow `json:"seven_day,omitempty"`
	TodayRequests       int64        `json:"today_requests"`
	TodayTokens         int64        `json:"today_tokens"`
	// Phase 21I DeRouter 五档 pacing（空/nil = 账号未启用 pacing）。
	PacingMode string        `json:"pacing_mode,omitempty"`
	Score      *PacingScore  `json:"score,omitempty"`
	Pacing     *PacingStatus `json:"pacing,omitempty"`
	// 会话量（Phase 21H tier-capacity）：0/0 = 未启用会话限制。
	SessionsMax  int    `json:"sessions_max"`
	SessionsUsed int    `json:"sessions_used"`
	UpdatedAt    string `json:"updated_at"` // RFC3339
}

// PacingScore 账号综合评分（0-100）及其构成，纯展示层，不参与调度。
type PacingScore struct {
	Total float64 `json:"total"`
	// 构成透明（各分项已按权重折算，加总 = Total）：
	FiveHourPart float64 `json:"five_hour_part"` // 5h 余量 ×40
	SevenDayPart float64 `json:"seven_day_part"` // 7d 余量 ×30
	SuccessPart  float64 `json:"success_part"`   // 成功率 ×20（无数据按满）
	RatePart     float64 `json:"rate_part"`      // RPM 余量 ×10
}

// PacingStatus DeRouter 五档调度的当前状态（展示用，替代旧的 PacingBudget）。
type PacingStatus struct {
	// Concurrency / RPM / RPH 当前档位的容量表（DeRouter 固定数值）。
	Concurrency int `json:"concurrency"`
	RPM         int `json:"rpm"`
	RPH         int `json:"rph"`
	// FiveHourUtil / SevenDayUtil 上游真实利用率（0-1）。
	FiveHourUtil float64 `json:"five_hour_util"`
	SevenDayUtil float64 `json:"seven_day_util"`
	// Dormant 是否因利用率接近满而主动休眠（新流量让路）。
	Dormant bool `json:"dormant"`
	// Humanized 该档位是否启用拟人化（活跃-冷却 + 每日休息）。
	Humanized bool `json:"humanized"`
	// Resting 当前是否处于拟人休息（每日休息窗口或冷却段）。
	Resting bool `json:"resting"`
	// NextRestStartUTC / NextRestEndUTC 下次每日休息窗口（"HH:MM" UTC）；
	// 仅拟人档有值，对齐 DeRouter 的"下次: 20:00–00:00 UTC"。
	NextRestStartUTC string `json:"next_rest_start_utc,omitempty"`
	NextRestEndUTC   string `json:"next_rest_end_utc,omitempty"`
}

// ProviderAccountMetricsService 组装单账号脱敏运行指标。
type ProviderAccountMetricsService struct {
	locator     providerMetricsAccountLocator
	accounts    providerMetricsAccountReader
	usage       providerMetricsUsageReader
	concurrency providerMetricsConcurrencyReader
	rpm         providerMetricsRPMReader
	sessions    providerMetricsSessionReader
	now         func() time.Time
}

// NewProviderAccountMetricsService creates the service.
func NewProviderAccountMetricsService(
	locator providerMetricsAccountLocator,
	accounts providerMetricsAccountReader,
	usage providerMetricsUsageReader,
	concurrency providerMetricsConcurrencyReader,
	rpm providerMetricsRPMReader,
	sessions providerMetricsSessionReader,
) *ProviderAccountMetricsService {
	return &ProviderAccountMetricsService{
		locator:     locator,
		accounts:    accounts,
		usage:       usage,
		concurrency: concurrency,
		rpm:         rpm,
		sessions:    sessions,
		now:         time.Now,
	}
}

// ErrProviderAccountNotFound 归属引用无对应账号。
var ErrProviderAccountNotFound = infraerrors.NotFound("PROVIDER_ACCOUNT_NOT_FOUND", "provider account not found")

// Metrics 返回指定 external_ref 账号的脱敏运行指标。
func (s *ProviderAccountMetricsService) Metrics(
	ctx context.Context, externalRef string,
) (*ProviderAccountMetrics, error) {
	id, found, err := s.locator.FindAccountIDByExternalRef(ctx, externalRef)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrProviderAccountNotFound
	}

	acc, err := s.accounts.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if acc == nil {
		return nil, ErrProviderAccountNotFound
	}

	out := &ProviderAccountMetrics{
		Status:         acc.Status,
		Concurrency:    acc.EffectiveLoadFactor(),
		ConcurrencyMax: acc.EffectiveLoadFactor(),
		RPMLimit:       acc.GetBaseRPM(),
		RPHLimit:       acc.GetBaseRPH(),
		ModelCount:     countMappedModels(acc.GetModelMapping()),
		UpdatedAt:      s.now().UTC().Format(time.RFC3339),
	}
	if !acc.CreatedAt.IsZero() {
		out.CreatedAt = acc.CreatedAt.UTC().Format(time.RFC3339)
	}

	// Current concurrency occupancy (DeRouter's 0/2 left value). Best-effort.
	if s.concurrency != nil {
		if m, cerr := s.concurrency.GetAccountConcurrencyBatch(ctx, []int64{id}); cerr == nil {
			out.ConcurrencyUsed = m[id]
		}
	}
	// Current-minute RPM count (DeRouter's 0/20 left value). Best-effort.
	if s.rpm != nil {
		if used, rerr := s.rpm.GetRPM(ctx, id); rerr == nil {
			out.RPMUsed = used
		}
		// 存活期累计成功率（Phase 21I）。仅当有样本时返回，避免新账号显示 0%。
		if succ, total, oerr := s.rpm.GetRequestOutcome(ctx, id); oerr == nil && total > 0 {
			rate := float64(succ) / float64(total)
			out.SuccessRate = &rate
		}
	}
	// Session occupancy (Phase 21H tier-capacity): max from extra, used from
	// the session-limit cache. Best-effort; 0/0 = limit not enabled.
	out.SessionsMax = acc.GetMaxSessions()
	if out.SessionsMax > 0 && s.sessions != nil {
		if used, serr := s.sessions.GetActiveSessionCount(ctx, id); serr == nil {
			out.SessionsUsed = used
		}
	}
	// Past-hour request count (DeRouter's per-hour left value). Computed from the
	// usage log on demand — no new cache, no gateway hot-path change. Best-effort.
	since := s.now().Add(-time.Hour)
	if st, herr := s.usage.GetAccountWindowStats(ctx, id, since); herr == nil && st != nil {
		out.RPHUsed = int(st.Requests)
	}

	// Usage / quota (best-effort: metrics must not fail because upstream usage
	// is momentarily unavailable — the base status/concurrency still return).
	if usage, uerr := s.usage.GetPassiveUsage(ctx, id); uerr == nil && usage != nil {
		out.SubscriptionTier = usage.SubscriptionTier
		out.SubscriptionTierRaw = usage.SubscriptionTierRaw
		out.FiveHour = toUsageWindow(usage.FiveHour)
		out.SevenDay = toUsageWindow(usage.SevenDay)
	}
	// Anthropic 订阅等级来自登录时抓取并存入 credentials 的 rate_limit_tier
	// （如 "default_claude_max_20x"）。usage 侧只对 Antigravity/Grok 填 tier，
	// 故此处补 Claude：美化成 "Max 20x" 作为对外展示名。
	if out.SubscriptionTierRaw == "" {
		if raw := strings.TrimSpace(acc.GetCredential("rate_limit_tier")); raw != "" {
			out.SubscriptionTierRaw = prettifyClaudeTier(raw)
		}
	}
	if today, terr := s.usage.GetTodayStats(ctx, id); terr == nil && today != nil {
		out.TodayRequests = today.Requests
		out.TodayTokens = today.Tokens
	}

	// Phase 21I: pacing 档位 + 评分 + 状态（仅启用 pacing 的账号返回）。
	if mode := acc.GetPacingMode(); mode != "" {
		now := s.now()
		out.PacingMode = mode
		out.Score = computePacingScore(out)

		fiveUtil := acc.GetSessionWindowUtilization()
		sevenUtil := acc.Get7dUtilization()
		status := &PacingStatus{
			FiveHourUtil: fiveUtil,
			SevenDayUtil: sevenUtil,
			Dormant:      acc.IsUtilizationDormant(),
			Humanized:    !pacingIsSpeedMode(mode),
			Resting:      acc.IsHumanizedDormant(now),
		}
		if p, ok := pacingProfileFor(mode); ok {
			status.Concurrency = p.Concurrency
			status.RPM = p.RPM
			status.RPH = p.RPH
		}
		if status.Humanized {
			status.NextRestStartUTC, status.NextRestEndUTC = acc.DailyRestWindowLabels()
		}
		out.Pacing = status
	}

	return out, nil
}

// computePacingScore 合成 0-100 评分（纯展示，不参与调度）：
// 5h 余量 ×40 + 7d 余量 ×30 + 成功率 ×20 + RPM 余量 ×10。
// 无数据的分项按满分处理（缺数据不该把账号显示成低分）。
func computePacingScore(m *ProviderAccountMetrics) *PacingScore {
	part := func(remaining float64, weight float64) float64 {
		if remaining < 0 {
			remaining = 0
		}
		if remaining > 1 {
			remaining = 1
		}
		return remaining * weight
	}

	fiveHour := 1.0
	if m.FiveHour != nil {
		fiveHour = 1 - m.FiveHour.Utilization/100.0
	}
	sevenDay := 1.0
	if m.SevenDay != nil {
		sevenDay = 1 - m.SevenDay.Utilization/100.0
	}
	// 成功率：现有指标面没有失败计数，无数据按满分；后续接入错误率后细化。
	success := 1.0
	rate := 1.0
	if m.RPMLimit > 0 {
		rate = 1 - float64(m.RPMUsed)/float64(m.RPMLimit)
	}

	s := &PacingScore{
		FiveHourPart: part(fiveHour, 40),
		SevenDayPart: part(sevenDay, 30),
		SuccessPart:  part(success, 20),
		RatePart:     part(rate, 10),
	}
	s.Total = s.FiveHourPart + s.SevenDayPart + s.SuccessPart + s.RatePart
	return s
}

// prettifyClaudeTier 把 Anthropic 原始 tier 值美化成展示名：
// "default_claude_max_20x" → "Max 20x"；"claude_pro" → "Pro"。未知格式原样返回。
func prettifyClaudeTier(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "default_claude_")
	s = strings.TrimPrefix(s, "claude_")
	s = strings.ReplaceAll(s, "_", " ")
	if s == "" {
		return raw
	}
	// Title-case each word (max 20x → Max 20x).
	parts := strings.Fields(s)
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// countMappedModels 数账号支持的模型数。model_mapping 的 key 是可用模型；若含
// 通配 "*"（Claude OAuth 常见 → 映射全部模型），个数无意义，返回 0（前端显示
// “全部”而非具体数字）。
func countMappedModels(mapping map[string]string) int {
	if len(mapping) == 0 {
		return 0
	}
	for k := range mapping {
		if k == "*" {
			return 0
		}
	}
	return len(mapping)
}

func toUsageWindow(p *UsageProgress) *UsageWindow {
	if p == nil {
		return nil
	}
	w := &UsageWindow{
		Utilization:      p.Utilization,
		RemainingSeconds: p.RemainingSeconds,
	}
	if p.ResetsAt != nil {
		s := p.ResetsAt.UTC().Format(time.RFC3339)
		w.ResetsAt = &s
	}
	return w
}
