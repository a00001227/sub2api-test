package service

import "testing"

func TestModerationInputText(t *testing.T) {
	if got := moderationInputText("hello 世界"); got != "hello 世界" {
		t.Fatalf("string input: got %q", got)
	}
	parts := []moderationAPIInputPart{
		{Type: "text", Text: "line1"},
		{Type: "image_url"},
		{Type: "text", Text: "line2"},
	}
	if got := moderationInputText(parts); got != "line1\nline2" {
		t.Fatalf("parts input: got %q", got)
	}
	if got := moderationInputText(123); got != "" {
		t.Fatalf("unknown input should be empty, got %q", got)
	}
}

func TestCloudConfigured(t *testing.T) {
	cases := []struct {
		name string
		cfg  ContentModerationConfig
		want bool
	}{
		{"openai empty", ContentModerationConfig{Provider: "openai"}, false},
		{"openai with key", ContentModerationConfig{Provider: "openai", APIKeys: []string{"sk-x"}}, true},
		{"aliyun missing secret", ContentModerationConfig{Provider: "aliyun", AliyunAccessKeyID: "id"}, false},
		{"aliyun ok", ContentModerationConfig{Provider: "aliyun", AliyunAccessKeyID: "id", AliyunAccessKeySecret: "sec"}, true},
		{"tencent ok", ContentModerationConfig{Provider: "tencent", TencentSecretID: "id", TencentSecretKey: "key"}, true},
		{"tencent missing", ContentModerationConfig{Provider: "tencent", TencentSecretID: "id"}, false},
	}
	for _, c := range cases {
		if got := c.cfg.cloudConfigured(); got != c.want {
			t.Errorf("%s: cloudConfigured=%v want %v", c.name, got, c.want)
		}
	}
}

func TestVendorLabelMappingFlags(t *testing.T) {
	thresholds := ContentModerationDefaultThresholds()

	// 阿里云政治标签 → political，应触发 flagged。
	if cat := aliyunLabelToCategory("political_content"); cat != ContentModerationCategoryPolitical {
		t.Fatalf("aliyun political map got %q", cat)
	}
	// 腾讯 Porn → porn。
	if cat := tencentLabelToCategory("Porn"); cat != ContentModerationCategoryPorn {
		t.Fatalf("tencent porn map got %q", cat)
	}
	// 归一到分类 + 阈值判定：political=1.0 应 flagged。
	scores := map[string]float64{ContentModerationCategoryPolitical: 1.0}
	flagged, cat, _ := evaluateModerationScores(scores, thresholds)
	if !flagged || cat != ContentModerationCategoryPolitical {
		t.Fatalf("political score should flag; flagged=%v cat=%q", flagged, cat)
	}
	// 干净（无命中）不应 flagged。
	flagged, _, _ = evaluateModerationScores(map[string]float64{}, thresholds)
	if flagged {
		t.Fatalf("empty scores should not flag")
	}
	// 广告默认阈值 0.99，单命中 1.0 才触发；低置信不触发。
	flagged, _, _ = evaluateModerationScores(map[string]float64{ContentModerationCategoryAd: 0.5}, thresholds)
	if flagged {
		t.Fatalf("ad 0.5 should not flag under 0.99 threshold")
	}
}

func TestModerationProviderDefault(t *testing.T) {
	cfg := &ContentModerationConfig{Provider: "  AliYun "}
	if got := cfg.moderationProvider(); got != ContentModerationProviderAliyun {
		t.Fatalf("provider normalize got %q", got)
	}
	cfg = &ContentModerationConfig{Provider: "unknown"}
	if got := cfg.moderationProvider(); got != ContentModerationProviderOpenAI {
		t.Fatalf("unknown provider should default openai, got %q", got)
	}
}
