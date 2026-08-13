package service

import (
	"strings"
	"time"
)

// Phase 21I DeRouter 五档调度：完全照搬 DeRouter 的档位模型。
//
// 五档 = 拟人 / 标准 / 2x / 3x / 5x。每档是一张固定容量表
// （并发 / 每分钟 RPM / 每小时 RPH），直接照搬 DeRouter 数值：
//
//	档位      并发  RPM  RPH   拟人化
//	humanized  2   20   190   是（活跃-冷却 + 每日休息）
//	standard   2   20   190   否
//	speed_2x   4   40   380   否
//	speed_3x   6   60   570   否
//	speed_5x  10  100   950   否
//
// 防封完全依赖上游真实利用率（Phase 1 主动休眠）+ 拟人化（Phase 4，仅
// humanized 档），不再用内部预算估算、也不再用 AIMD 反馈——两者已删除。
//
// 生效范围：仅当 extra["pacing_mode"] 显式设置。未设置的存量账号（admin
// 账号）完全保持旧的静态阈值行为。旧档位名（steady/smart/burst）作为别名
// 映射到新五档，保证存量 provider 账号无需数据迁移。

// Pacing modes（DeRouter 五档）。
const (
	// PacingModeHumanized 拟人：模拟真人节奏（活跃-冷却 + 每日休息），封号风险最低。
	// 可手动切换；新号默认档已改为 standard（见 provider_connect_account_repo）。
	PacingModeHumanized = "humanized"
	// PacingModeStandard 标准：与拟人同容量，但不遵守活跃-冷却/每日休息。
	PacingModeStandard = "standard"
	// PacingModeSpeed2x 2x 速度档。
	PacingModeSpeed2x = "speed_2x"
	// PacingModeSpeed3x 3x 速度档。
	PacingModeSpeed3x = "speed_3x"
	// PacingModeSpeed5x 5x 速度档，吞吐最高、封号风险最高。
	PacingModeSpeed5x = "speed_5x"
)

// pacingModeProfile 每档位的固定容量表（照搬 DeRouter）。
type pacingModeProfile struct {
	Concurrency int
	RPM         int
	RPH         int
	Humanized   bool // 是否遵守拟人节奏 + 每日休息
}

var pacingModeProfiles = map[string]pacingModeProfile{
	PacingModeHumanized: {Concurrency: 2, RPM: 20, RPH: 190, Humanized: true},
	PacingModeStandard:  {Concurrency: 2, RPM: 20, RPH: 190, Humanized: false},
	PacingModeSpeed2x:   {Concurrency: 4, RPM: 40, RPH: 380, Humanized: false},
	PacingModeSpeed3x:   {Concurrency: 6, RPM: 60, RPH: 570, Humanized: false},
	PacingModeSpeed5x:   {Concurrency: 10, RPM: 100, RPH: 950, Humanized: false},
}

// pacingModeAliases 旧档位名 → 新五档，保证存量账号（extra 里存的是旧名）
// 无需迁移即可继续生效。
var pacingModeAliases = map[string]string{
	"steady": PacingModeStandard,  // 旧「稳健」→ 标准
	"smart":  PacingModeHumanized, // 旧「智能」（默认）→ 拟人
	"burst":  PacingModeSpeed2x,   // 旧「冲量」→ 2x（保守映射）
}

// normalizePacingMode 归一化档位名（小写、去空白、别名解析）；非法返回 ""。
func normalizePacingMode(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if alias, ok := pacingModeAliases[s]; ok {
		s = alias
	}
	if _, valid := pacingModeProfiles[s]; valid {
		return s
	}
	return ""
}

// GetPacingMode 返回账号归一化后的调度档位；未设置或非法值返回 ""
// （= 不启用 pacing，保持传统静态阈值行为）。
func (a *Account) GetPacingMode() string {
	if a.Extra == nil {
		return ""
	}
	v, ok := a.Extra["pacing_mode"]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return normalizePacingMode(s)
}

