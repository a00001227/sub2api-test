package service

import "testing"

// simhash 归一化/坍缩单测。ComputeMessagesSimhash 仍被 Risk V2 使用（risk_simhash.go），
// 故随 v1 摘除时把这几个用例从原 risk_scoring_test.go 迁移到此处保留覆盖。

func TestRiskSimhashTemplateCollision(t *testing.T) {
	// 同模板、仅数字槽位不同 → 归一化后应完全一致 → simhash 相同。
	a := []byte(`[{"role":"user","content":"Translate order 12345 shipped on 2024-01-02"}]`)
	b := []byte(`[{"role":"user","content":"Translate order 98765 shipped on 2025-11-30"}]`)
	ha := ComputeMessagesSimhash(a)
	hb := ComputeMessagesSimhash(b)
	if ha == 0 || hb == 0 {
		t.Fatalf("simhash should be non-zero: ha=%d hb=%d", ha, hb)
	}
	if ha != hb {
		t.Errorf("same template different digits should collide: ha=%d hb=%d (hamming=%d)", ha, hb, hammingDistance64(ha, hb))
	}
}

func TestRiskSimhashCaseAndWhitespaceCollision(t *testing.T) {
	a := []byte(`[{"role":"user","content":"HELLO   WORLD"}]`)
	b := []byte(`[{"role":"user","content":"hello world"}]`)
	if ComputeMessagesSimhash(a) != ComputeMessagesSimhash(b) {
		t.Errorf("case/whitespace should normalize to same hash")
	}
}

func TestRiskSimhashDifferentContentDiverges(t *testing.T) {
	a := []byte(`[{"role":"user","content":"write a poem about the ocean and its waves"}]`)
	b := []byte(`[{"role":"user","content":"explain quantum entanglement in simple terms"}]`)
	ha := ComputeMessagesSimhash(a)
	hb := ComputeMessagesSimhash(b)
	if ha == hb {
		t.Errorf("clearly different content should not collide")
	}
	if d := hammingDistance64(ha, hb); d < 5 {
		t.Errorf("expected meaningful hamming distance, got %d", d)
	}
}

func TestRiskSimhashEmptyIsZero(t *testing.T) {
	if ComputeMessagesSimhash(nil) != 0 {
		t.Errorf("nil input should be 0")
	}
	if ComputeMessagesSimhash([]byte("   ")) != 0 {
		t.Errorf("whitespace-only should be 0")
	}
}

func TestNormalizeForSimhashCapsAt8KB(t *testing.T) {
	big := make([]byte, 20*1024)
	for i := range big {
		big[i] = 'a'
	}
	n := normalizeForSimhash(big)
	if len(n) > riskSimhashMaxInput {
		t.Errorf("normalized should be capped near 8KB, got %d", len(n))
	}
}
