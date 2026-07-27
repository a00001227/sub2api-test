package service

import (
	"strings"
	"time"
)

// Phase 21H quota-budget pacing: 配额预算调度。
//
// 与"静态阈值 + 撞墙"不同，预算调度把 5h 窗口费用额度当作预算按时间匀速花：
//
//	预算目标(now) = limit × min(1, floor + 窗口已过比例 × 节奏系数 × 反馈系数)
//
// 当前花费超过预算目标 → StickyOnly（既有粘性会话继续、新会话去别的账号），
// 从源头避免"窗口前 1 小时烧光 99%、随后 4 小时躺平"。硬上限（limit /
// limit+stickyReserve）语义不变，预算只是提前的软刹车。
//
// 反馈系数（无状态 AIMD）：从账号已有的 rate_limited_at / overload_until
// 信号直接推导——刚被上游限流 → 0.5（乘性降），随时间线性恢复到 1.0
// （加性升的时间等价形式）。无 worker、无新存储、无 gateway 主流程改动。
//
// 生效范围：仅当 extra["pacing_mode"] 显式设置（provider 账号创建时写入
// "smart"）。未设置的存量账号完全保持旧行为。

// Pacing modes.
const (
	// PacingModeSteady 稳健：更保守的预算节奏与 RPM，封号风险最低。
	PacingModeSteady = "steady"
	// PacingModeSmart 智能（provider 账号默认）：满速花预算 + 反馈自适应。
	PacingModeSmart = "smart"
	// PacingModeBurst 冲量：允许前期透支预算、RPM 放大，风险自担。
	PacingModeBurst = "burst"
)

// pacingParams 每档位的调度参数。
type pacingParams struct {
	// BudgetCoeff 花预算的节奏系数（>1 = 允许比匀速快）。
	BudgetCoeff float64
	// RPMFactor base_rpm 的放大/收缩倍数。
	RPMFactor float64
}

var pacingParamsByMode = map[string]pacingParams{
	PacingModeSteady: {BudgetCoeff: 0.75, RPMFactor: 0.7},
	PacingModeSmart:  {BudgetCoeff: 1.0, RPMFactor: 1.0},
	PacingModeBurst:  {BudgetCoeff: 1.5, RPMFactor: 1.5},
}

// pacingBudgetFloor 窗口刚开始时即可花的预算比例（避免 frac≈0 时全员刹车）。
const pacingBudgetFloor = 0.10

// pacingFeedbackHalt / pacingFeedbackRecovery 反馈系数的降幅与恢复期。
const (
	pacingFeedbackFloor    = 0.5
	pacingFeedbackHold     = 30 * time.Minute // 限流后系数保持地板的时长
	pacingFeedbackRecovery = 90 * time.Minute // 之后线性恢复到 1.0 的时长
)

// claudeSessionWindowDuration Claude 订阅的 5h 会话窗口长度。
const claudeSessionWindowDuration = 5 * time.Hour

// GetPacingMode 返回账号的调度档位；未设置或非法值返回 ""（= 不启用预算
// 调度，保持传统静态阈值行为）。
func (a *Account) GetPacingMode() string {
	if a.Extra == nil {
		return ""
	}
	v, ok := a.Extra["pacing_mode"]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	s = strings.ToLower(strings.TrimSpace(s))
	if _, valid := pacingParamsByMode[s]; valid {
		return s
	}
	return ""
}

// IsValidPacingMode 校验档位值（对外接口用）。
func IsValidPacingMode(mode string) bool {
	_, ok := pacingParamsByMode[strings.ToLower(strings.TrimSpace(mode))]
	return ok
}

// PacingFeedbackFactor 无状态反馈系数 ∈ [0.5, 1.0]。
// 刚被上游限流/过载 → 0.5；30 分钟后开始线性恢复，90 分钟恢复满速。
// 没有任何限流历史 → 1.0。
func (a *Account) PacingFeedbackFactor(now time.Time) float64 {
	// 过载期内直接地板。
	if a.OverloadUntil != nil && now.Before(*a.OverloadUntil) {
		return pacingFeedbackFloor
	}
	last := a.RateLimitedAt
	if last == nil || !now.After(*last) {
		if last != nil {
			return pacingFeedbackFloor // rate_limited_at 在未来/等于 now：保守
		}
		return 1.0
	}
	since := now.Sub(*last)
	if since <= pacingFeedbackHold {
		return pacingFeedbackFloor
	}
	if since >= pacingFeedbackHold+pacingFeedbackRecovery {
		return 1.0
	}
	progress := float64(since-pacingFeedbackHold) / float64(pacingFeedbackRecovery)
	return pacingFeedbackFloor + (1.0-pacingFeedbackFloor)*progress
}

