package service

import (
	"context"
	"errors"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// provider-account change-proxy: ProviderAccountProxyService unit tests (fakes).

type fakeProxyLocator struct {
	id    int64
	found bool
	err   error
}

func (f *fakeProxyLocator) FindAccountIDByExternalRef(_ context.Context, _ string) (int64, bool, error) {
	return f.id, f.found, f.err
}

type fakeProxyRegionResolver struct {
	id     int64
	found  bool
	err    error
	region string // captured
}

func (f *fakeProxyRegionResolver) FindActiveProxyIDByRegion(_ context.Context, region string) (int64, bool, error) {
	f.region = region
	return f.id, f.found, f.err
}

type fakeProxyAccountRepo struct {
	acc     *Account
	getErr  error
	updated *Account
	updErr  error
}

func (f *fakeProxyAccountRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	return f.acc, f.getErr
}

func (f *fakeProxyAccountRepo) Update(_ context.Context, account *Account) error {
	f.updated = account
	return f.updErr
}

func newProxySvc(loc *fakeProxyLocator, res *fakeProxyRegionResolver, repo *fakeProxyAccountRepo) *ProviderAccountProxyService {
	return NewProviderAccountProxyService(loc, res, repo)
}

func TestSetProxyByRegion_RejectsInvalidRef(t *testing.T) {
	svc := newProxySvc(&fakeProxyLocator{}, &fakeProxyRegionResolver{}, &fakeProxyAccountRepo{})
	for _, ref := range []string{"", "   ", "not_a_ref", "acc_123"} {
		_, err := svc.SetProxyByRegion(context.Background(), ref, "OWN-abc")
		require.Error(t, err, "ref %q must be rejected", ref)
	}
}

func TestSetProxyByRegion_RejectsEmptyRegion(t *testing.T) {
	svc := newProxySvc(&fakeProxyLocator{id: 7, found: true}, &fakeProxyRegionResolver{}, &fakeProxyAccountRepo{})
	_, err := svc.SetProxyByRegion(context.Background(), "pa_abc", "   ")
	require.Error(t, err)
}

func TestSetProxyByRegion_AccountNotFound(t *testing.T) {
	repo := &fakeProxyAccountRepo{}
	svc := newProxySvc(&fakeProxyLocator{found: false}, &fakeProxyRegionResolver{}, repo)
	_, err := svc.SetProxyByRegion(context.Background(), "pa_abc", "OWN-abc")
	require.Error(t, err)
	require.Nil(t, repo.updated, "must not touch the account")
}

func TestSetProxyByRegion_ProxyNotSynced(t *testing.T) {
	repo := &fakeProxyAccountRepo{acc: &Account{ID: 7}}
	svc := newProxySvc(
		&fakeProxyLocator{id: 7, found: true},
		&fakeProxyRegionResolver{found: false}, // proxy not on this cell
		repo,
	)
	_, err := svc.SetProxyByRegion(context.Background(), "pa_abc", "OWN-abc")
	require.Error(t, err)
	require.Nil(t, repo.updated, "must not bind when proxy is absent")
}

func TestSetProxyByRegion_Success(t *testing.T) {
	repo := &fakeProxyAccountRepo{acc: &Account{ID: 7, ProxyID: ptrInt64(11)}}
	res := &fakeProxyRegionResolver{id: 42, found: true}
	svc := newProxySvc(&fakeProxyLocator{id: 7, found: true}, res, repo)

	out, err := svc.SetProxyByRegion(context.Background(), "pa_abc", "  own-XYZ  ")
	require.NoError(t, err)
	require.Equal(t, "bound", out.Status)
	require.Equal(t, int64(42), out.ProxyID)

	require.Equal(t, "own-XYZ", res.region, "region is trimmed before lookup")
	require.NotNil(t, repo.updated)
	require.NotNil(t, repo.updated.ProxyID)
	require.Equal(t, int64(42), *repo.updated.ProxyID, "account proxy_id re-pointed to the synced proxy")
	require.Nil(t, repo.updated.Proxy, "stale eager proxy cleared so Update writes the new id")
}

// ptrInt64 is defined in sub_key_channels_test.go (same package).

func TestSetProxyByRegion_PropagatesUpdateError(t *testing.T) {
	repo := &fakeProxyAccountRepo{acc: &Account{ID: 7}, updErr: errors.New("db down")}
	svc := newProxySvc(
		&fakeProxyLocator{id: 7, found: true},
		&fakeProxyRegionResolver{id: 42, found: true},
		repo,
	)
	_, err := svc.SetProxyByRegion(context.Background(), "pa_abc", "OWN-abc")
	require.Error(t, err)
}

func TestSetProxyByRegion_ConflictCodeOnMissingProxy(t *testing.T) {
	svc := newProxySvc(
		&fakeProxyLocator{id: 7, found: true},
		&fakeProxyRegionResolver{found: false},
		&fakeProxyAccountRepo{acc: &Account{ID: 7}},
	)
	_, err := svc.SetProxyByRegion(context.Background(), "pa_abc", "OWN-abc")
	require.Error(t, err)
	require.True(t, infraerrors.IsConflict(err), "missing synced proxy surfaces a 409 conflict")
	require.Equal(t, "PROXY_NOT_SYNCED", infraerrors.Reason(err))
}
