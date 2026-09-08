package service

import (
	"context"
	"errors"
	"testing"
)

// stubPricingModelRepo 是最小 PricingModelRepository:只用于喂 ListEnabled。
type stubPricingModelRepo struct {
	enabled []*PricingModelRecord
	err     error
}

func (s *stubPricingModelRepo) Create(context.Context, *PricingModelRecord) error { return nil }
func (s *stubPricingModelRepo) Update(context.Context, *PricingModelRecord) error { return nil }
func (s *stubPricingModelRepo) Delete(context.Context, int64) error               { return nil }
func (s *stubPricingModelRepo) GetByID(context.Context, int64) (*PricingModelRecord, error) {
	return nil, nil
}
func (s *stubPricingModelRepo) List(context.Context) ([]*PricingModelRecord, error) { return nil, nil }
func (s *stubPricingModelRepo) ListEnabled(context.Context) ([]*PricingModelRecord, error) {
	return s.enabled, s.err
}
func (s *stubPricingModelRepo) Reorder(context.Context, []int64) (int, error) { return 0, nil }

func TestPlatformForModelName(t *testing.T) {
	cases := map[string]string{
		"gpt-6-astra":       PlatformOpenAI,
		"gpt-5.6-sol":       PlatformOpenAI,
		"gpt-5.6-terra":     PlatformOpenAI,
		"gpt-image-2":       PlatformOpenAI,
		"codex-auto-review": PlatformOpenAI,
		"claude-fable-5-1":  PlatformAnthropic,
		"claude-opus-4-8":   PlatformAnthropic,
		"gemini-2.5-pro":    PlatformGemini,
		"some-unknown-x":    "", // 认不出的前缀不归任何平台
	}
	for model, want := range cases {
		if got := PlatformForModelName(model); got != want {
			t.Errorf("PlatformForModelName(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestEnabledModelIDsForPlatform_FiltersByPlatformAndOrder(t *testing.T) {
	repo := &stubPricingModelRepo{enabled: []*PricingModelRecord{
		{Model: "gpt-6-astra", Enabled: true},
		{Model: "claude-fable-5-1", Enabled: true},
		{Model: "gpt-5.6-sol", Enabled: true},
		{Model: "gpt-5.6-sol", Enabled: true},   // 重复应去重
		{Model: "  ", Enabled: true},            // 空名跳过
		{Model: "mystery-model", Enabled: true}, // 认不出平台 → openai 查询里不出现
	}}
	s := NewPricingDisplayService(repo, nil)

	got := s.EnabledModelIDsForPlatform(context.Background(), PlatformOpenAI)
	want := []string{"gpt-6-astra", "gpt-5.6-sol"}
	if len(got) != len(want) {
		t.Fatalf("openai candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("openai candidates = %v, want %v (order matters)", got, want)
		}
	}

	if cl := s.EnabledModelIDsForPlatform(context.Background(), PlatformAnthropic); len(cl) != 1 || cl[0] != "claude-fable-5-1" {
		t.Fatalf("anthropic candidates = %v, want [claude-fable-5-1]", cl)
	}
}

func TestEnabledModelIDsForPlatform_FailOpenNilOnRepoError(t *testing.T) {
	s := NewPricingDisplayService(&stubPricingModelRepo{err: errors.New("db down")}, nil)
	if got := s.EnabledModelIDsForPlatform(context.Background(), PlatformOpenAI); got != nil {
		t.Fatalf("repo 出错应返回 nil(让上层回退硬编码),got: %v", got)
	}
}