// WindowBudgetTargetFraction 当前时刻允许花掉的窗口预算比例 ∈ [floor, 1]。
// 返回值 × limit 即预算目标金额。mode 为空时返回 1（无预算刹车）。
func (a *Account) WindowBudgetTargetFraction(now time.Time) float64 {
	mode := a.GetPacingMode()
	if mode == "" {
		return 1.0
	}
	params := pacingParamsByMode[mode]

	start := a.GetCurrentWindowStartTime()
	end := start.Add(claudeSessionWindowDuration)
	if a.SessionWindowEnd != nil && now.Before(*a.SessionWindowEnd) {
		end = *a.SessionWindowEnd
	}
	total := end.Sub(start)
	if total <= 0 {
		return 1.0
	}
	elapsed := now.Sub(start)
	if elapsed < 0 {
		elapsed = 0
	}
	frac := float64(elapsed) / float64(total)
	if frac > 1 {
		frac = 1
	}

	target := pacingBudgetFloor + frac*params.BudgetCoeff*a.PacingFeedbackFactor(now)
	if target > 1 {
		target = 1
	}
	return target
}

// EffectiveBaseRPM 档位与反馈修正后的 RPM 上限。mode 为空时原样返回 base。
func (a *Account) EffectiveBaseRPM(now time.Time) int {
	base := a.GetBaseRPM()
	if base <= 0 {
		return base
	}
	mode := a.GetPacingMode()
	if mode == "" {
		return base
	}
	params := pacingParamsByMode[mode]
	effective := int(float64(base) * params.RPMFactor * a.PacingFeedbackFactor(now))
	if effective < 1 {
		effective = 1
	}
	return effective
}

// PacingTierProfile 按订阅等级分级的账号容量/预算默认值（建号时写入）。
// window_cost_limit 是预算刹车的基准（5h 窗口，美元）；concurrency /
// max_sessions 是容量护栏。动态调整不改这些基准——档位系数与反馈系数
// 在调度时实时作用于它们之上。
type PacingTierProfile struct {
	WindowCostLimit float64
	BaseRPM         int
	Concurrency     int
	MaxSessions     int
}

// pacingTierProfiles 各订阅等级的分级默认值。金额按 Claude 各订阅 5h 窗口
// 的大致标准用量能力估算（可通过后续调参修正，值只在建号时写入 Extra，
// 之后 admin/脚本可改）。
var pacingTierProfiles = map[string]PacingTierProfile{
	"max_20x": {WindowCostLimit: 35, BaseRPM: 20, Concurrency: 3, MaxSessions: 5},
	"max_5x":  {WindowCostLimit: 12, BaseRPM: 12, Concurrency: 2, MaxSessions: 3},
	"pro":     {WindowCostLimit: 5, BaseRPM: 8, Concurrency: 1, MaxSessions: 2},
}

// pacingDefaultProfile 未知/缺失等级时的保守默认（介于 pro 与 max_5x 之间）。
var pacingDefaultProfile = PacingTierProfile{WindowCostLimit: 10, BaseRPM: 10, Concurrency: 2, MaxSessions: 3}

// PacingProfileForTier 把上游原始 tier（如 "default_claude_max_20x"、
// "claude_pro"）解析为分级默认值。大小写不敏感、容忍前缀变体；未识别
// 返回保守默认。
func PacingProfileForTier(rawTier string) PacingTierProfile {
	s := strings.ToLower(strings.TrimSpace(rawTier))
	s = strings.TrimPrefix(s, "default_")
	s = strings.TrimPrefix(s, "claude_")
	if p, ok := pacingTierProfiles[s]; ok {
		return p
	}
	// 次级匹配：raw 里带关键子串（防上游改前缀格式）。
	switch {
	case strings.Contains(s, "max_20x") || strings.Contains(s, "max 20x"):
		return pacingTierProfiles["max_20x"]
	case strings.Contains(s, "max_5x") || strings.Contains(s, "max 5x"):
		return pacingTierProfiles["max_5x"]
	case strings.Contains(s, "pro"):
		return pacingTierProfiles["pro"]
	}
	return pacingDefaultProfile
}
