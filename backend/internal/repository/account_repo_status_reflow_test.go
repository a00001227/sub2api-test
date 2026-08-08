package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service/providerwebhook"
	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// fakeReflow records events synchronously so #87 emit logic is deterministic in
// tests (the real *providerwebhook.Sender.SendAsync spawns a goroutine).
type fakeReflow struct {
	enabled bool
	events  []providerwebhook.Event
}

func (f *fakeReflow) Enabled() bool                       { return f.enabled }
func (f *fakeReflow) SendAsync(ev providerwebhook.Event)  { f.events = append(f.events, ev) }

func newReflowRepo(t *testing.T, mock func(sqlmock.Sqlmock), reflow StatusReflowNotifier) *accountRepository {
	t.Helper()
	db, m, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if mock != nil {
		mock(m)
	}
	r := newAccountRepositoryWithSQL(nil, captureQuerySQL{db: db}, nil)
	r.statusReflow = reflow
	return r
}

// Portal-linked account + enabled notifier → exactly one status.changed event
// carrying the mapped status and the resolved external ref.
func TestEmitProviderStatusReflow_PortalLinked_Emits(t *testing.T) {
	fake := &fakeReflow{enabled: true}
	repo := newReflowRepo(t, func(m sqlmock.Sqlmock) {
		m.ExpectQuery("external_provider_account_id").
			WithArgs(int64(42)).
			WillReturnRows(sqlmock.NewRows([]string{"external_provider_account_id"}).AddRow("pa_abc"))
	}, fake)

	repo.emitProviderStatusReflow(context.Background(), 42, "invalid")

	require.Len(t, fake.events, 1)
	body := fake.events[0].Body
	require.Equal(t, "provider.account.status.changed", body["event_type"])
	require.Equal(t, 1, body["schema_version"])
	payload := body["payload"].(map[string]any)
	require.Equal(t, "pa_abc", payload["external_provider_account_id"])
	require.Equal(t, "invalid", payload["status"])
	// event_id is unique-per-occurrence and matches the header id.
	require.Equal(t, body["event_id"], fake.events[0].EventID)
	require.Contains(t, fake.events[0].EventID, "evt_status_pa_abc_invalid_")
}

// Non-Portal account (NULL external ref) → no event, even when enabled.
func TestEmitProviderStatusReflow_NotLinked_Silent(t *testing.T) {
	fake := &fakeReflow{enabled: true}
	repo := newReflowRepo(t, func(m sqlmock.Sqlmock) {
		m.ExpectQuery("external_provider_account_id").
			WithArgs(int64(7)).
			WillReturnRows(sqlmock.NewRows([]string{"external_provider_account_id"}).AddRow(nil))
	}, fake)

	repo.emitProviderStatusReflow(context.Background(), 7, "active")

	require.Empty(t, fake.events)
}

// Disabled notifier short-circuits BEFORE any DB lookup (no ExpectQuery set, so
// a query here would fail the mock) — config-gated, zero cost when off.
func TestEmitProviderStatusReflow_Disabled_NoQuery(t *testing.T) {
	fake := &fakeReflow{enabled: false}
	repo := newReflowRepo(t, nil, fake)

	repo.emitProviderStatusReflow(context.Background(), 1, "invalid")

	require.Empty(t, fake.events)
}

// nil notifier is a safe no-op (central / unconfigured cell).
func TestEmitProviderStatusReflow_NilNotifier_NoPanic(t *testing.T) {
	repo := newReflowRepo(t, nil, nil)
	require.NotPanics(t, func() {
		repo.emitProviderStatusReflow(context.Background(), 1, "invalid")
	})
}
