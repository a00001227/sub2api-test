package handler

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type noopTempUnscheduler struct{ calls int }

func (n *noopTempUnscheduler) TempUnscheduleRetryableError(context.Context, int64, *service.UpstreamFailoverError) {
	n.calls++
}

// SameAccountRetryLimit=1:第 1 次同号重试,第 2 次即换号(并触发临时下线),不再按默认 3 次重试。
func TestHandleFailoverError_SameAccountRetryLimitOverridesDefault(t *testing.T) {
	fs := NewFailoverState(10, false)
	tu := &noopTempUnscheduler{}
	fe := &service.UpstreamFailoverError{StatusCode: 502, RetryableOnSameAccount: true, SameAccountRetryLimit: 1}

	a1 := fs.HandleFailoverError(context.Background(), tu, 20, service.PlatformAnthropic, fe)
	require.Equal(t, FailoverContinue, a1)
	require.Equal(t, 1, fs.SameAccountRetryCount[20])
	require.Equal(t, 0, fs.SwitchCount, "first timeout retries the same account, no switch yet")
	require.Equal(t, 0, tu.calls)

	a2 := fs.HandleFailoverError(context.Background(), tu, 20, service.PlatformAnthropic, fe)
	require.Equal(t, FailoverContinue, a2)
	require.Equal(t, 1, fs.SwitchCount, "second timeout switches account")
	require.Equal(t, 1, tu.calls, "and temp-unschedules the stalled account")
	_, excluded := fs.FailedAccountIDs[20]
	require.True(t, excluded)
}
