package service

import (
	"context"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Portal「唤醒」休眠账号:利用率休眠(5h>=0.85 / 7d>=0.75)把号排除出调度后,库里的利用率只在
// 下一次成功响应时刷新,7d 要等窗口重置才回池。Portal 需要一个手动放行:写
// accounts.extra["utilization_dormancy_override_until"](unix 秒),到期前 IsUtilizationDormant
// 恒 false。默认唤醒到当前窗口重置时间(7d 休眠取 7d 重置,5h 取窗口结束),到点自动失效,
// 回归正常休眠逻辑;不做永久关闭,避免把 7d 硬封的安全边际一起放弃。
//
// 写 Extra 走 UpdateExtra:该键不在调度中性键名单内,仓储会自动投递 account_changed outbox,
// 快照 1s 内生效(与 utilization_dormancy_changed 同一条路)。

// DormancyActionWake / DormancyActionClear 请求动作。
const (
	DormancyActionWake  = "wake"
	DormancyActionClear = "clear"
)

// dormancyOverrideMaxDuration 显式指定 until 时的上限,防止误传一个"永久"。
const dormancyOverrideMaxDuration = 7 * 24 * time.Hour

// dormancyOverrideFallbackDuration 没有可用窗口重置时间时的默认唤醒时长。
const dormancyOverrideFallbackDuration = 5 * time.Hour

// ErrInvalidDormancyAction 非法动作。
var ErrInvalidDormancyAction = infraerrors.BadRequest(
	"INVALID_DORMANCY_ACTION", "action must be one of wake/clear")

// ErrInvalidDormancyUntil until_unix 不在 (now, now+7d] 内。
var ErrInvalidDormancyUntil = infraerrors.BadRequest(
	"INVALID_DORMANCY_UNTIL", "until_unix must be in the future and within 7 days")

// providerDormancyAccountRepo 读账号 + 写 Extra 的最小依赖面(账号 repo 天然满足)。
type providerDormancyAccountRepo interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
	UpdateExtra(ctx context.Context, id int64, updates map[string]any) error
}

// ProviderAccountDormancyResult 返回给 Portal 的结果。
type ProviderAccountDormancyResult struct {
	Status        string `json:"status"` // updated | cleared | not_found | not_dormant
	OverrideUntil string `json:"override_until,omitempty"`
	OverrideUnix  int64  `json:"override_until_unix,omitempty"`
	Dormant5h     bool   `json:"dormant_5h"`
	Dormant7d     bool   `json:"dormant_7d"`
}

// ProviderAccountDormancyService 设置/清除 provider 账号的手动唤醒覆盖。
type ProviderAccountDormancyService struct {
	locator providerDeactivateLocator
	repo    providerDormancyAccountRepo
	now     func() time.Time
}

// NewProviderAccountDormancyService builds the service.
func NewProviderAccountDormancyService(
	locator providerDeactivateLocator,
	repo providerDormancyAccountRepo,
) *ProviderAccountDormancyService {
	return &ProviderAccountDormancyService{locator: locator, repo: repo, now: time.Now}
}

