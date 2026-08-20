package service

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeEvidenceStore 内存实现 EvidenceCaptureStore；PutTemplate 通过 channel 通知异步聚合完成。
type fakeEvidenceStore struct {
	mu     sync.Mutex
	flags  map[string]EvidenceFlag
	tpls   map[string]map[string]EvidenceTemplate // target → simhash → template
	putCh  chan struct{}
}

func newFakeEvidenceStore() *fakeEvidenceStore {
	return &fakeEvidenceStore{flags: map[string]EvidenceFlag{}, tpls: map[string]map[string]EvidenceTemplate{}, putCh: make(chan struct{}, 256)}
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
func (f *fakeEvidenceStore) GetTemplate(_ context.Context, tk, sim string) (*EvidenceTemplate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := f.tpls[tk]; m != nil {
		if t, ok := m[sim]; ok {
			cp := t
			return &cp, nil
		}
	}
	return nil, nil
}
func (f *fakeEvidenceStore) PutTemplate(_ context.Context, tk string, t EvidenceTemplate, _ time.Duration) error {
	f.mu.Lock()
	if f.tpls[tk] == nil {
		f.tpls[tk] = map[string]EvidenceTemplate{}
	}
	f.tpls[tk][t.Simhash] = t
	f.mu.Unlock()
	select {
	case f.putCh <- struct{}{}:
	default:
	}
	return nil
}
func (f *fakeEvidenceStore) TemplateCount(_ context.Context, tk string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tpls[tk]), nil
}
func (f *fakeEvidenceStore) ListTemplates(_ context.Context, tk string) ([]EvidenceTemplate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]EvidenceTemplate, 0)
	for _, t := range f.tpls[tk] {
		out = append(out, t)
	}
	return out, nil
}
func (f *fakeEvidenceStore) PurgeEvidence(_ context.Context, tk string) error {
	f.mu.Lock()
	delete(f.tpls, tk)
	f.mu.Unlock()
	return nil
}
func (f *fakeEvidenceStore) templates(tk string) []EvidenceTemplate {
	out, _ := f.ListTemplates(context.Background(), tk)
	return out
}

func waitPuts(t *testing.T, f *fakeEvidenceStore, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-f.putCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for put #%d", i+1)
		}
	}
}

func newEvidenceSvc(store EvidenceCaptureStore) *EvidenceCaptureService {
	return NewEvidenceCaptureService(store, EvidenceCaptureConfigView{Enabled: true, MaxTemplates: 500, StoreThreshold: 2, BufferTTL: time.Hour, MaxBodyBytes: 16 * 1024})
}

