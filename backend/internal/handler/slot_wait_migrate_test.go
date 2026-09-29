package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsAccountSlotWaitExhausted(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"wait timeout", &ConcurrencyError{SlotType: "account", IsTimeout: true}, true},
		{"queue full", &WaitQueueFullError{SlotType: "account"}, true},
		{"concurrency error without timeout (redis etc.)", &ConcurrencyError{SlotType: "account"}, false},
		{"context canceled", context.Canceled, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isAccountSlotWaitExhausted(tc.err))
		})
	}
}

func TestFailoverState_HandleSlotWaitExhausted_MigratesOnce(t *testing.T) {
	ctx := context.Background()
	fs := NewFailoverState(3, false)
	waitErr := &ConcurrencyError{SlotType: "account", IsTimeout: true}

	// 第一次:允许改投,号进排除列表,记下等待错误;不算 failover 切换,不强制缓存计费(无绑定会话)
	require.True(t, fs.HandleSlotWaitExhausted(ctx, 42, waitErr))
	require.True(t, fs.SlotWaitMigrated)
	require.Contains(t, fs.FailedAccountIDs, int64(42))
	require.Same(t, waitErr, fs.LastSlotWaitErr)
	require.Zero(t, fs.SwitchCount)
	require.False(t, fs.ForceCacheBilling)
	require.Nil(t, fs.LastFailoverErr)

	// 第二次:改投后的号也满 → 不再改投,回原 429;排除列表不再增加
	require.False(t, fs.HandleSlotWaitExhausted(ctx, 43, &WaitQueueFullError{SlotType: "account"}))
	require.NotContains(t, fs.FailedAccountIDs, int64(43))
	require.Same(t, waitErr, fs.LastSlotWaitErr, "保留第一次的等待错误")
}

func TestFailoverState_HandleSlotWaitExhausted_BoundSessionForcesCacheBilling(t *testing.T) {
	fs := NewFailoverState(3, true)
	require.True(t, fs.HandleSlotWaitExhausted(context.Background(), 7, &WaitQueueFullError{SlotType: "account"}))
	// 有绑定会话的用户被迫换号会丢缓存,与 failover 同一口径按缓存计费,不让用户多花钱
	require.True(t, fs.ForceCacheBilling)
}
