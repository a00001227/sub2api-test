package service

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeEvidenceStore 内存实现 EvidenceCaptureStore；AppendEvidence 通过 channel 通知异步捕获完成。
type fakeEvidenceStore struct {
	mu       sync.Mutex
	flags    map[string]EvidenceFlag
	entries  map[string][]EvidenceEntry
	appended chan struct{}
}

func newFakeEvidenceStore() *fakeEvidenceStore {
	return &fakeEvidenceStore{flags: map[string]EvidenceFlag{}, entries: map[string][]EvidenceEntry{}, appended: make(chan struct{}, 64)}
}

func (f *fakeEvidenceStore) LoadFlags(_ context.Context) ([]EvidenceFlag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]EvidenceFlag, 0, len(f.flags))
	for _, v := range f.flags {
		out = append(out, v)
	}
	return out, nil
}
func (f *fakeEvidenceStore) SaveFlag(_ context.Context, fl EvidenceFlag) error {
	f.mu.Lock()
	f.flags[fl.TargetKey] = fl
	f.mu.Unlock()
	return nil
}
func (f *fakeEvidenceStore) DeleteFlag(_ context.Context, tk string) error {
	f.mu.Lock()
	delete(f.flags, tk)
	f.mu.Unlock()
	return nil
}
func (f *fakeEvidenceStore) AppendEvidence(_ context.Context, tk string, e EvidenceEntry, capN int, _ time.Duration) error {
	f.mu.Lock()
	f.entries[tk] = append([]EvidenceEntry{e}, f.entries[tk]...)
	if capN > 0 && len(f.entries[tk]) > capN {
		f.entries[tk] = f.entries[tk][:capN]
	}
	f.mu.Unlock()
	select {
	case f.appended <- struct{}{}:
	default:
	}
	return nil
}
func (f *fakeEvidenceStore) ListEvidence(_ context.Context, tk string, limit int) ([]EvidenceEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	es := f.entries[tk]
	if limit > 0 && len(es) > limit {
		es = es[:limit]
	}
	return append([]EvidenceEntry(nil), es...), nil
}
func (f *fakeEvidenceStore) PurgeEvidence(_ context.Context, tk string) error {
	f.mu.Lock()
	delete(f.entries, tk)
	f.mu.Unlock()
	return nil
}
func (f *fakeEvidenceStore) entryCount(tk string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.entries[tk])
}

func waitAppends(t *testing.T, f *fakeEvidenceStore, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-f.appended:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for append #%d", i+1)
		}
	}
}

func newEvidenceSvc(store EvidenceCaptureStore) *EvidenceCaptureService {
	return NewEvidenceCaptureService(store, EvidenceCaptureConfigView{Enabled: true, MaxCountLimit: 500, BufferTTL: time.Hour, MaxBodyBytes: 16 * 1024})
}

const evBody = `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hello"}]}`

// 无 flag → Active=false，CaptureIfFlagged 零开销不碰 store。
func TestEvidence_ZeroOverheadWhenNoFlag(t *testing.T) {
	f := newFakeEvidenceStore()
	s := newEvidenceSvc(f)
	if s.Active() {
		t.Fatal("Active should be false with no flags")
	}
	s.CaptureIfFlagged(42, 7, []byte(evBody), CaptureMeta{})
	time.Sleep(50 * time.Millisecond)
	if f.entryCount("u:42") != 0 || f.entryCount("k:7") != 0 {
		t.Fatal("nothing should be captured with no flag")
	}
}

// user 级 flag → 命中捕获，脱敏后存储。
func TestEvidence_UserCapture(t *testing.T) {
	f := newFakeEvidenceStore()
	s := newEvidenceSvc(f)
	if _, err := s.StartCapture(context.Background(), EvidenceTargetUser, 42, 3, 1); err != nil {
		t.Fatal(err)
	}
	if !s.Active() {
		t.Fatal("Active should be true after StartCapture")
	}
	s.CaptureIfFlagged(42, 7, []byte(evBody), CaptureMeta{Model: "claude-3-5-sonnet"})
	waitAppends(t, f, 1)
	if f.entryCount("u:42") != 1 {
		t.Fatalf("want 1 captured, got %d", f.entryCount("u:42"))
	}
	es, _ := s.ListEvidence(context.Background(), "u:42", 10)
	if len(es) != 1 || es[0].Body == "" || es[0].UserID != 42 {
		t.Fatalf("unexpected evidence entry: %+v", es)
	}
}

