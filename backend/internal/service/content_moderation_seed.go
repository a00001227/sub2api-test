package service

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

// content_moderation_seed.go —— 本地基础词库“一键导入”（选项2）。
// 种子表内嵌于二进制（content_moderation_seed_keywords.txt），导入时与现有
// blocked_keywords 合并去重，不覆盖用户自定义词；云审仍在上层兜底。

//go:embed content_moderation_seed_keywords.txt
var contentModerationSeedRaw string

// ContentModerationImportResult 导入结果摘要。
type ContentModerationImportResult struct {
	Added  int                          `json:"added"`
	Total  int                          `json:"total"`
	Config *ContentModerationConfigView `json:"config"`
}

// parseSeedKeywords 解析种子表：逐行、去空白、跳过空行与 # 注释。
func parseSeedKeywords(raw string) []string {
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		out = append(out, ln)
	}
	return out
}

// ContentModerationSeedKeywordCount 返回内置种子词条数（供前端展示）。
func ContentModerationSeedKeywordCount() int {
	return len(parseSeedKeywords(contentModerationSeedRaw))
}

// ImportSeedKeywords 把内置基础词库合并进 blocked_keywords（追加去重），返回新增条数。
func (s *ContentModerationService) ImportSeedKeywords(ctx context.Context) (*ContentModerationImportResult, error) {
	cfg, err := s.loadConfig(ctx)
	if err != nil {
		return nil, err
	}
	before := normalizeBlockedKeywords(cfg.BlockedKeywords)
	beforeSet := make(map[string]struct{}, len(before))
	for _, kw := range before {
		beforeSet[strings.ToLower(kw)] = struct{}{}
	}
	merged := normalizeBlockedKeywords(append(append([]string{}, before...), parseSeedKeywords(contentModerationSeedRaw)...))
	added := 0
	for _, kw := range merged {
		if _, ok := beforeSet[strings.ToLower(kw)]; !ok {
			added++
		}
	}
	cfg.BlockedKeywords = merged
	cfg.normalize()
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal content moderation config: %w", err)
	}
	if err := s.settingRepo.Set(ctx, SettingKeyContentModerationConfig, string(raw)); err != nil {
		return nil, fmt.Errorf("save content moderation config: %w", err)
	}
	return &ContentModerationImportResult{Added: added, Total: len(merged), Config: s.configView(cfg)}, nil
}
