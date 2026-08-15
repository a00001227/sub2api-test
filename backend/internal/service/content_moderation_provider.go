package service

import "strings"

// content_moderation_provider.go —— 云审厂商分派的公共辅助（无外部 SDK 依赖）。
// 阿里云/腾讯云的具体调用见 content_moderation_aliyun.go / content_moderation_tencent.go。

// moderationProvider 返回规范化后的厂商标识（未知→openai）。
func (cfg *ContentModerationConfig) moderationProvider() string {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case ContentModerationProviderAliyun:
		return ContentModerationProviderAliyun
	case ContentModerationProviderTencent:
		return ContentModerationProviderTencent
	default:
		return ContentModerationProviderOpenAI
	}
}

// cloudConfigured 判断当前厂商的云审凭据是否齐全。未配置→上层 fail-open 跳过（放行 + 记日志）。
func (cfg *ContentModerationConfig) cloudConfigured() bool {
	switch cfg.moderationProvider() {
	case ContentModerationProviderAliyun:
		return cfg.AliyunAccessKeyID != "" && cfg.AliyunAccessKeySecret != ""
	case ContentModerationProviderTencent:
		return cfg.TencentSecretID != "" && cfg.TencentSecretKey != ""
	default:
		return len(cfg.apiKeys()) > 0
	}
}

// moderationInputText 从 OpenAI 形态的审核输入（string 或 []moderationAPIInputPart）中抽出纯文本，
// 供只做文本审核的阿里云/腾讯云使用（图片部分忽略）。
func moderationInputText(input any) string {
	switch v := input.(type) {
	case string:
		return v
	case []moderationAPIInputPart:
		parts := make([]string, 0, len(v))
		for _, p := range v {
			if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
				parts = append(parts, p.Text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// vendorCategoryScores 把一组 (合规分类键 -> 置信度[0..1]) 收敛成 moderationAPIResult。
// flagged 由下游 evaluateModerationScores 依阈值重算，这里只带 CategoryScores。
func vendorCategoryScores(scores map[string]float64) *moderationAPIResult {
	if scores == nil {
		scores = map[string]float64{}
	}
	return &moderationAPIResult{CategoryScores: scores}
}

// putMaxScore 把 category 的分数取最大值写入 m（多标签命中同一分类时取最高）。
func putMaxScore(m map[string]float64, category string, score float64) {
	if category == "" {
		return
	}
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	if cur, ok := m[category]; !ok || score > cur {
		m[category] = score
	}
}
