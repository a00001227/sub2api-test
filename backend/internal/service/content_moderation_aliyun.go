package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	// 护栏无风险标记：query_security_check 每个检测项无风险回 nonLabel（敏感数据回 "0"）。
	if l == "" || l == "nonlabel" || l == "0" || l == "none" {
		return ""
	}
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
	case strings.Contains(l, "prompt") || strings.Contains(l, "attack") || strings.Contains(l, "jailbreak") || strings.Contains(l, "inject"):
		// AI 安全护栏提示词攻击（越狱/注入/指令攻击）。
		return ContentModerationCategoryPromptAttack
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
			// 内容合规（Result[]）：映射到内容分类；未识别的非空标签仅打点。
			for _, r := range resp.Body.Data.Result {
				if r == nil || r.Label == nil {
					continue
				}
				cat := aliyunLabelToCategory(*r.Label)
				if cat == "" {
					if ll := strings.ToLower(strings.TrimSpace(*r.Label)); ll != "" && ll != "nonlabel" && ll != "0" && ll != "none" {
						slog.Info("content_moderation.aliyun_unmapped_label", "service", svc, "label", *r.Label)
					}
					continue
				}
				score := 1.0
				if r.Confidence != nil {
					score = float64(*r.Confidence) / 100.0
				}
				putMaxScore(scores, cat, score)
			}
			// 提示词攻击在独立的 AttackResult[]（护栏 query/response_security_check 返回；
			// 其它服务为空，循环空转）。AttackLevel != none 即命中，置信度作分值。
			for _, a := range resp.Body.Data.AttackResult {
				if a == nil {
					continue
				}
				lvl := ""
				if a.AttackLevel != nil {
					lvl = strings.ToLower(strings.TrimSpace(*a.AttackLevel))
				}
				if lvl == "" || lvl == "none" {
					continue
				}
				// 检出攻击即按命中处理（fail-closed，low/medium/high 都拦）；置信度仅记录。
				label := ""
				if a.Label != nil {
					label = *a.Label
				}
				conf := float32(0)
				if a.Confidence != nil {
					conf = *a.Confidence
				}
				slog.Info("content_moderation.aliyun_attack_hit", "service", svc, "label", label, "level", lvl, "confidence", conf)
				putMaxScore(scores, ContentModerationCategoryPromptAttack, 1.0)
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
	// llm/_pro：内容审核大模型与专业版；security_check：AI 安全护栏
	// query_security_check / response_security_check（含 _intl）同样走 TextModerationPlus。
	return strings.Contains(s, "llm") || strings.HasSuffix(s, "_pro") || strings.Contains(s, "security_check")
}