// pacingProfileFor 返回归一化档位对应的容量表；档位为空/非法返回零值+false。
func pacingProfileFor(mode string) (pacingModeProfile, bool) {
	if mode == "" {
		return pacingModeProfile{}, false
	}
	p, ok := pacingModeProfiles[mode]
	return p, ok
}

// IsValidPacingMode 校验档位值（对外接口用）；接受新五档与旧别名。
func IsValidPacingMode(mode string) bool {
	return normalizePacingMode(mode) != ""
}

// PacingModeConcurrency 返回档位规定的并发数；档位为空返回 0（表示不覆盖）。
func (a *Account) PacingModeConcurrency() int {
	if p, ok := pacingProfileFor(a.GetPacingMode()); ok {
		return p.Concurrency
	}
	return 0
}

// PacingModeConcurrencyFor 按档位名返回并发数（供建号/改档时写 concurrency 列）。
// 非法档位返回 0。
func PacingModeConcurrencyFor(mode string) int {
	if p, ok := pacingProfileFor(normalizePacingMode(mode)); ok {
		return p.Concurrency
	}
	return 0
}

// PacingTierProfile 按订阅等级分级的账号容量/预算默认值（建号时写入）。
// window_cost_limit 是预算刹车的基准（5h 窗口，美元）；concurrency /
// max_sessions 是容量护栏。动态调整不改这些基准——档位系数与反馈系数
// 在调度时实时作用于它们之上。
type PacingTierProfile struct {
	WindowCostLimit float64
	BaseRPM         int
	BaseRPH         int // Phase 21I: 每小时请求上限（DeRouter 每小时闸）
	Concurrency     int
	MaxSessions     int
}

// pacingTierProfiles 各订阅等级的分级默认值。金额按 Claude 各订阅 5h 窗口
// 的大致标准用量能力估算（可通过后续调参修正，值只在建号时写入 Extra，
// 之后 admin/脚本可改）。
// BaseRPH 按 DeRouter 每小时闸的量级估算（约 base_rpm × 9~10，含真人节奏留白）。
var pacingTierProfiles = map[string]PacingTierProfile{
	"max_20x": {WindowCostLimit: 35, BaseRPM: 20, BaseRPH: 190, Concurrency: 3, MaxSessions: 5},
	"max_5x":  {WindowCostLimit: 12, BaseRPM: 12, BaseRPH: 114, Concurrency: 2, MaxSessions: 3},
	"pro":     {WindowCostLimit: 5, BaseRPM: 8, BaseRPH: 76, Concurrency: 1, MaxSessions: 2},
}

// pacingDefaultProfile 未知/缺失等级时的保守默认（介于 pro 与 max_5x 之间）。
var pacingDefaultProfile = PacingTierProfile{WindowCostLimit: 10, BaseRPM: 10, BaseRPH: 95, Concurrency: 2, MaxSessions: 3}

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

// ── Phase 21I: 上游真实利用率驱动的主动休眠 ────────────────────────────
//
// DeRouter 的核心防封手段：读上游返回的 5h / 7d 真实利用率，接近 100% 时
// 主动让账号退出调度（休眠），而不是等 429 撞墙。利用率数据由
// UpdateSessionWindow 在每次成功响应时采集，存入 Extra：
//
//	session_window_utilization    5h 利用率 (0-1 小数)
//	passive_usage_7d_utilization  7d 利用率 (0-1 小数)
//	passive_usage_7d_reset        7d 窗口重置时间 (unix 秒)
//
// 本阶段把这些已采集、原本仅用于展示的数值接入调度决策。

// pacingUtilizationDormantThreshold 利用率达到此值即主动休眠（排除出调度）。
// 对齐 DeRouter「剩余 < 10% → 排除」，即利用率 >= 0.90。
const pacingUtilizationDormantThreshold = 0.90

// GetSessionWindowUtilization 返回上游 5h 窗口真实利用率 (0-1)；无数据返回 0。
func (a *Account) GetSessionWindowUtilization() float64 {
	if a.Extra == nil {
		return 0
	}
	if v, ok := a.Extra["session_window_utilization"]; ok {
		return parseExtraFloat64(v)
	}
	return 0
}

