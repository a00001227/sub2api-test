package providerwebhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Phase 21E-6C-2D-1: outbound webhook sender to the Provider Portal.
//
// Contract locked to Portal's Sub2apiHmacGuard:
//   headers  X-Event-Id / X-Timestamp / X-Signature
//   body     JSON of the event (must also carry event_id == header)
//   sig      hex(hmac_sha256(secret, timestamp + "." + CanonicalJSON(body)))
//   timestamp = unix seconds
//
// Fail-safe by design: config-gated (URL+secret required, else no-op),
// asynchronous (never blocks the caller / account creation), bounded
// retry. Portal downtime can never affect account creation — worst case a
// dropped notification, which Portal can reconcile out-of-band.

// Config for the sender. Empty URL or Secret ⇒ disabled (no-op).
type Config struct {
	URL     string
	Secret  string
	Timeout time.Duration
}

// Enabled reports whether both URL and secret are configured.
func (c Config) Enabled() bool { return c.URL != "" && c.Secret != "" }

// retryDelays is the bounded backoff schedule (max 3 attempts total:
// initial + 2 retries after failure). No infinite retry.
var retryDelays = []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute}

// Sender delivers events to the Portal.
type Sender struct {
	cfg    Config
	client *http.Client
	logger *slog.Logger
	// now/sleep are injectable for tests.
	now   func() time.Time
	sleep func(context.Context, time.Duration)
}

// NewSender builds a sender. A disabled config yields a no-op sender.
func NewSender(cfg Config, logger *slog.Logger) *Sender {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Sender{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout},
		logger: logger,
		now:    time.Now,
		sleep:  sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Enabled exposes config state (callers may skip event construction).
func (s *Sender) Enabled() bool { return s.cfg.Enabled() }

// Event is a ready-to-send event whose event_id is fixed at creation time
// and reused across all retries (stable id).
type Event struct {
	EventID string
	Body    map[string]any
}

// SignBody computes the Portal-compatible signature for a body.
func SignBody(secret, timestamp string, body map[string]any) string {
	message := timestamp + "." + CanonicalJSON(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

// Send delivers the event with bounded retry. It returns nil on the first
// 2xx; on exhaustion it returns the last error (callers that dispatch this
// asynchronously typically ignore the result — the log is the record).
// Disabled sender is a silent no-op.
func (s *Sender) Send(ctx context.Context, ev Event) error {
	if !s.cfg.Enabled() {
		return nil
	}
	var lastErr error
	for attempt := 0; attempt < len(retryDelays)+1; attempt++ {
		if attempt > 0 {
			s.sleep(ctx, retryDelays[attempt-1])
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		err := s.deliver(ctx, ev)
		if err == nil {
			return nil
		}
		lastErr = err
		// event_id only — never URL, secret, signature or payload.
		s.logger.Warn("provider webhook delivery failed",
			"event_id", ev.EventID, "attempt", attempt+1, "error", redact(err))
	}
	s.logger.Error("provider webhook giving up after retries", "event_id", ev.EventID)
	return lastErr
}

// SendOnce delivers the event with NO internal retry — a single attempt. The
// provider usage outbox worker owns persistent retry/backoff, so layering the
// Sender's in-process retry on top would double the retry semantics (Phase
// 21E-6D-6B-2). Disabled sender is a silent no-op (returns nil). Returns the
// delivery error so the worker can record it and schedule the next attempt.
func (s *Sender) SendOnce(ctx context.Context, ev Event) error {
	if !s.cfg.Enabled() {
		return nil
	}
	return s.deliver(ctx, ev)
}

// SendAsync fires Send in a background goroutine with its own timeout so it
// never blocks the caller (e.g. the completion service). The parent ctx is
// intentionally NOT propagated — a request-scoped cancel must not abort a
// notification that should outlive the request.
func (s *Sender) SendAsync(ev Event) {
	if !s.cfg.Enabled() {
		return
	}
	go func() {
		// Generous ceiling covering the full retry schedule + slack.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		_ = s.Send(ctx, ev)
	}()
}

func (s *Sender) deliver(ctx context.Context, ev Event) error {
	timestamp := strconv.FormatInt(s.now().Unix(), 10)
	payload := []byte(CanonicalJSON(ev.Body))
	sig := SignBody(s.cfg.Secret, timestamp, ev.Body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.URL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Event-Id", ev.EventID)
	req.Header.Set("X-Timestamp", timestamp)
	req.Header.Set("X-Signature", sig)

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("portal returned status %d", resp.StatusCode)
}

// redact strips anything URL-shaped from an error message (defense in depth
// — net/http errors can embed the target URL, which must never be logged).
func redact(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// Coarse: cut everything after a scheme marker if present.
	for _, marker := range []string{"http://", "https://"} {
		if i := indexOf(msg, marker); i >= 0 {
			return msg[:i] + "<redacted-url>"
		}
	}
	return msg
}

func indexOf(s, sub string) int {
	return bytesIndex([]byte(s), []byte(sub))
}

func bytesIndex(s, sub []byte) int {
	return bytes.Index(s, sub)
}
