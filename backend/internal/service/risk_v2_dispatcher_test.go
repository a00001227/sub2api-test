package service

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// ─────────────────────────── shared test sinks ───────────────────────────

type recordingSink struct {
	mu   sync.Mutex
	envs []RiskFeatureEnvelope
}

func (r *recordingSink) Consume(env RiskFeatureEnvelope) {
	r.mu.Lock()
	r.envs = append(r.envs, env)
	r.mu.Unlock()
}
func (r *recordingSink) last() (RiskFeatureEnvelope, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.envs)
	if n == 0 {
		return RiskFeatureEnvelope{}, 0
	}
	return r.envs[n-1], n
}
func (r *recordingSink) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.envs)
}
func (r *recordingSink) snapshot() []RiskFeatureEnvelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RiskFeatureEnvelope, len(r.envs))
	copy(out, r.envs)
	return out
}

type panicOnceSink struct {
	panicked int32
	consumed int64
}

func (s *panicOnceSink) Consume(_ RiskFeatureEnvelope) {
	atomic.AddInt64(&s.consumed, 1)
	if atomic.CompareAndSwapInt32(&s.panicked, 0, 1) {
		panic("sink boom")
	}
}
func (s *panicOnceSink) count() int64 { return atomic.LoadInt64(&s.consumed) }

type blockingSink struct{ release chan struct{} }

func (s *blockingSink) Consume(_ RiskFeatureEnvelope) { <-s.release }

// ─────────────────────────── lifecycle ───────────────────────────

func TestDispatcher_StartIdempotent_ProcessesAll(t *testing.T) {
	rec := &recordingSink{}
	d := NewRiskV2Dispatcher(64, rec)
	d.Start()
	d.Start()
	for i := 0; i < 10; i++ {
		d.Enqueue(RiskFeatureEnvelope{ServerRequestID: fmt.Sprintf("id-%d", i)})
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if rec.len() != 10 || d.Stats().Processed != 10 {
		t.Errorf("want 10 processed, got len=%d stats=%+v", rec.len(), d.Stats())
	}
}

func TestDispatcher_StopIdempotent_AfterStopEnqueueFalse(t *testing.T) {
	d := NewRiskV2Dispatcher(8, NewLogRiskV2Sink())
	d.Start()
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop1: %v", err)
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop2: %v", err)
	}
	if d.Enqueue(RiskFeatureEnvelope{ServerRequestID: "x"}) {
		t.Error("Enqueue after Stop must be false")
	}
	if d.Stats().Dropped == 0 {
		t.Error("post-stop enqueue must count dropped")
	}
}

