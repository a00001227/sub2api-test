package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 手动唤醒覆盖键 utilization_dormancy_override_until:生效期内不休眠,到期自动回归。

func overriddenUtilAccount(extra map[string]any, overrideUntil any) *Account {
	a := utilAccount("standard", extra)
	a.Extra[ExtraKeyUtilizationDormancyOverrideUntil] = overrideUntil
	return a
}

func TestPacing_DormancyOverride_ActiveSuppressesDormant(t *testing.T) {
	future := time.Now().Add(3 * 24 * time.Hour).Unix()
	a := overriddenUtilAccount(map[string]any{
		"passive_usage_7d_utilization": 0.80,
		"passive_usage_7d_reset":       float64(future),
	}, float64(time.Now().Add(time.Hour).Unix()))
	require.True(t, a.IsUtilizationDormancyOverridden())
	require.False(t, a.IsUtilizationDormant(), "覆盖期内不休眠")
	require.True(t, a.IsSchedulable(), "覆盖期内可调度")
	require.Equal(t, "", a.SchedulableRejectReason())
	require.Equal(t, pacingDownweightFactor, a.PacingSelectionWeight(), "覆盖只放行,降权仍按利用率")
}

func TestPacing_DormancyOverride_ExpiredRestoresDormant(t *testing.T) {
	future := time.Now().Add(3 * 24 * time.Hour).Unix()
	a := overriddenUtilAccount(map[string]any{
		"passive_usage_7d_utilization": 0.80,
		"passive_usage_7d_reset":       float64(future),
	}, float64(time.Now().Add(-time.Minute).Unix()))
	require.False(t, a.IsUtilizationDormancyOverridden())
	require.True(t, a.IsUtilizationDormant(), "覆盖到期自动回归休眠")
	require.Equal(t, "util_dormant_7d", a.SchedulableRejectReason())
}

func TestPacing_DormancyOverride_NullOrZeroIgnored(t *testing.T) {
	for _, v := range []any{nil, float64(0), "", "abc"} {
		a := overriddenUtilAccount(map[string]any{"session_window_utilization": 0.9}, v)
		require.False(t, a.IsUtilizationDormancyOverridden(), "value=%v", v)
		require.True(t, a.IsUtilizationDormant(), "value=%v", v)
	}
}

func TestPacing_DormancyOverride_StoredDormantRespectsOverride(t *testing.T) {
	a := overriddenUtilAccount(map[string]any{"session_window_utilization": 0.9},
		float64(time.Now().Add(time.Hour).Unix()))
	require.False(t, storedUtilizationDormant(a), "通知口径与 IsUtilizationDormant 一致,覆盖期内不算休眠")
}