// 模板重复：同一 prompt 的两条捕获应得到相同的 PromptSimhash（红标依据）；request id 透传。
func TestEvidence_SimhashAndRequestID(t *testing.T) {
	f := newFakeEvidenceStore()
	s := newEvidenceSvc(f)
	_, _ = s.StartCapture(context.Background(), EvidenceTargetUser, 42, 5, 1)
	s.CaptureIfFlagged(42, 7, []byte(evBody), CaptureMeta{RequestID: "req-a"})
	s.CaptureIfFlagged(42, 7, []byte(evBody), CaptureMeta{RequestID: "req-b"})
	waitAppends(t, f, 2)
	es, _ := s.ListEvidence(context.Background(), "u:42", 10)
	if len(es) != 2 {
		t.Fatalf("want 2 entries, got %d", len(es))
	}
	if es[0].PromptSimhash == "" || es[0].PromptSimhash == "0" {
		t.Fatalf("expected non-zero simhash, got %q", es[0].PromptSimhash)
	}
	if es[0].PromptSimhash != es[1].PromptSimhash {
		t.Errorf("identical prompts must share simhash: %s vs %s", es[0].PromptSimhash, es[1].PromptSimhash)
	}
	// request id 透传（两条各自不同）。
	ids := map[string]bool{es[0].RequestID: true, es[1].RequestID: true}
	if !ids["req-a"] || !ids["req-b"] {
		t.Errorf("request ids not captured: %+v", []string{es[0].RequestID, es[1].RequestID})
	}
}

// 抓够 N 条自动停：第 N+1 条不再捕获，Active 变回 false。
func TestEvidence_StopsAtN(t *testing.T) {
	f := newFakeEvidenceStore()
	s := newEvidenceSvc(f)
	_, _ = s.StartCapture(context.Background(), EvidenceTargetKey, 9, 2, 1)
	for i := 0; i < 2; i++ {
		s.CaptureIfFlagged(42, 9, []byte(evBody), CaptureMeta{})
	}
	waitAppends(t, f, 2)
	// 抓够 2 条后应自动停。
	if s.Active() {
		t.Fatal("should be inactive after reaching N")
	}
	s.CaptureIfFlagged(42, 9, []byte(evBody), CaptureMeta{})
	time.Sleep(50 * time.Millisecond)
	if got := f.entryCount("k:9"); got != 2 {
		t.Fatalf("want exactly 2 captured (N cap), got %d", got)
	}
}

// key 级优先于 user 级。
func TestEvidence_KeyPreferredOverUser(t *testing.T) {
	f := newFakeEvidenceStore()
	s := newEvidenceSvc(f)
	_, _ = s.StartCapture(context.Background(), EvidenceTargetUser, 42, 5, 1)
	_, _ = s.StartCapture(context.Background(), EvidenceTargetKey, 7, 5, 1)
	s.CaptureIfFlagged(42, 7, []byte(evBody), CaptureMeta{})
	waitAppends(t, f, 1)
	if f.entryCount("k:7") != 1 || f.entryCount("u:42") != 0 {
		t.Fatalf("key flag should take precedence: k:7=%d u:42=%d", f.entryCount("k:7"), f.entryCount("u:42"))
	}
}

// purge 清空证据 + 停止捕获。
func TestEvidence_Purge(t *testing.T) {
	f := newFakeEvidenceStore()
	s := newEvidenceSvc(f)
	_, _ = s.StartCapture(context.Background(), EvidenceTargetUser, 42, 5, 1)
	s.CaptureIfFlagged(42, 7, []byte(evBody), CaptureMeta{})
	waitAppends(t, f, 1)
	if err := s.PurgeEvidence(context.Background(), "u:42", 1); err != nil {
		t.Fatal(err)
	}
	if s.Active() {
		t.Fatal("should be inactive after purge")
	}
	if f.entryCount("u:42") != 0 {
		t.Fatal("evidence should be purged")
	}
}

// 禁用 store（nil）→ 服务禁用，StartCapture 报 unavailable，捕获不 panic。
func TestEvidence_DisabledWhenNoStore(t *testing.T) {
	s := NewEvidenceCaptureService(nil, EvidenceCaptureConfigView{Enabled: true})
	if s.Active() {
		t.Fatal("no store → inactive")
	}
	if _, err := s.StartCapture(context.Background(), EvidenceTargetUser, 1, 5, 1); err != ErrEvidenceUnavailable {
		t.Fatalf("want ErrEvidenceUnavailable, got %v", err)
	}
	s.CaptureIfFlagged(1, 1, []byte(evBody), CaptureMeta{}) // 不 panic
}

// maxCount 超上限被夹住。
func TestEvidence_MaxCountClamped(t *testing.T) {
	f := newFakeEvidenceStore()
	s := NewEvidenceCaptureService(f, EvidenceCaptureConfigView{Enabled: true, MaxCountLimit: 10, BufferTTL: time.Hour, MaxBodyBytes: 1024})
	fl, err := s.StartCapture(context.Background(), EvidenceTargetUser, 42, 999, 1)
	if err != nil {
		t.Fatal(err)
	}
	if fl.Max != 10 {
		t.Fatalf("maxCount should be clamped to 10, got %d", fl.Max)
	}
}
