package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeConfigStore struct {
	snap      *ProviderAccountConfigSnapshot
	found     bool
	lastPatch *ProviderAccountConfigPatch
	updates   int
}

func (f *fakeConfigStore) GetAccountConfig(_ context.Context, _ string) (*ProviderAccountConfigSnapshot, bool, error) {
	return f.snap, f.found, nil
}

func (f *fakeConfigStore) UpdateAccountConfig(_ context.Context, _ string, patch ProviderAccountConfigPatch) error {
	if !f.found {
		return ErrProviderAccountNotFound
	}
	f.updates++
	f.lastPatch = &patch
	// reflect a few fields into the snapshot for read-back.
	if patch.ModelMapping != nil {
		f.snap.ModelMapping = patch.ModelMapping
	}
	if patch.Priority != nil {
		f.snap.Priority = *patch.Priority
	}
	if patch.MaxSessions != nil {
		f.snap.MaxSessions = *patch.MaxSessions
	}
	if patch.InterceptWarmup != nil {
		f.snap.InterceptWarmup = *patch.InterceptWarmup
	}
	if patch.TempUnschedEnabled != nil {
		f.snap.TempUnschedEnabled = *patch.TempUnschedEnabled
	}
	if patch.TempUnschedRules != nil {
		f.snap.TempUnschedRules = *patch.TempUnschedRules
	}
	return nil
}

// 全量部分更新:各字段落到 patch + 回读反映。
func TestProviderAccountConfig_SetAllFields(t *testing.T) {
	store := &fakeConfigStore{found: true, snap: &ProviderAccountConfigSnapshot{}}
	svc := NewProviderAccountConfigService(store)

	pri, sess, warm, tue := 7, 4, true, true
	rules := []TempUnschedulableRule{{ErrorCode: 429, Keywords: []string{"rate limit"}, DurationMinutes: 10}}
	out, err := svc.SetConfig(context.Background(), "pa_abc", ProviderAccountConfigInput{
		ModelMapping:       map[string]string{"m": "m"},
		Priority:           &pri,
		MaxSessions:        &sess,
		InterceptWarmup:    &warm,
		TempUnschedEnabled: &tue,
		TempUnschedRules:   &rules,
	})
	require.NoError(t, err)
	require.Equal(t, 1, store.updates)
	require.Equal(t, map[string]string{"m": "m"}, out.ModelMapping)
	require.Equal(t, 7, out.Priority)
	require.Equal(t, 4, out.MaxSessions)
	require.True(t, out.InterceptWarmup)
	require.True(t, out.TempUnschedEnabled)
	require.Len(t, out.TempUnschedRules, 1)
	require.Equal(t, 429, out.TempUnschedRules[0].ErrorCode)
}

// 全 nil = 仍调一次 Update(各字段 nil 由仓储侧忽略),回读现值。
func TestProviderAccountConfig_PartialNilFields(t *testing.T) {
	store := &fakeConfigStore{found: true, snap: &ProviderAccountConfigSnapshot{Priority: 50}}
	svc := NewProviderAccountConfigService(store)

	out, err := svc.SetConfig(context.Background(), "pa_abc", ProviderAccountConfigInput{})
	require.NoError(t, err)
	require.NotNil(t, store.lastPatch)
	require.Nil(t, store.lastPatch.ModelMapping)
	require.Nil(t, store.lastPatch.Priority)
	require.Equal(t, 50, out.Priority) // 未改
}

func TestProviderAccountConfig_NotFound(t *testing.T) {
	svc := NewProviderAccountConfigService(&fakeConfigStore{found: false})
	_, err := svc.GetConfig(context.Background(), "pa_missing")
	require.ErrorIs(t, err, ErrProviderAccountNotFound)
}