// Get7dUtilization 返回上游 7d 窗口真实利用率 (0-1)；窗口已过期或无数据返回 0。
func (a *Account) Get7dUtilization() float64 {
	if a.Extra == nil {
		return 0
	}
	// 7d 窗口已过重置时间 → 数据陈旧，视为 0（不误判休眠）。
	if v, ok := a.Extra["passive_usage_7d_reset"]; ok {
		if reset := parseExtraFloat64(v); reset > 0 {
			if time.Now().After(time.Unix(int64(reset), 0)) {
				return 0
			}
		}
	}
	if v, ok := a.Extra["passive_usage_7d_utilization"]; ok {
		return parseExtraFloat64(v)
	}
	return 0
}

// pacingDownweightThreshold 利用率进入降权带的下界。
// 对齐 DeRouter：剩余 > 50%（util < 0.5）正常；剩余 10-50%（util 0.5-0.9）
// 降权 ×0.5；剩余 < 10%（util >= 0.9）排除（已由 IsUtilizationDormant 处理）。
const pacingDownweightThreshold = 0.50

// pacingDownweightFactor 降权带内的选择权重系数。
const pacingDownweightFactor = 0.5

// PacingSelectionWeight 返回账号在同优先级组内被优先选中的相对权重 (0,1]。
// util < 0.5 或未启用 pacing → 1.0（正常）；util ∈ [0.5, 0.9) → 0.5（降权）。
// util >= 0.9 的排除不在此处理（走 IsUtilizationDormant 直接退出调度）。
func (a *Account) PacingSelectionWeight() float64 {
	if a.GetPacingMode() == "" || !a.IsAnthropicOAuthOrSetupToken() {
		return 1.0
	}
	util := a.GetSessionWindowUtilization()
	if u7 := a.Get7dUtilization(); u7 > util {
		util = u7
	}
	if util >= pacingDownweightThreshold && util < pacingUtilizationDormantThreshold {
		return pacingDownweightFactor
	}
	return 1.0
}

// IsUtilizationDormant 判断账号是否因上游真实利用率接近满而应主动休眠。
// 5h 或 7d 任一窗口达到阈值即休眠。仅对 Anthropic OAuth/SetupToken 生效
// （只有这类账号才有 unified-ratelimit 利用率响应头）。
func (a *Account) IsUtilizationDormant() bool {
	if !a.IsAnthropicOAuthOrSetupToken() {
		return false
	}
	if a.GetSessionWindowUtilization() >= pacingUtilizationDormantThreshold {
		return true
	}
	if a.Get7dUtilization() >= pacingUtilizationDormantThreshold {
		return true
	}
	return false
}

// ── Phase 21I: 拟人化防封（每日休息窗口 + 活跃-冷却节奏）────────────────
//
// DeRouter 最核心的事前防封：让账号像真人一样「集中干一段、歇一段」，
// 并遵守每日作息。速度档（burst）跳过全部拟人化。
//
// 全部无状态实现——不新建表、不加 worker，用账号 ID 作为稳定种子 + 墙钟时间
// 当场算出。ID 决定每个账号的休息时段与节奏相位，天然把全池打散（避免所有
// 账号同一时刻集体休息导致容量断崖，这是多账号池相对 DeRouter 单账号视角
// 必须做的适配）。

// pacingIsSpeedMode 判断档位是否跳过拟人化（standard/2x/3x/5x 均不拟人，
// 仅 humanized 档遵守活跃-冷却 + 每日休息）。
func pacingIsSpeedMode(mode string) bool {
	p, ok := pacingProfileFor(mode)
	if !ok {
		return false
	}
	return !p.Humanized
}

const (
	// pacingDailyRestDurationMin 每日休息窗口时长（分钟），对齐 DeRouter 的 4h。
	pacingDailyRestDurationMin = 4 * 60
	// pacingDayMinutes 一天的分钟数。
	pacingDayMinutes = 24 * 60
	// pacingRestSpreadPrime 用于按 ID 打散休息起点的质数步长（与 1440 互质，
	// 保证不同 ID 的起点在 [0,1440) 上均匀铺开）。
	pacingRestSpreadPrime = 373
)

