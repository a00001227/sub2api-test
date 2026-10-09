package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 5h 利用率随窗口过期归零:过期窗口不能把号锁在休眠里。
func TestGetSessionWindowUtilization_ExpiredWindowReturnsZero(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	base := func(end *time.Time) *Account {
		return &Account{
			Platform:         PlatformAnthropic,
			Type:             AccountTypeOAuth,
			SessionWindowEnd: end,
			Extra:            map[string]any{"session_window_utilization": 0.9, "pacing_mode": PacingModeStandard},
		}
	}

	require.Equal(t, 0.9, base(&future).GetSessionWindowUtilization(), "窗口内原样返回")
	require.True(t, base(&future).IsUtilizationDormant())

	require.Equal(t, 0.0, base(&past).GetSessionWindowUtilization(), "窗口已过期视为 0")
	require.False(t, base(&past).IsUtilizationDormant(), "过期后不再休眠")
	require.Equal(t, 1.0, base(&past).PacingSelectionWeight(), "过期后不再降权")

	require.Equal(t, 0.9, base(nil).GetSessionWindowUtilization(), "没有窗口结束时间 → 不做判断,保持原值")
}
