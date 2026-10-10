package repository

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 调度快照裁剪 Extra 时必须保留利用率三键,否则 snapshot 路径下 IsUtilizationDormant /
// PacingSelectionWeight 永远看到 0,Portal 显示休眠的号照常被派单(2026-10-10 cell2 账号 6)。
func TestBuildSchedulerMetadataAccount_KeepsUtilizationDormancy(t *testing.T) {
	windowEnd := time.Now().Add(2 * time.Hour)
	reset7d := float64(time.Now().Add(72 * time.Hour).Unix())
	full := service.Account{
		ID:          6,
		Platform:    service.PlatformAnthropic,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Schedulable: true,
		Extra: map[string]any{
			"pacing_mode":                  "speed_2x",
			"session_window_utilization":   0.11,
			"passive_usage_7d_utilization": 0.79,
			"passive_usage_7d_reset":       reset7d,
			"unrelated_key":                "dropped",
		},
	}
	full.SessionWindowEnd = &windowEnd
	require.True(t, full.IsUtilizationDormant(), "前提:完整账号本身判休眠")
	require.False(t, full.IsSchedulable())

	slim := buildSchedulerMetadataAccount(full)
	require.Equal(t, 0.79, slim.Get7dUtilization())
	require.Equal(t, 0.11, slim.GetSessionWindowUtilization())
	require.True(t, slim.IsUtilizationDormant(), "快照副本必须同样判休眠")
	require.False(t, slim.IsSchedulable())
	_, kept := slim.Extra["unrelated_key"]
	require.False(t, kept, "白名单外的键仍应被裁掉")

	// 5h 过线同样生效
	full.Extra["passive_usage_7d_utilization"] = 0.1
	full.Extra["session_window_utilization"] = 0.9
	slim = buildSchedulerMetadataAccount(full)
	require.True(t, slim.IsUtilizationDormant())
}
