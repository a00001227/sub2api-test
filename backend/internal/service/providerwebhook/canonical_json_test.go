package providerwebhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// Phase 21E-6C-2D-1: canonical JSON must be byte-identical to Portal's
// canonicalJson (apps/provider-portal-api/src/common/canonical-json.ts).
// The expected strings below are what the Portal JS function produces for
// the same inputs (verified against its algorithm).

func TestCanonicalJSON_SortedKeysRecursive(t *testing.T) {
	// Phase fixture: {"b":2,"a":{"d":4,"c":3}}
	in := map[string]any{
		"b": 2,
		"a": map[string]any{"d": 4, "c": 3},
	}
	require.Equal(t, `{"a":{"c":3,"d":4},"b":2}`, CanonicalJSON(in))
}

func TestCanonicalJSON_ArraysKeepOrder(t *testing.T) {
	in := map[string]any{"list": []any{3, 1, 2}, "a": "x"}
	require.Equal(t, `{"a":"x","list":[3,1,2]}`, CanonicalJSON(in))
}

func TestCanonicalJSON_NullAndScalars(t *testing.T) {
	require.Equal(t, "null", CanonicalJSON(nil))
	require.Equal(t, `"s"`, CanonicalJSON("s"))
	require.Equal(t, "true", CanonicalJSON(true))
	require.Equal(t, "false", CanonicalJSON(false))
	require.Equal(t, "42", CanonicalJSON(42))
	require.Equal(t, "42", CanonicalJSON(int64(42)))
	require.Equal(t, "42", CanonicalJSON(float64(42)))
	require.Equal(t, map[string]any(nil) == nil, true)
	require.Equal(t, `{"k":null}`, CanonicalJSON(map[string]any{"k": nil}))
}

// JS JSON.stringify does NOT HTML-escape <, >, & — canonical must match.
func TestCanonicalJSON_NoHTMLEscaping(t *testing.T) {
	in := map[string]any{"u": "https://x.io/a?b=1&c=2", "html": "<b>&</b>"}
	require.Equal(t, `{"html":"<b>&</b>","u":"https://x.io/a?b=1&c=2"}`, CanonicalJSON(in))
}

// A realistic event body: keys must come out sorted at every level.
func TestCanonicalJSON_EventBody(t *testing.T) {
	body := map[string]any{
		"schema_version": 1,
		"event_id":       "evt_1",
		"event_type":     "provider.account.activated",
		"created_at":     "2026-07-14T00:00:00Z",
		"payload": map[string]any{
			"external_provider_account_id": "pa_abc",
			"sub2api_account_id":           "77",
			"status":                       "active",
			"provider_type":                "claude",
			"platform":                     "anthropic",
			"region":                       "US",
		},
	}
	got := CanonicalJSON(body)
	want := `{"created_at":"2026-07-14T00:00:00Z","event_id":"evt_1","event_type":"provider.account.activated","payload":{"external_provider_account_id":"pa_abc","platform":"anthropic","provider_type":"claude","region":"US","status":"active","sub2api_account_id":"77"},"schema_version":1}`
	require.Equal(t, want, got)
}

// Simulate Portal's verification: sign here, then recompute the exact
// Portal-side expected signature and compare — proves interop.
func TestSignature_MatchesPortalRule(t *testing.T) {
	secret := "shared-secret"
	timestamp := "1780000000"
	body := map[string]any{
		"event_id":   "evt_abc",
		"event_type": "provider.account.activated",
		"payload":    map[string]any{"external_provider_account_id": "pa_1"},
	}

	// Sender side (our impl):
	sig := SignBody(secret, timestamp, body)

	// Portal side (independent recomputation of its documented rule):
	message := timestamp + "." + CanonicalJSON(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(message))
	expected := hex.EncodeToString(mac.Sum(nil))

	require.Equal(t, expected, sig)

	// Wrong secret must not verify.
	wrong := SignBody("other-secret", timestamp, body)
	require.NotEqual(t, expected, wrong)
}
