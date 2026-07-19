package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"time"

	"github.com/lib/pq"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service/providerwebhook"
)

// Phase 21E-6D-6B-2: Provider Usage Outbox delivery worker.
//
// Claims pending/backed-off provider_usage_outbox rows (FOR UPDATE SKIP LOCKED
// so concurrent workers never grab the same row), delivers each via the shared
// providerwebhook.Sender with a SINGLE attempt (SendOnce — the outbox owns the
// persistent retry/backoff, not the Sender), then records the outcome:
//   success → status=sent, sent_at=now
//   failure → status=failed (or needs_review past max), retry_count++,
//             next_retry_at=now+backoff, last_error=<msg>
//
// It never touches the gateway/billing hot path — enqueue already happened
// atomically with the charge; this is a pure read-side delivery loop.

const (
	usageOutboxPollInterval = 2 * time.Second
	usageOutboxBatchSize    = 50
	usageOutboxMaxRetries   = 8
)

// usageOutboxBackoff returns the delay before the next attempt for a given
// (already incremented) retry_count. Capped so a stuck row retries hourly.
func usageOutboxBackoff(retryCount int) time.Duration {
	switch {
	case retryCount <= 1:
		return 30 * time.Second
	case retryCount == 2:
		return 2 * time.Minute
	case retryCount == 3:
		return 10 * time.Minute
	default:
		return time.Hour
	}
}

// ProviderUsageOutboxSender is the minimal delivery surface (satisfied by
// *providerwebhook.Sender). Kept as an interface for testability.
type ProviderUsageOutboxSender interface {
	Enabled() bool
	SendOnce(ctx context.Context, ev providerwebhook.Event) error
}

type ProviderUsageOutboxWorker struct {
	db     *sql.DB
	sender ProviderUsageOutboxSender

	stopCh chan struct{}
	wg     sync.WaitGroup
	now    func() time.Time
}

func NewProviderUsageOutboxWorker(db *sql.DB, sender ProviderUsageOutboxSender) *ProviderUsageOutboxWorker {
	return &ProviderUsageOutboxWorker{
		db:     db,
		sender: sender,
		stopCh: make(chan struct{}),
		now:    time.Now,
	}
}

// Start launches the poll loop. No-op (worker stays idle) when unconfigured:
// nil db, nil/disabled sender — so a deployment without Portal webhook config
// simply accumulates outbox rows without delivering, and never errors.
func (w *ProviderUsageOutboxWorker) Start() {
	if w == nil || w.db == nil || w.sender == nil || !w.sender.Enabled() {
		return
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.run()
	}()
}

func (w *ProviderUsageOutboxWorker) Stop() {
	if w == nil {
		return
	}
	select {
	case <-w.stopCh:
	default:
		close(w.stopCh)
	}
	w.wg.Wait()
}

func (w *ProviderUsageOutboxWorker) run() {
	ticker := time.NewTicker(usageOutboxPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w.pollOnce()
		case <-w.stopCh:
			return
		}
	}
}

// claimedRow is one outbox row locked for delivery.
type claimedRow struct {
	id         int64
	eventID    string
	retryCount int
	body       map[string]any
}

// pollOnce claims a batch and delivers each row. Delivery + status update is
// done per row; failures are isolated (one bad row never blocks the batch).
func (w *ProviderUsageOutboxWorker) pollOnce() {
	rows, err := w.claimBatch()
	if err != nil {
		logger.LegacyPrintf("service.provider_usage_outbox", "[UsageOutbox] claim failed: %v", err)
		return
	}
	for _, row := range rows {
		w.deliver(row)
	}
}

// claimBatch atomically selects & marks a batch as 'sending' inside one tx,
// using FOR UPDATE SKIP LOCKED so parallel workers take disjoint rows.
func (w *ProviderUsageOutboxWorker) claimBatch() ([]claimedRow, error) {
	ctx := context.Background()
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	sqlRows, err := tx.QueryContext(ctx, `
		SELECT id, event_id, retry_count, payload
		FROM provider_usage_outbox
		WHERE status IN ('pending', 'failed')
		  AND (next_retry_at IS NULL OR next_retry_at <= NOW())
		ORDER BY id ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, usageOutboxBatchSize)
	if err != nil {
		return nil, err
	}

	var claimed []claimedRow
	var ids []int64
	for sqlRows.Next() {
		var r claimedRow
		var payloadRaw []byte
		if err := sqlRows.Scan(&r.id, &r.eventID, &r.retryCount, &payloadRaw); err != nil {
			_ = sqlRows.Close()
			return nil, err
		}
		if err := json.Unmarshal(payloadRaw, &r.body); err != nil {
			// Corrupt payload can never deliver — skip claiming it here; it is
			// left for admin review rather than crashing the batch.
			logger.LegacyPrintf("service.provider_usage_outbox",
				"[UsageOutbox] bad payload id=%d: %v", r.id, err)
			continue
		}
		claimed = append(claimed, r)
		ids = append(ids, r.id)
	}
	if err := sqlRows.Err(); err != nil {
		_ = sqlRows.Close()
		return nil, err
	}
	_ = sqlRows.Close()

	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE provider_usage_outbox
		SET status = 'sending', updated_at = NOW()
		WHERE id = ANY($1)
	`, pq.Array(ids)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claimed, nil
}

func (w *ProviderUsageOutboxWorker) deliver(row claimedRow) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ev := providerwebhook.Event{EventID: row.eventID, Body: row.body}
	err := w.sender.SendOnce(ctx, ev)
	if err == nil {
		w.markSent(row.id)
		return
	}
	w.markFailed(row.id, row.retryCount+1, err.Error())
}

func (w *ProviderUsageOutboxWorker) markSent(id int64) {
	if _, err := w.db.ExecContext(context.Background(), `
		UPDATE provider_usage_outbox
		SET status = 'sent', sent_at = NOW(), last_error = NULL, updated_at = NOW()
		WHERE id = $1
	`, id); err != nil {
		logger.LegacyPrintf("service.provider_usage_outbox", "[UsageOutbox] mark sent id=%d failed: %v", id, err)
	}
}

func (w *ProviderUsageOutboxWorker) markFailed(id int64, retryCount int, errMsg string) {
	// Past the retry ceiling, do not spin forever — park with a far-future
	// retry so an admin can inspect. No auto-give-up delete; the row is evidence.
	next := w.now().Add(usageOutboxBackoff(retryCount))
	if retryCount >= usageOutboxMaxRetries {
		next = w.now().Add(24 * time.Hour)
	}
	if len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}
	if _, err := w.db.ExecContext(context.Background(), `
		UPDATE provider_usage_outbox
		SET status = 'failed', retry_count = $2, next_retry_at = $3,
		    last_error = $4, updated_at = NOW()
		WHERE id = $1
	`, id, retryCount, next, errMsg); err != nil {
		logger.LegacyPrintf("service.provider_usage_outbox", "[UsageOutbox] mark failed id=%d failed: %v", id, err)
	}
}