func TestDispatcher_QueueFullNonBlocking(t *testing.T) {
	d := NewRiskV2Dispatcher(2, NewLogRiskV2Sink()) // no worker
	done := make(chan int, 1)
	go func() {
		acc := 0
		for i := 0; i < 5; i++ {
			if d.Enqueue(RiskFeatureEnvelope{ServerRequestID: fmt.Sprintf("q%d", i)}) {
				acc++
			}
		}
		done <- acc
	}()
	select {
	case acc := <-done:
		if acc != 2 {
			t.Errorf("cap=2 → 2 accepted, got %d", acc)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue blocked — must be non-blocking")
	}
	if d.Stats().Dropped != 3 {
		t.Errorf("want 3 dropped, got %d", d.Stats().Dropped)
	}
}

func TestDispatcher_PanicRecoversContinues(t *testing.T) {
	sink := &panicOnceSink{}
	d := NewRiskV2Dispatcher(8, sink)
	d.Start()
	defer d.Stop(context.Background())
	d.Enqueue(RiskFeatureEnvelope{ServerRequestID: "boom"})
	d.Enqueue(RiskFeatureEnvelope{ServerRequestID: "ok"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && sink.count() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if sink.count() < 2 {
		t.Errorf("worker must continue after panic; consumed=%d", sink.count())
	}
	if d.Stats().Panics < 1 {
		t.Errorf("panic must be counted: %+v", d.Stats())
	}
}

func TestDispatcher_GracefulDrainTimeout(t *testing.T) {
	bs := &blockingSink{release: make(chan struct{})}
	d := NewRiskV2Dispatcher(8, bs)
	d.Start()
	d.Enqueue(RiskFeatureEnvelope{ServerRequestID: "blocks"})
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if err := d.Stop(ctx); err == nil {
		t.Error("Stop must return ctx error when drain exceeds deadline")
	}
	close(bs.release)
}

func TestDispatcher_NilSafe(t *testing.T) {
	var d *RiskV2Dispatcher
	if d.Enqueue(RiskFeatureEnvelope{}) {
		t.Error("nil Enqueue → false")
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Errorf("nil Stop → nil, got %v", err)
	}
	d.Start()
}

// ─────────────────────────── area 6：dedup 上限 + 淘汰 ───────────────────────────

func TestDispatcher_DedupSameServerID(t *testing.T) {
	rec := &recordingSink{}
	d := NewRiskV2Dispatcher(16, rec)
	d.Start()
	ok1 := d.Enqueue(RiskFeatureEnvelope{ServerRequestID: "same"})
	ok2 := d.Enqueue(RiskFeatureEnvelope{ServerRequestID: "same"})
	_ = d.Stop(context.Background())
	if !ok1 || ok2 || rec.len() != 1 || d.Stats().Deduped != 1 {
		t.Errorf("dedup broken: ok1=%v ok2=%v len=%d stats=%+v", ok1, ok2, rec.len(), d.Stats())
	}
}

func TestDispatcher_EmptyServerIDNotDeduped(t *testing.T) {
	rec := &recordingSink{}
	d := NewRiskV2Dispatcher(16, rec)
	d.Start()
	d.Enqueue(RiskFeatureEnvelope{ServerRequestID: ""})
	d.Enqueue(RiskFeatureEnvelope{ServerRequestID: ""})
	_ = d.Stop(context.Background())
	if rec.len() != 2 {
		t.Errorf("empty server id must not dedup, got %d", rec.len())
	}
}

func TestDispatcher_DedupBoundedWithEviction(t *testing.T) {
	// 小 dedupCap 显式构造，塞入远超容量的唯一 ID：去重表有界（<=2*cap）且发生淘汰。
	const cap = 100
	d := newRiskV2Dispatcher(1<<15, cap, NewLogRiskV2Sink())
	d.Start()
	for i := 0; i < 1000; i++ {
		d.Enqueue(RiskFeatureEnvelope{ServerRequestID: fmt.Sprintf("srv-%d", i)})
	}
	_ = d.Stop(context.Background())
	st := d.Stats()
	if st.DedupCap != cap {
		t.Errorf("DedupCap=%d want %d", st.DedupCap, cap)
	}
	if st.DedupTracked > 2*cap {
		t.Errorf("dedup table must be bounded to <=2*cap=%d, got %d", 2*cap, st.DedupTracked)
	}
	if st.Evicted == 0 {
		t.Error("expected evictions when exceeding dedup capacity")
	}
}

// ─────────────────────────── provider states ───────────────────────────

func TestProvideRiskV2Dispatcher_States(t *testing.T) {
	if ProvideRiskV2Dispatcher(&config.Config{}, nil) != nil {
		t.Error("disabled → nil")
	}
	bad := &config.Config{}
	bad.Risk.V2 = config.RiskV2Config{Enabled: true, FingerprintHMACKey: "short", FingerprintKeyVersion: "v1"}
	if ProvideRiskV2Dispatcher(bad, nil) != nil {
		t.Error("DEGRADED → nil")
	}
	good := &config.Config{}
	good.Risk.V2 = testRiskV2Cfg()
	d := ProvideRiskV2Dispatcher(good, nil)
	if d == nil {
		t.Fatal("active → non-nil")
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Errorf("Stop: %v", err)
	}
}
