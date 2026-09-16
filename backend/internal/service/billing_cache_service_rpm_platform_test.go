//go:build unit

package service

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// platformRPMCacheStub 分别计数全局与平台级递增，用于验证 checkRPM 的平台分流。
type platformRPMCacheStub struct {
	userCalls     int32
	platformCalls int32
	lastPlatform  string
	counter       int32
}

func (s *platformRPMCacheStub) IncrementUserGroupRPM(context.Context, int64, int64) (int, error) {
	return 0, nil
}

func (s *platformRPMCacheStub) IncrementUserRPM(context.Context, int64) (int, error) {
	atomic.AddInt32(&s.userCalls, 1)
	return int(atomic.AddInt32(&s.counter, 1)), nil
}

func (s *platformRPMCacheStub) GetUserGroupRPM(context.Context, int64, int64) (int, error) {
	return 0, nil
}
func (s *platformRPMCacheStub) GetUserRPM(context.Context, int64) (int, error) { return 0, nil }

func (s *platformRPMCacheStub) IncrementUserPlatformRPM(_ context.Context, _ int64, platform string) (int, error) {
	atomic.AddInt32(&s.platformCalls, 1)
	s.lastPlatform = platform
	return int(atomic.AddInt32(&s.counter, 1)), nil
}

func (s *platformRPMCacheStub) GetUserPlatformRPM(context.Context, int64, string) (int, error) {
	return 0, nil
}

func newBillingServiceForPlatformRPM(cache UserRPMCache) *BillingCacheService {
	return &BillingCacheService{userRPMCache: cache}
}

func platformRPMIntPtr(v int) *int { return &v }

// 用户为 openai 设了专属 RPM=1：openai 请求走平台计数，第二次超限；anthropic 请求走全局（不限）。
func TestCheckRPM_PlatformScopedLimitReplacesGlobal(t *testing.T) {
	cache := &platformRPMCacheStub{}
	svc := newBillingServiceForPlatformRPM(cache)
	user := &User{ID: 1, RPMLimit: 0, PlatformLimits: map[string]UserPlatformLimit{
		PlatformOpenAI: {RPMLimit: platformRPMIntPtr(1)},
	}}

	require.NoError(t, svc.checkRPM(context.Background(), user, nil, PlatformOpenAI))
	require.ErrorIs(t, svc.checkRPM(context.Background(), user, nil, PlatformOpenAI), ErrUserRPMExceeded)
	require.EqualValues(t, 2, atomic.LoadInt32(&cache.platformCalls))
	require.Equal(t, PlatformOpenAI, cache.lastPlatform)
	require.EqualValues(t, 0, atomic.LoadInt32(&cache.userCalls))

	// anthropic 没设专属值，全局 RPMLimit=0 → 不限、不计数
	require.NoError(t, svc.checkRPM(context.Background(), user, nil, PlatformAnthropic))
	require.EqualValues(t, 0, atomic.LoadInt32(&cache.userCalls))
}

// 平台专属值为 0 = 该平台不限，即使全局 RPMLimit>0。
func TestCheckRPM_PlatformZeroMeansUnlimited(t *testing.T) {
	cache := &platformRPMCacheStub{}
	svc := newBillingServiceForPlatformRPM(cache)
	user := &User{ID: 1, RPMLimit: 1, PlatformLimits: map[string]UserPlatformLimit{
		PlatformAnthropic: {RPMLimit: platformRPMIntPtr(0)},
	}}
	for i := 0; i < 3; i++ {
		require.NoError(t, svc.checkRPM(context.Background(), user, nil, PlatformAnthropic))
	}
	require.EqualValues(t, 0, atomic.LoadInt32(&cache.platformCalls))
	require.EqualValues(t, 0, atomic.LoadInt32(&cache.userCalls))

	// 未设专属值的平台仍受全局 RPMLimit=1 约束
	require.NoError(t, svc.checkRPM(context.Background(), user, nil, PlatformOpenAI))
	require.ErrorIs(t, svc.checkRPM(context.Background(), user, nil, PlatformOpenAI), ErrUserRPMExceeded)
	require.EqualValues(t, 2, atomic.LoadInt32(&cache.userCalls))
}

func TestUser_EffectiveLimits(t *testing.T) {
	u := &User{Concurrency: 5, RPMLimit: 30, PlatformLimits: map[string]UserPlatformLimit{
		PlatformOpenAI: {Concurrency: platformRPMIntPtr(2)},
	}}
	limit, scoped := u.EffectiveConcurrency(PlatformOpenAI)
	require.Equal(t, 2, limit)
	require.True(t, scoped)
	limit, scoped = u.EffectiveConcurrency(PlatformAnthropic)
	require.Equal(t, 5, limit)
	require.False(t, scoped)
	limit, scoped = u.EffectiveRPMLimit(PlatformOpenAI) // 只设了并发，RPM 沿用全局
	require.Equal(t, 30, limit)
	require.False(t, scoped)
	limit, scoped = u.EffectiveConcurrency("")
	require.Equal(t, 5, limit)
	require.False(t, scoped)
	var nilUser *User
	limit, scoped = nilUser.EffectiveRPMLimit(PlatformOpenAI)
	require.Equal(t, 0, limit)
	require.False(t, scoped)
}
