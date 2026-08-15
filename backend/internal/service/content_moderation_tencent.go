package service

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"

	tccommon "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
	tms "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/tms/v20201229"
)

// content_moderation_tencent.go —— 腾讯云文本审核（TMS TextModeration）适配。
// 把腾讯返回的 Label/Score 归一化成本系统的 CategoryScores（0..1），交给下游阈值判定。

// tencentLabelToCategory 腾讯 TMS 恶意标签 → 本系统合规分类键。
func tencentLabelToCategory(label string) string {
	switch strings.TrimSpace(label) {
	case "Porn", "Moan":
		return ContentModerationCategoryPorn
	case "Polity":
		return ContentModerationCategoryPolitical
	case "Illegal", "Custom":
		return ContentModerationCategoryContraband
	case "Terror":
		return ContentModerationCategoryTerror
	case "Abuse":
		return ContentModerationCategoryAbuse
	case "Ad":
		return ContentModerationCategoryAd
	default:
		return ""
	}
}

func (s *ContentModerationService) callTencentModeration(ctx context.Context, cfg *ContentModerationConfig, text string) (*moderationAPIResult, error) {
	if strings.TrimSpace(text) == "" {
		return vendorCategoryScores(map[string]float64{}), nil
	}
	if cfg.TencentSecretID == "" || cfg.TencentSecretKey == "" {
		return nil, errors.New("tencent moderation credentials not configured")
	}
	region := cfg.TencentRegion
	if region == "" {
		region = "ap-guangzhou"
	}
	credential := tccommon.NewCredential(cfg.TencentSecretID, cfg.TencentSecretKey)
	cpf := profile.NewClientProfile()
	timeoutSec := cfg.TimeoutMS / 1000
	if timeoutSec < 1 {
		timeoutSec = 1
	}
	cpf.HttpProfile.ReqTimeout = timeoutSec

	client, err := tms.NewClient(credential, region, cpf)
	if err != nil {
		return nil, err
	}
	req := tms.NewTextModerationRequest()
	req.Content = tccommon.StringPtr(base64.StdEncoding.EncodeToString([]byte(text)))
	req.Type = tccommon.StringPtr("TEXT")
	if cfg.TencentBizType != "" {
		req.BizType = tccommon.StringPtr(cfg.TencentBizType)
	}

	resp, err := client.TextModerationWithContext(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Response == nil {
		return nil, errors.New("tencent moderation returned empty response")
	}

	scores := map[string]float64{}
	added := false
	for _, d := range resp.Response.DetailResults {
		if d == nil || d.Label == nil {
			continue
		}
		cat := tencentLabelToCategory(*d.Label)
		if cat == "" {
			continue
		}
		var score float64 = 1.0
		if d.Score != nil {
			score = float64(*d.Score) / 100.0
		}
		putMaxScore(scores, cat, score)
		added = true
	}
	// DetailResults 为空时用顶层 Label/Score 兜底。
	if !added && resp.Response.Label != nil {
		if cat := tencentLabelToCategory(*resp.Response.Label); cat != "" {
			var score float64 = 1.0
			if resp.Response.Score != nil {
				score = float64(*resp.Response.Score) / 100.0
			}
			putMaxScore(scores, cat, score)
		}
	}
	// Suggestion=Block 但未映射到具体分类时，兜底记入 contraband，避免漏判。
	if len(scores) == 0 && resp.Response.Suggestion != nil && strings.EqualFold(*resp.Response.Suggestion, "Block") {
		putMaxScore(scores, ContentModerationCategoryContraband, 1.0)
	}
	return vendorCategoryScores(scores), nil
}
