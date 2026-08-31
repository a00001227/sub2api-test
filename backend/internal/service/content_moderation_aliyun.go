package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	green "github.com/alibabacloud-go/green-20220302/v2/client"
	"github.com/alibabacloud-go/tea/tea"
)

// content_moderation_aliyun.go —— 阿里云内容安全（green-20220302 TextModeration）适配。
// 阿里返回逗号分隔的风险 labels，映射成本系统合规分类键（命中即视为高置信 1.0）。

const defaultAliyunModerationService = "chat_detection"

// aliyunLabelToCategory 阿里 label → 本系统合规分类键。
func aliyunLabelToCategory(label string) string {
	l := strings.ToLower(strings.TrimSpace(label))
	switch {
	case strings.Contains(l, "politic"):
		return ContentModerationCategoryPolitical
	case strings.Contains(l, "sexual") || strings.Contains(l, "porn"):
		return ContentModerationCategoryPorn
	case strings.Contains(l, "violence") || strings.Contains(l, "terror"):
		return ContentModerationCategoryTerror
	case strings.Contains(l, "contraband") || l == "c_customized" || strings.Contains(l, "customized"):
		return ContentModerationCategoryContraband
	case strings.Contains(l, "profanity") || strings.Contains(l, "cyberbullying") || strings.Contains(l, "negative"):
		return ContentModerationCategoryAbuse
	case l == "ad" || strings.Contains(l, "spam"):
		return ContentModerationCategoryAd
	default:
		return ""
	}
}

func (s *ContentModerationService) callAliyunModeration(ctx context.Context, cfg *ContentModerationConfig, text string) (*moderationAPIResult, error) {
	if strings.TrimSpace(text) == "" {
		return vendorCategoryScores(map[string]float64{}), nil
	}
	if cfg.AliyunAccessKeyID == "" || cfg.AliyunAccessKeySecret == "" {
		return nil, errors.New("aliyun moderation credentials not configured")
	}
	region := cfg.AliyunRegion
	if region == "" {
		region = "cn-shanghai"
	}
	endpoint := cfg.AliyunEndpoint
	if endpoint == "" {
		endpoint = fmt.Sprintf("green-cip.%s.aliyuncs.com", region)
	}
	svc := cfg.AliyunService
	if svc == "" {
		svc = defaultAliyunModerationService
	}

	config := &openapi.Config{
		AccessKeyId:     tea.String(cfg.AliyunAccessKeyID),
		AccessKeySecret: tea.String(cfg.AliyunAccessKeySecret),
		RegionId:        tea.String(region),
		Endpoint:        tea.String(endpoint),
		ReadTimeout:     tea.Int(cfg.TimeoutMS),
		ConnectTimeout:  tea.Int(cfg.TimeoutMS),
	}
	client, err := green.NewClient(config)
	if err != nil {
		return nil, err
	}

	params, err := json.Marshal(map[string]string{"content": text})
	if err != nil {
		return nil, err
	}

	scores := map[string]float64{}
	if aliyunServiceUsesPlus(svc) {
		// 大模型审核服务（byllm/llm/*_pro，如 ugc_moderation_byllm_ec）走 TextModerationPlus:
		// 与经典 TextModeration 请求同形（Service + ServiceParameters），但响应是 Data.Result 数组。
		resp, err := client.TextModerationPlus(&green.TextModerationPlusRequest{
			Service:           tea.String(svc),
			ServiceParameters: tea.String(string(params)),
		})
		if err != nil {
			return nil, err
		}
		if resp == nil || resp.Body == nil {
			return nil, errors.New("aliyun moderation returned empty response")
		}
		if resp.Body.Code != nil && *resp.Body.Code != 200 {
			msg := ""
			if resp.Body.Message != nil {
				msg = *resp.Body.Message
			}
			return nil, fmt.Errorf("aliyun moderation code %d: %s", *resp.Body.Code, msg)
		}
		if resp.Body.Data != nil {
			for _, r := range resp.Body.Data.Result {
				if r == nil || r.Label == nil {
					continue
				}
				if cat := aliyunLabelToCategory(*r.Label); cat != "" {
					score := 1.0
					if r.Confidence != nil {
						score = float64(*r.Confidence) / 100.0
					}
					putMaxScore(scores, cat, score)
				}
			}
		}
		return vendorCategoryScores(scores), nil
	}

	// 经典审核服务走 TextModeration（响应为逗号分隔的 Data.Labels）。
	resp, err := client.TextModeration(&green.TextModerationRequest{
		Service:           tea.String(svc),
		ServiceParameters: tea.String(string(params)),
	})
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Body == nil {
		return nil, errors.New("aliyun moderation returned empty response")
	}
	if resp.Body.Code != nil && *resp.Body.Code != 200 {
		msg := ""
		if resp.Body.Message != nil {
			msg = *resp.Body.Message
		}
		return nil, fmt.Errorf("aliyun moderation code %d: %s", *resp.Body.Code, msg)
	}
	if resp.Body.Data != nil && resp.Body.Data.Labels != nil {
		for _, label := range strings.Split(*resp.Body.Data.Labels, ",") {
			if cat := aliyunLabelToCategory(label); cat != "" {
				putMaxScore(scores, cat, 1.0)
			}
		}
	}
	return vendorCategoryScores(scores), nil
}

// aliyunServiceUsesPlus 判断服务是否走大模型审核接口 TextModerationPlus:
// byllm/llm 系列（ugc_moderation_byllm_ec、llm_query_moderation、llm_response_moderation）
// 及 *_pro 专业版（chat_detection_pro 等）。其余经典服务走 TextModeration。
func aliyunServiceUsesPlus(service string) bool {
	s := strings.ToLower(strings.TrimSpace(service))
	return strings.Contains(s, "llm") || strings.HasSuffix(s, "_pro")
}
