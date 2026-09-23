package service

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeEgressRepo 只实现可选的暂停能力,其余接口方法由内嵌的 nil 接口占位(不会被调用)。
type fakeEgressRepo struct {
	AccountRepository
	paused  []int64
	reasons []string
	resumed []int64
}

func (f *fakeEgressRepo) PauseForEgressFailure(_ context.Context, id int64, reason string) error {
	f.paused = append(f.paused, id)
	f.reasons = append(f.reasons, reason)
	return nil
}
func (f *fakeEgressRepo) ResumeFromEgressPause(_ context.Context, id int64) error {
	f.resumed = append(f.resumed, id)
	return nil
}

type plainRepo struct{ AccountRepository }

func TestClassifyEgressFailure(t *testing.T) {
	cases := []struct {
		name                  string
		err                   error
		persistent, immediate bool
	}{
		{"socks auth", errors.New(`Post "https://chatgpt.com/x": socks connect tcp 1.2.3.4:443->chatgpt.com:443: username/password authentication failed`), true, true},
		{"http 407", errors.New("Proxy Authentication Required"), true, true},
		{"refused typed", syscall.ECONNREFUSED, true, false},
		{"refused string", errors.New("dial tcp 1.2.3.4:1080: connection refused"), true, false},
		{"dns", &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true}, true, false},
		{"timeout", errors.New("net/http: timeout awaiting response headers"), false, false},
		{"eof", errors.New("unexpected EOF"), false, false},
		{"canceled", context.Canceled, false, false},
		{"nil", nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, i := classifyEgressFailure(tc.err)
			require.Equal(t, tc.persistent, p, "persistent")
			require.Equal(t, tc.immediate, i, "immediate")
		})
	}
}

func TestPauseAccountForEgressFailure_AuthFailsImmediately(t *testing.T) {
	repo := &fakeEgressRepo{}
	acct := &Account{ID: 7, Name: "pa_x", Platform: PlatformOpenAI, Schedulable: true}
	tr := &egressFailureTracker{strikes: map[int64][]time.Time{}}
	err := errors.New("socks connect: username/password authentication failed")
	ok := pauseAccountForEgressFailureWith(context.Background(), repo, acct, err, "socks connect: username/password authentication failed", tr, time.Now())
	require.True(t, ok)
	require.Equal(t, []int64{7}, repo.paused)
	require.True(t, IsEgressPauseReason(repo.reasons[0]))
	require.False(t, acct.Schedulable, "in-memory account object must reflect the pause")
}

func TestPauseAccountForEgressFailure_RefusedNeedsTwoStrikesInWindow(t *testing.T) {
	repo := &fakeEgressRepo{}
	acct := &Account{ID: 9, Platform: PlatformAnthropic, Schedulable: true}
	tr := &egressFailureTracker{strikes: map[int64][]time.Time{}}
	now := time.Now()
	refused := errors.New("dial tcp 1.2.3.4:1080: connection refused")

	require.False(t, pauseAccountForEgressFailureWith(context.Background(), repo, acct, refused, "connection refused", tr, now), "first strike: no pause")
	require.Empty(t, repo.paused)
	// 超出窗口的第二次不算
	require.False(t, pauseAccountForEgressFailureWith(context.Background(), repo, acct, refused, "connection refused", tr, now.Add(egressFailureWindow+time.Second)))
	require.Empty(t, repo.paused)
	// 窗口内第二次 → 暂停
	require.True(t, pauseAccountForEgressFailureWith(context.Background(), repo, acct, refused, "connection refused", tr, now.Add(egressFailureWindow+2*time.Second)))
	require.Equal(t, []int64{9}, repo.paused)
	// 暂停后计数清零:再来一次又是第一击
	require.False(t, pauseAccountForEgressFailureWith(context.Background(), repo, acct, refused, "connection refused", tr, now.Add(egressFailureWindow+3*time.Second)))
}

func TestPauseAccountForEgressFailure_NonPersistentOrUnsupportedRepo(t *testing.T) {
	tr := &egressFailureTracker{strikes: map[int64][]time.Time{}}
	acct := &Account{ID: 1, Schedulable: true}
	// 非持久性错误不停
	repo := &fakeEgressRepo{}
	require.False(t, pauseAccountForEgressFailureWith(context.Background(), repo, acct, errors.New("unexpected EOF"), "unexpected EOF", tr, time.Now()))
	require.Empty(t, repo.paused)
	// repo 不支持暂停能力 → 只判定不落库
	require.False(t, pauseAccountForEgressFailureWith(context.Background(), &plainRepo{}, acct, errors.New("username/password authentication failed"), "x", tr, time.Now()))
	require.True(t, acct.Schedulable)
	// nil repo
	require.False(t, pauseAccountForEgressFailureWith(context.Background(), nil, acct, errors.New("username/password authentication failed"), "x", tr, time.Now()))
}