// dailyRestStartMinute 账号每日休息窗口的起始分钟（UTC，[0,1440)）。
// 由 ID 确定性打散：(ID*prime) mod 1440，全池均匀分布。
func (a *Account) dailyRestStartMinute() int {
	return int((a.ID*pacingRestSpreadPrime)%pacingDayMinutes+pacingDayMinutes) % pacingDayMinutes
}

// DailyRestWindowUTC 返回账号每日休息窗口 [startMin, endMin)（UTC 分钟）。
// endMin 可能 >1440（表示跨日），判定时对 1440 取模。
func (a *Account) DailyRestWindowUTC() (startMin, endMin int) {
	start := a.dailyRestStartMinute()
	return start, start + pacingDailyRestDurationMin
}

// IsInDailyRestWindow 判断 now(UTC) 是否落在账号的每日休息窗口内。
// 仅对启用 pacing 且非速度档的账号生效。
func (a *Account) IsInDailyRestWindow(now time.Time) bool {
	mode := a.GetPacingMode()
	if mode == "" || pacingIsSpeedMode(mode) {
		return false
	}
	u := now.UTC()
	cur := u.Hour()*60 + u.Minute()
	start, end := a.DailyRestWindowUTC()
	// 处理跨日：把当前分钟同时按「原值」和「+1440」检查。
	if cur >= start && cur < end {
		return true
	}
	if end > pacingDayMinutes && cur+pacingDayMinutes < end && cur+pacingDayMinutes >= start {
		return true
	}
	return false
}

const (
	// pacingActiveMin / pacingCoolMin 活跃段 / 冷却段时长（分钟）。
	// 真人节奏：集中干 ~50 分钟，歇 ~10 分钟（可后续调参）。
	pacingActiveMin = 50
	pacingCoolMin   = 10
	pacingCycleMin  = pacingActiveMin + pacingCoolMin
)

// IsInCooldownPhase 判断账号当前是否处于活跃-冷却节奏的「冷却段」。
// 用墙钟分钟数 + 账号 ID 相位偏移当场计算，无状态；不同账号相位错开，
// 避免全池同时进入冷却。速度档与未启用 pacing 的账号恒返回 false。
func (a *Account) IsInCooldownPhase(now time.Time) bool {
	mode := a.GetPacingMode()
	if mode == "" || pacingIsSpeedMode(mode) {
		return false
	}
	// 相位偏移：每个账号在 [0,cycle) 内错开一个固定起点。
	offset := int(a.ID % int64(pacingCycleMin))
	minuteOfEpoch := now.UTC().Unix() / 60
	pos := int((minuteOfEpoch+int64(offset))%int64(pacingCycleMin)+int64(pacingCycleMin)) % pacingCycleMin
	return pos >= pacingActiveMin
}

// IsHumanizedDormant 拟人化维度下账号当前是否应退出调度
// （处于每日休息窗口，或活跃-冷却的冷却段）。
func (a *Account) IsHumanizedDormant(now time.Time) bool {
	return a.IsInDailyRestWindow(now) || a.IsInCooldownPhase(now)
}

// DailyRestWindowLabels 返回每日休息窗口的 "HH:MM"–"HH:MM"（UTC）文案，
// 用于展示（对齐 DeRouter 的"下次: 20:00–00:00 UTC"）。非拟人档返回空串。
func (a *Account) DailyRestWindowLabels() (start, end string) {
	mode := a.GetPacingMode()
	if mode == "" || pacingIsSpeedMode(mode) {
		return "", ""
	}
	s, e := a.DailyRestWindowUTC()
	fmtMin := func(m int) string {
		m %= pacingDayMinutes
		return sprintfHHMM(m/60, m%60)
	}
	return fmtMin(s), fmtMin(e)
}

// sprintfHHMM 零填充格式化 "HH:MM"。
func sprintfHHMM(h, m int) string {
	hh := []byte{byte('0' + h/10), byte('0' + h%10)}
	mm := []byte{byte('0' + m/10), byte('0' + m%10)}
	return string(hh) + ":" + string(mm)
}
