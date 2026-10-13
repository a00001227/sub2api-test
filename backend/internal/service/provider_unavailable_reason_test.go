package service

import (
	"strings"
	"testing"
	"time"
)

// Portal 回流的「为什么不可用」:临时不可调度时 status 仍 active、error_message 为空,
// 必须退回 temp_unschedulable_reason,否则面板只见「熔断规则」不见原因。
func TestProviderUnavailableReason(t *testing.T) {
	now := time.Date(2026, 10, 13, 7, 40, 0, 0, time.UTC)
	future := now.Add(8 * time.Minute)
	past := now.Add(-1 * time.Minute)

	t.Run("error_message wins", func(t *testing.T) {
		acc := &Account{ErrorMessage: "Authentication failed (401): bad", TempUnschedulableUntil: &future, TempUnschedulableReason: "x"}
		if got := providerUnavailableReason(acc, now); got != "Authentication failed (401): bad" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("plain reason while window active", func(t *testing.T) {
		acc := &Account{TempUnschedulableUntil: &future, TempUnschedulableReason: "OAuth 401: invalid token"}
		got := providerUnavailableReason(acc, now)
		if got != "temp_unschedulable: OAuth 401: invalid token" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("json state rendered as one line", func(t *testing.T) {
		acc := &Account{
			TempUnschedulableUntil:  &future,
			TempUnschedulableReason: `{"until_unix":1,"triggered_at_unix":1,"status_code":529,"matched_keyword":"overloaded","rule_index":0,"error_message":"Overloaded"}`,
		}
		got := providerUnavailableReason(acc, now)
		for _, want := range []string{"HTTP 529", "overloaded", "Overloaded"} {
			if !strings.Contains(got, want) {
				t.Fatalf("missing %q in %q", want, got)
			}
		}
	})

	t.Run("expired window yields nothing", func(t *testing.T) {
		acc := &Account{TempUnschedulableUntil: &past, TempUnschedulableReason: "stale"}
		if got := providerUnavailableReason(acc, now); got != "" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("no window no reason", func(t *testing.T) {
		if got := providerUnavailableReason(&Account{}, now); got != "" {
			t.Fatalf("got %q", got)
		}
	})
}