// 构造带指定「最新一条」文本的 anthropic 请求体。
func evReq(text string) []byte {
	return []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"` + text + `"}]}`)
}

// 无 flag → Active=false，CaptureIfFlagged 零开销不碰 store。
func TestEvidence_ZeroOverheadWhenNoFlag(t *testing.T) {
	f := newFakeEvidenceStore()
	s := newEvidenceSvc(f)
	if s.Active() {
		t.Fatal("Active should be false with no flags")
	}
	s.CaptureIfFlagged(42, 7, evReq("hi"), CaptureMeta{})
	time.Sleep(50 * time.Millisecond)
	if len(f.templates("u:42")) != 0 {
		t.Fatal("nothing should be captured with no flag")
	}
}

// 同一模板重复≥阈值 → 聚合成 1 个模板、count 累加、达阈值存 body、聚合 key。
func TestEvidence_RepeatedTemplateAggregated(t *testing.T) {
	f := newFakeEvidenceStore()
	s := newEvidenceSvc(f) // StoreThreshold=2
	if _, err := s.StartCapture(context.Background(), EvidenceTargetUser, 42, 0, 1); err != nil {
		t.Fatal(err)
	}
	// 同一 prompt 发 3 次（不同 key）。
	s.CaptureIfFlagged(42, 7, evReq("harvest me"), CaptureMeta{Model: "m", RequestID: "r1"})
	s.CaptureIfFlagged(42, 8, evReq("harvest me"), CaptureMeta{Model: "m", RequestID: "r2"})
	s.CaptureIfFlagged(42, 7, evReq("harvest me"), CaptureMeta{Model: "m", RequestID: "r3"})
	waitPuts(t, f, 3)

	tpls := f.templates("u:42")
	if len(tpls) != 1 {
		t.Fatalf("want 1 aggregated template, got %d", len(tpls))
	}
	tp := tpls[0]
	if tp.Count != 3 {
		t.Errorf("count should be 3, got %d", tp.Count)
	}
	if !tp.HasBody || tp.Body == "" {
		t.Errorf("body should be stored once threshold(2) reached")
	}
	if len(tp.APIKeyIDs) != 2 {
		t.Errorf("should aggregate 2 distinct keys, got %v", tp.APIKeyIDs)
	}
}

// 一次性不同 prompt → 各 count=1、未达阈值 → 不存原文（正常流量原文不落库）。
func TestEvidence_OneOffNotStored(t *testing.T) {
	f := newFakeEvidenceStore()
	s := newEvidenceSvc(f) // StoreThreshold=2
	_, _ = s.StartCapture(context.Background(), EvidenceTargetUser, 42, 0, 1)
	s.CaptureIfFlagged(42, 7, evReq("unique question one about apples"), CaptureMeta{})
	s.CaptureIfFlagged(42, 7, evReq("unique question two about oranges"), CaptureMeta{})
	waitPuts(t, f, 2)
	for _, tp := range f.templates("u:42") {
		if tp.Count != 1 {
			t.Errorf("one-off should have count 1, got %d", tp.Count)
		}
		if tp.HasBody || tp.Body != "" {
			t.Errorf("one-off (below threshold) must NOT store body")
		}
	}
}

// key 级优先于 user 级。
func TestEvidence_KeyPreferredOverUser(t *testing.T) {
	f := newFakeEvidenceStore()
	s := newEvidenceSvc(f)
	_, _ = s.StartCapture(context.Background(), EvidenceTargetUser, 42, 0, 1)
	_, _ = s.StartCapture(context.Background(), EvidenceTargetKey, 7, 0, 1)
	s.CaptureIfFlagged(42, 7, evReq("x"), CaptureMeta{})
	waitPuts(t, f, 1)
	if len(f.templates("k:7")) != 1 || len(f.templates("u:42")) != 0 {
		t.Fatalf("key flag should take precedence: k:7=%d u:42=%d", len(f.templates("k:7")), len(f.templates("u:42")))
	}
}

// 超 MaxTemplates 不再追新模板。
func TestEvidence_MaxTemplatesCap(t *testing.T) {
	f := newFakeEvidenceStore()
	s := NewEvidenceCaptureService(f, EvidenceCaptureConfigView{Enabled: true, MaxTemplates: 2, StoreThreshold: 2, BufferTTL: time.Hour, MaxBodyBytes: 1024})
	_, _ = s.StartCapture(context.Background(), EvidenceTargetUser, 42, 0, 1)
	s.CaptureIfFlagged(42, 7, evReq("t one"), CaptureMeta{})
	s.CaptureIfFlagged(42, 7, evReq("t two"), CaptureMeta{})
	waitPuts(t, f, 2)
	// 第 3 个不同模板：超上限，不追新 → 无 put。
	s.CaptureIfFlagged(42, 7, evReq("t three overflow"), CaptureMeta{})
	time.Sleep(80 * time.Millisecond)
	if n := len(f.templates("u:42")); n != 2 {
		t.Fatalf("want max 2 templates, got %d", n)
	}
}

// purge 清空证据 + 停止捕获。
func TestEvidence_Purge(t *testing.T) {
	f := newFakeEvidenceStore()
	s := newEvidenceSvc(f)
	_, _ = s.StartCapture(context.Background(), EvidenceTargetUser, 42, 0, 1)
	s.CaptureIfFlagged(42, 7, evReq("y"), CaptureMeta{})
	waitPuts(t, f, 1)
	if err := s.PurgeEvidence(context.Background(), "u:42", 1); err != nil {
		t.Fatal(err)
	}
	if s.Active() {
		t.Fatal("should be inactive after purge")
	}
	if len(f.templates("u:42")) != 0 {
		t.Fatal("evidence should be purged")
	}
}

// 禁用 store（nil）→ 服务禁用，StartCapture 报 unavailable，捕获不 panic。
func TestEvidence_DisabledWhenNoStore(t *testing.T) {
	s := NewEvidenceCaptureService(nil, EvidenceCaptureConfigView{Enabled: true})
	if s.Active() {
		t.Fatal("no store → inactive")
	}
	if _, err := s.StartCapture(context.Background(), EvidenceTargetUser, 1, 0, 1); err != ErrEvidenceUnavailable {
		t.Fatalf("want ErrEvidenceUnavailable, got %v", err)
	}
	s.CaptureIfFlagged(1, 1, evReq("z"), CaptureMeta{}) // 不 panic
}

// simhash 只看最新一条：多轮对话（共享历史、最新一条不同）不误判为重复；同一模板反复发才判重。
func TestEvidence_SimhashUsesLastMessageOnly(t *testing.T) {
	a := []byte(`{"messages":[{"role":"user","content":"same shared preamble history"},{"role":"assistant","content":"ok"},{"role":"user","content":"a distinct question about apples and oranges"}]}`)
	b := []byte(`{"messages":[{"role":"user","content":"same shared preamble history"},{"role":"assistant","content":"ok"},{"role":"user","content":"a completely different question on quantum entanglement physics"}]}`)
	if evidencePromptSimhash(a) == evidencePromptSimhash(b) {
		t.Error("different last messages must yield different simhash (no false dup for continued conversation)")
	}
	c := []byte(`{"messages":[{"role":"user","content":"history one alpha"},{"role":"user","content":"harvest the exact same template prompt here"}]}`)
	d := []byte(`{"messages":[{"role":"user","content":"totally other history two beta"},{"role":"user","content":"harvest the exact same template prompt here"}]}`)
	if evidencePromptSimhash(c) != evidencePromptSimhash(d) {
		t.Error("same last message must yield same simhash (real repeated template must be detected)")
	}
}