// SetDormancyOverride 按 external_ref 处理 wake/clear。
// wake:untilUnix>0 用显式时间(须在 (now, now+7d]),否则取当前休眠窗口的重置时间;
// 账号当前并未按存储值休眠且未给 until → not_dormant,不写。
// clear:删除覆盖键(写 JSON null),账号回归正常休眠判定。
func (s *ProviderAccountDormancyService) SetDormancyOverride(
	ctx context.Context, externalRef, action string, untilUnix int64,
) (*ProviderAccountDormancyResult, error) {
	externalRef = strings.TrimSpace(externalRef)
	if externalRef == "" || !strings.HasPrefix(externalRef, "pa_") {
		return nil, infraerrors.BadRequest("INVALID_REQUEST", "invalid external_provider_account_id")
	}
	action = strings.ToLower(strings.TrimSpace(action))
	if action != DormancyActionWake && action != DormancyActionClear {
		return nil, ErrInvalidDormancyAction
	}

	id, found, err := s.locator.FindAccountIDByExternalRef(ctx, externalRef)
	if err != nil {
		return nil, err
	}
	if !found {
		return &ProviderAccountDormancyResult{Status: "not_found"}, nil
	}
	acc, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if acc == nil {
		return &ProviderAccountDormancyResult{Status: "not_found"}, nil
	}

	now := s.now()
	dormant5h, dormant7d := storedDormancyByWindow(acc, now)
	res := &ProviderAccountDormancyResult{Dormant5h: dormant5h, Dormant7d: dormant7d}

	if action == DormancyActionClear {
		if err := s.repo.UpdateExtra(ctx, id, map[string]any{ExtraKeyUtilizationDormancyOverrideUntil: nil}); err != nil {
			return nil, err
		}
		res.Status = "cleared"
		return res, nil
	}

	var until time.Time
	switch {
	case untilUnix > 0:
		until = time.Unix(untilUnix, 0)
		if !until.After(now) || until.After(now.Add(dormancyOverrideMaxDuration)) {
			return nil, ErrInvalidDormancyUntil
		}
	default:
		if !dormant5h && !dormant7d {
			res.Status = "not_dormant"
			return res, nil
		}
		until = dormancyWindowResetAt(acc, now, dormant5h, dormant7d)
	}

	if err := s.repo.UpdateExtra(ctx, id, map[string]any{ExtraKeyUtilizationDormancyOverrideUntil: until.Unix()}); err != nil {
		return nil, err
	}
	res.Status = "updated"
	res.OverrideUntil = until.UTC().Format(time.RFC3339)
	res.OverrideUnix = until.Unix()
	return res, nil
}

// storedDormancyByWindow 按 Extra 里存储的原始利用率(不看覆盖键,但看窗口是否过期)分窗口判休眠。
func storedDormancyByWindow(acc *Account, now time.Time) (dormant5h, dormant7d bool) {
	if acc == nil || acc.Extra == nil || !acc.IsAnthropicOAuthOrSetupToken() {
		return false, false
	}
	u5 := parseExtraFloat64(acc.Extra["session_window_utilization"])
	if u5 >= pacingUtilizationDormantThreshold5h {
		// 5h 窗口已过期的陈旧值不算休眠(与 GetSessionWindowUtilization 口径一致)。
		if acc.SessionWindowEnd == nil || now.Before(*acc.SessionWindowEnd) {
			dormant5h = true
		}
	}
	u7 := parseExtraFloat64(acc.Extra["passive_usage_7d_utilization"])
	if u7 >= pacingUtilizationDormantThreshold7d {
		reset := parseExtraFloat64(acc.Extra["passive_usage_7d_reset"])
		if reset <= 0 || now.Before(time.Unix(int64(reset), 0)) {
			dormant7d = true
		}
	}
	return dormant5h, dormant7d
}

// dormancyWindowResetAt 取触发休眠的窗口里最晚的重置时间作为默认唤醒截止;没有可用时间则兜底 5h。
func dormancyWindowResetAt(acc *Account, now time.Time, dormant5h, dormant7d bool) time.Time {
	var until time.Time
	if dormant7d {
		if reset := parseExtraFloat64(acc.Extra["passive_usage_7d_reset"]); reset > 0 {
			if t := time.Unix(int64(reset), 0); t.After(now) && t.After(until) {
				until = t
			}
		}
	}
	if dormant5h && acc.SessionWindowEnd != nil && acc.SessionWindowEnd.After(now) && acc.SessionWindowEnd.After(until) {
		until = *acc.SessionWindowEnd
	}
	if until.IsZero() {
		until = now.Add(dormancyOverrideFallbackDuration)
	}
	if until.After(now.Add(dormancyOverrideMaxDuration)) {
		until = now.Add(dormancyOverrideMaxDuration)
	}
	return until
}
