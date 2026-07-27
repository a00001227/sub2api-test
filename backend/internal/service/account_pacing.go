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
