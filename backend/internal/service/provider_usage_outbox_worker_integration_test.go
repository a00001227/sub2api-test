//go:build integration

package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service/providerwebhook"
)

// Phase 21E-6D-6B-2: worker delivery tests. These need a Postgres with the
// provider_usage_outbox table. Point USAGE_OUTBOX_TEST_DSN at a database that
// has migration 162 applied; the tests skip when it is unset.

var usageOutboxTestDB *sql.DB

func usageOutboxDBOrSkip(t *testing.T) *sql.DB {
	t.Helper()
	if usageOutboxTestDB != nil {
		return usageOutboxTestDB
	}
	dsn := os.Getenv("USAGE_OUTBOX_TEST_DSN")
	if dsn == "" {
		t.Skip("USAGE_OUTBOX_TEST_DSN not set — skipping outbox worker DB test")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	usageOutboxTestDB = db
	return db
}

// Phase 21E-6D-6B-2: worker delivery tests. Reuses the repository package's
// testcontainers DB via a thin accessor exported for tests.

type fakeUsageSender struct {
	mu      sync.Mutex
	sent    []string
	failAll bool
}

func (f *fakeUsageSender) Enabled() bool { return true }
func (f *fakeUsageSender) SendOnce(_ context.Context, ev providerwebhook.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll {
		return errors.New("portal returned status 500")
	}
	f.sent = append(f.sent, ev.EventID)
	return nil
}

func seedOutboxRow(t *testing.T, db *sql.DB, requestID string) string {
	t.Helper()
	eventID := providerwebhook.UsageBillableEventID(requestID)
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO provider_usage_outbox
			(event_id, request_id, external_provider_account_id, sub2api_account_id,
			 payload, status, retry_count, next_retry_at)
		VALUES ($1, $2, 'pa_x', 1, $3, 'pending', 0, NOW())
		ON CONFLICT (event_id) DO NOTHING
	`, eventID, requestID, `{"event_id":"`+eventID+`","payload":{"request_id":"`+requestID+`"}}`)
	require.NoError(t, err)
	return eventID
}

func outboxStatus(t *testing.T, db *sql.DB, eventID string) (string, int) {
	t.Helper()
	var status string
	var retry int
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT status, retry_count FROM provider_usage_outbox WHERE event_id=$1`, eventID).Scan(&status, &retry))
	return status, retry
}

// 6 + 8: worker claims pending rows and delivers → status=sent.
func TestUsageOutboxWorker_DeliversPendingToSent(t *testing.T) {
	db := usageOutboxDBOrSkip(t)
	_, _ = db.ExecContext(context.Background(), `DELETE FROM provider_usage_outbox`)
	reqID := fmt.Sprintf("client:worker-%d", time.Now().UnixNano())
	eventID := seedOutboxRow(t, db, reqID)

	sender := &fakeUsageSender{}
	w := NewProviderUsageOutboxWorker(db, sender)
	w.pollOnce() // one claim+deliver cycle, synchronous

	status, _ := outboxStatus(t, db, eventID)
	require.Equal(t, "sent", status)
	require.Contains(t, sender.sent, eventID)
}

// 7: webhook failure → status=failed with retry scheduled, retry_count++.
func TestUsageOutboxWorker_FailureSchedulesRetry(t *testing.T) {
	db := usageOutboxDBOrSkip(t)
	_, _ = db.ExecContext(context.Background(), `DELETE FROM provider_usage_outbox`)
	reqID := fmt.Sprintf("client:worker-fail-%d", time.Now().UnixNano())
	eventID := seedOutboxRow(t, db, reqID)

	sender := &fakeUsageSender{failAll: true}
	w := NewProviderUsageOutboxWorker(db, sender)
	w.pollOnce()

	status, retry := outboxStatus(t, db, eventID)
	require.Equal(t, "failed", status)
	require.Equal(t, 1, retry)

	var nextRetry *time.Time
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT next_retry_at FROM provider_usage_outbox WHERE event_id=$1`, eventID).Scan(&nextRetry))
	require.NotNil(t, nextRetry)
	require.True(t, nextRetry.After(time.Now()))
}

// 6 (concurrency): parallel workers polling the same batch never send the same
// row twice (FOR UPDATE SKIP LOCKED).
func TestUsageOutboxWorker_ConcurrentClaimNoDuplicate(t *testing.T) {
	db := usageOutboxDBOrSkip(t)
	_, _ = db.ExecContext(context.Background(), `DELETE FROM provider_usage_outbox`)
	base := time.Now().UnixNano()
	for i := 0; i < 20; i++ {
		seedOutboxRow(t, db, fmt.Sprintf("client:conc-%d-%d", base, i))
	}
	sender := &fakeUsageSender{}
	w := NewProviderUsageOutboxWorker(db, sender)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); w.pollOnce() }()
	}
	wg.Wait()

	// each seeded row delivered exactly once
	seen := map[string]int{}
	for _, id := range sender.sent {
		seen[id]++
	}
	for id, n := range seen {
		require.Equalf(t, 1, n, "event %s delivered %d times", id, n)
	}
	var sentCount int
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM provider_usage_outbox WHERE status='sent'`).Scan(&sentCount))
	require.Equal(t, 20, sentCount)
}
