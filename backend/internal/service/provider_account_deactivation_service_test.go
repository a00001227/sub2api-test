package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// Phase 21F provider-account-deactivate: ProviderAccountDeactivationService
// unit tests (all fakes).

type fakeDeactivateLocator struct {
	id    int64
	found bool
	err   error
}

func (f *fakeDeactivateLocator) FindAccountIDByExternalRef(_ context.Context, _ string) (int64, bool, error) {
	return f.id, f.found, f.err
}

type fakeDeactivateRepo struct {
	acc            *Account
	getErr         error
	schedulableSet *bool
	updated        *Account
	setErr         error
	updErr         error
}

func (f *fakeDeactivateRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	return f.acc, f.getErr
}

func (f *fakeDeactivateRepo) SetSchedulable(_ context.Context, _ int64, schedulable bool) error {
	f.schedulableSet = &schedulable
	return f.setErr
}

func (f *fakeDeactivateRepo) Update(_ context.Context, account *Account) error {
	f.updated = account
	return f.updErr
}

func TestDeactivate_ActiveAccountStopsAndDisables(t *testing.T) {
	repo := &fakeDeactivateRepo{acc: &Account{ID: 7, Status: domain.StatusActive}}
	svc := NewProviderAccountDeactivationService(
		&fakeDeactivateLocator{id: 7, found: true}, repo,
	)
	res, err := svc.Deactivate(context.Background(), "pa_abc", "user requested")
	require.NoError(t, err)
	require.Equal(t, "deactivated", res.Status)
	require.NotNil(t, repo.schedulableSet)
	require.False(t, *repo.schedulableSet, "must stop scheduling")
	require.NotNil(t, repo.updated)
	require.Equal(t, domain.StatusDisabled, repo.updated.Status)
	require.Empty(t, repo.updated.ErrorMessage)
}

func TestDeactivate_UnknownRefIsBenignNoOp(t *testing.T) {
	repo := &fakeDeactivateRepo{}
	svc := NewProviderAccountDeactivationService(
		&fakeDeactivateLocator{found: false}, repo,
	)
	res, err := svc.Deactivate(context.Background(), "pa_missing", "")
	require.NoError(t, err)
	require.Equal(t, "not_found", res.Status)
	require.Nil(t, repo.schedulableSet, "must not touch the account")
	require.Nil(t, repo.updated)
}

func TestDeactivate_AlreadyDisabledIsIdempotent(t *testing.T) {
	repo := &fakeDeactivateRepo{acc: &Account{ID: 7, Status: domain.StatusDisabled}}
	svc := NewProviderAccountDeactivationService(
		&fakeDeactivateLocator{id: 7, found: true}, repo,
	)
	res, err := svc.Deactivate(context.Background(), "pa_abc", "")
	require.NoError(t, err)
	require.Equal(t, "already_inactive", res.Status)
	require.Nil(t, repo.schedulableSet, "must not re-write an already-disabled account")
	require.Nil(t, repo.updated)
}

func TestDeactivate_RejectsInvalidRef(t *testing.T) {
	svc := NewProviderAccountDeactivationService(&fakeDeactivateLocator{}, &fakeDeactivateRepo{})
	for _, ref := range []string{"", "   ", "not_a_ref", "acc_123"} {
		_, err := svc.Deactivate(context.Background(), ref, "")
		require.Error(t, err, "ref %q must be rejected", ref)
	}
}
