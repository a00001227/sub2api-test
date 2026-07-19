package providerwebhook

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Phase 21E-6C-2D-1: sender delivery / headers / retry / disabled tests.
// sleep is stubbed so retries do not actually wait.

func newTestSender(url, secret string) *Sender {
	s := NewSender(Config{URL: url, Secret: secret, Timeout: 2 * time.Second}, nil)
	s.sleep = func(context.Context, time.Duration) {} // no real backoff in tests
	s.now = func() time.Time { return time.Unix(1780000000, 0) }
	return s
}

func sampleEvent() Event {
	return BuildActivated(ActivatedInput{
		EventID:                   "evt_connect_5",
		CreatedAt:                 "2026-07-14T00:00:00Z",
		ExternalProviderAccountID: "pa_1",
		Sub2apiAccountID:          "77",
		ProviderType:              "claude",
		Platform:                  "anthropic",
		Region:                    "US",
	})
}

func TestSender_Success_SetsHeadersAndVerifiableSignature(t *testing.T) {
	secret := "sek"
	var gotEventID, gotTS, gotSig string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEventID = r.Header.Get("X-Event-Id")
		gotTS = r.Header.Get("X-Timestamp")
		gotSig = r.Header.Get("X-Signature")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := newTestSender(srv.URL, secret)
	ev := sampleEvent()
	require.NoError(t, s.Send(context.Background(), ev))

	// headers present
	require.Equal(t, "evt_connect_5", gotEventID)
	require.Equal(t, "1780000000", gotTS)
	require.NotEmpty(t, gotSig)

	// signature verifies under Portal's rule over the delivered body bytes
	expected := SignBody(secret, gotTS, ev.Body)
	require.Equal(t, expected, gotSig)
	// body bytes are exactly the canonical JSON we signed
	require.Equal(t, CanonicalJSON(ev.Body), string(gotBody))
}

func TestSender_WrongSecretWouldNotVerify(t *testing.T) {
	ev := sampleEvent()
	ts := "1780000000"
	right := SignBody("right", ts, ev.Body)
	wrong := SignBody("wrong", ts, ev.Body)
	require.NotEqual(t, right, wrong)
}

func TestSender_RetriesOn500ThenGivesUp(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := newTestSender(srv.URL, "sek")
	err := s.Send(context.Background(), sampleEvent())
	require.Error(t, err)
	// bounded: initial + len(retryDelays) attempts = 1 + 3 = 4
	require.Equal(t, int32(len(retryDelays)+1), atomic.LoadInt32(&calls), "must retry a bounded number of times")
}

func TestSender_RecoversOnSecondAttempt(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := newTestSender(srv.URL, "sek")
	require.NoError(t, s.Send(context.Background(), sampleEvent()))
	require.Equal(t, int32(2), atomic.LoadInt32(&calls))
}

func TestSender_DisabledIsNoOp(t *testing.T) {
	var called int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&called, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// no secret ⇒ disabled
	s := newTestSender(srv.URL, "")
	require.False(t, s.Enabled())
	require.NoError(t, s.Send(context.Background(), sampleEvent()))
	require.Equal(t, int32(0), atomic.LoadInt32(&called), "disabled sender must not hit the server")
}

func TestSender_EventIDStableAcrossRetries(t *testing.T) {
	var ids []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids = append(ids, r.Header.Get("X-Event-Id"))
		w.WriteHeader(http.StatusInternalServerError) // force all retries
	}))
	defer srv.Close()

	s := newTestSender(srv.URL, "sek")
	_ = s.Send(context.Background(), sampleEvent())
	require.Len(t, ids, len(retryDelays)+1)
	for _, id := range ids {
		require.Equal(t, "evt_connect_5", id, "event_id must stay constant across retries")
	}
}

func TestConnectActivatedEventID_Stable(t *testing.T) {
	require.Equal(t, "evt_connect_42", ConnectActivatedEventID(42))
	require.Equal(t, ConnectActivatedEventID(42), ConnectActivatedEventID(42))
}
