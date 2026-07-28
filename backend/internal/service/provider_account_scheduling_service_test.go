package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// Phase 21I provider-account-scheduling: ProviderAccountSchedulingService
// unit tests (all fakes). Pausing/resuming only toggles `schedulable`; status
// is never touched.

type fakeSchedulingLocator struct {
	id    int64
	found bool
	err   error
}

func (f *fakeSchedulingLocator) FindAccountIDByExternalRef(_ context.Context, _ string) (int64, bool, error) {
	return f.id, f.found, f.err
}

type fakeSchedulingRepo struct {
	acc            *Account
	getErr         error
	schedulableSet *bool
	setErr         error
}

func (f *fakeSchedulingRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	return f.acc, f.getErr
}

func (f *fakeSchedulingRepo) SetSchedulable(_ context.Context, _ int64, schedulable bool) error {
	f.schedulableSet = &schedulable
	return f.setErr
}

func TestScheduling_PauseSetsSchedulableFalseAndKeepsStatus(t *testing.T) {
	repo := &fakeSchedulingRepo{acc: &Account{ID: 7, Status: "active", Schedulable: true}}
	svc := NewProviderAccountSchedulingService(&fakeSchedulingLocator{id: 7, found: true}, repo)
	res, err := svc.SetScheduling(context.Background(), "pa_abc", false)
	require.NoError(t, err)
	require.Equal(t, "updated", res.Status)
	require.NotNil(t, repo.schedulableSet)
	require.False(t, *repo.schedulableSet, "pause → schedulable=false")
	require.Equal(t, "active", repo.acc.Status, "status must not change")
}

func TestScheduling_ResumeSetsSchedulableTrue(t *testing.T) {
	repo := &fakeSchedulingRepo{acc: &Account{ID: 7, Status: "active", Schedulable: false}}
	svc := NewProviderAccountSchedulingService(&fakeSchedulingLocator{id: 7, found: true}, repo)
	res, err := svc.SetScheduling(context.Background(), "pa_abc", true)
	require.NoError(t, err)
	require.Equal(t, "updated", res.Status)
	require.NotNil(t, repo.schedulableSet)
	require.True(t, *repo.schedulableSet, "resume → schedulable=true")
}

func TestScheduling_AlreadyAtTargetIsIdempotent(t *testing.T) {
	// Already paused → pausing again is a benign no-op (no re-write).
	repo := &fakeSchedulingRepo{acc: &Account{ID: 7, Schedulable: false}}
	svc := NewProviderAccountSchedulingService(&fakeSchedulingLocator{id: 7, found: true}, repo)
	res, err := svc.SetScheduling(context.Background(), "pa_abc", false)
	require.NoError(t, err)
	require.Equal(t, "unchanged", res.Status)
	require.Nil(t, repo.schedulableSet, "must not re-write when already at target")
}

func TestScheduling_UnknownRefIsBenignNoOp(t *testing.T) {
	repo := &fakeSchedulingRepo{}
	svc := NewProviderAccountSchedulingService(&fakeSchedulingLocator{found: false}, repo)
	res, err := svc.SetScheduling(context.Background(), "pa_missing", false)
	require.NoError(t, err)
	require.Equal(t, "not_found", res.Status)
	require.Nil(t, repo.schedulableSet, "must not touch the account")
}

func TestScheduling_RejectsInvalidRef(t *testing.T) {
	svc := NewProviderAccountSchedulingService(&fakeSchedulingLocator{}, &fakeSchedulingRepo{})
	for _, ref := range []string{"", "   ", "not_a_ref", "acc_123"} {
		_, err := svc.SetScheduling(context.Background(), ref, false)
		require.Error(t, err, "ref %q must be rejected", ref)
	}
}

func TestScheduling_PropagatesSetError(t *testing.T) {
	repo := &fakeSchedulingRepo{acc: &Account{ID: 7, Schedulable: true}, setErr: errors.New("db down")}
	svc := NewProviderAccountSchedulingService(&fakeSchedulingLocator{id: 7, found: true}, repo)
	_, err := svc.SetScheduling(context.Background(), "pa_abc", false)
	require.Error(t, err, "repo error must surface so the Portal keeps state consistent")
}
