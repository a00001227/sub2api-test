package service

import (
	"context"
	"math"
	"strings"
	"time"
)

// hvoy 价格接口(GET /api/provider/pricing):把我们「定价展示」里的模型价格按 hvoyai.com
// Provider Pricing API schema 1.1 的格式对外发布(https://hvoyai.com/docs/provider-price-api)。
//
// 口径:
//   - 价格源 = pricing_models(官网价格页那份,USD / token),× 分组 rate_multiplier(与计费同口径)
//     × 固定汇率 → CNY / 1M tokens。
//   - 每条 = model + group;只发 hvoyPricingGroups 里列出的分组(按 name 或 slug 匹配),
//     模型按平台归属分组(claude* → anthropic 组,gpt/codex/o* → openai 组)。
//   - 1 小时缓存写价:我们的表只有单档写价,按 LiteLLM 目录里该模型 1h/5m 官方比价折算;
//     目录没有的传 null。
//   - 只发文本模型(per_1m_tokens);图片模型暂不发布。
//
// 以下常量按运营决定硬编码(不走 env / 系统设置)。
const (
	hvoyPricingSchemaVersion = "1.1"
	hvoyPricingCurrency      = "CNY"
	hvoyPricingUnit          = "per_1m_tokens"
	hvoyPricingSiteDomain    = "eirouter.ai"
	// hvoyPricingUSDToCNY 发布价的 USD → CNY 汇率。
	hvoyPricingUSDToCNY = 6.7
)

// hvoyPricingGroup 一条对外发布的分组映射:Match 按我们的 group.Name 或 group.Slug 匹配,
// Publish 是发给 hvoy 的 group_name(机器匹配用的稳定标识,发布后不要改)。
// 运营决定直接用分组名对外发布。
type hvoyPricingGroup struct {
	Match   string
	Publish string
}

// hvoyPricingGroups 对外发布的分组(输出顺序即此顺序)。
var hvoyPricingGroups = []hvoyPricingGroup{
	{Match: "EiRouter-Claude-Standard", Publish: "EiRouter-Claude-Standard"},
	{Match: "EiRouter-GPT-Standard", Publish: "EiRouter-GPT-Standard"},
}

// HvoyPricingResponse 是 /api/provider/pricing 的完整响应体(字段顺序与 hvoy 示例一致)。
type HvoyPricingResponse struct {
	SchemaVersion string          `json:"schema_version"`
	Success       bool            `json:"success"`
	Message       string          `json:"message"`
	Data          HvoyPricingData `json:"data"`
}

// HvoyPricingData 是响应的 data 段。
type HvoyPricingData struct {
	Currency   string             `json:"currency"`
	PriceUnit  string             `json:"price_unit"`
	SiteName   string             `json:"site_name"`
	SiteDomain string             `json:"site_domain"`
	UpdatedAt  string             `json:"updated_at"`
	Models     []HvoyPricingModel `json:"models"`
}

// HvoyPricingModel 是 data.models[] 的一项(model + group)。价格单位 CNY / 1M tokens。
type HvoyPricingModel struct {
	ModelName          string   `json:"model_name"`
	GroupName          string   `json:"group_name"`
	InputPrice         float64  `json:"input_price"`
	OutputPrice        *float64 `json:"output_price"`
	CacheInputPrice    *float64 `json:"cache_input_price"`
	CacheCreatePrice   *float64 `json:"cache_create_price"`
	CacheCreatePrice1h *float64 `json:"cache_create_price_1h"`
	Enabled            bool     `json:"enabled"`
	Note               string   `json:"note"`
}

// hvoyGroupLister / hvoySiteNamer 是本服务需要的最小依赖面(便于单测)。
type hvoyGroupLister interface {
	ListActive(ctx context.Context) ([]Group, error)
}

type hvoySiteNamer interface {
	GetSiteName(ctx context.Context) string
}

// HvoyPricingService 组装 hvoy 格式的价格数据。
type HvoyPricingService struct {
	display *PricingDisplayService
	groups  hvoyGroupLister
	site    hvoySiteNamer
	now     func() time.Time
}

// NewHvoyPricingService 构造 HvoyPricingService。
func NewHvoyPricingService(display *PricingDisplayService, groupRepo GroupRepository, settings *SettingService) *HvoyPricingService {
	return &HvoyPricingService{display: display, groups: groupRepo, site: settings, now: time.Now}
}

// Build 生成当前的 hvoy 价格响应。
func (s *HvoyPricingService) Build(ctx context.Context) (*HvoyPricingResponse, error) {
	items, err := s.display.GetPublicPricingDisplay(ctx)
	if err != nil {
		return nil, err
	}
	activeGroups, err := s.groups.ListActive(ctx)
	if err != nil {
		return nil, err
	}

	models := make([]HvoyPricingModel, 0, len(items)*len(hvoyPricingGroups))
	for _, sel := range s.selectGroups(activeGroups) {
		for i := range items {
			item := &items[i]
			// 只发文本模型的终端用户价;图片模型(per_call)暂不发布。
			if item.Pricing.Text == nil || item.UserType != UserTypeEndUser {
				continue
			}
			if PlatformForModelName(item.Model) != sel.group.Platform {
				continue
			}
			models = append(models, s.buildModel(item, sel.group, sel.publish))
		}
	}

	siteName := hvoyPricingSiteDomain
	if s.site != nil {
		if name := strings.TrimSpace(s.site.GetSiteName(ctx)); name != "" {
			siteName = name
		}
	}

	return &HvoyPricingResponse{
		SchemaVersion: hvoyPricingSchemaVersion,
		Success:       true,
		Message:       "",
		Data: HvoyPricingData{
			Currency:   hvoyPricingCurrency,
			PriceUnit:  hvoyPricingUnit,
			SiteName:   siteName,
			SiteDomain: hvoyPricingSiteDomain,
			UpdatedAt:  s.now().UTC().Format(time.RFC3339),
			Models:     models,
		},
	}, nil
}

type hvoySelectedGroup struct {
	group   Group
	publish string
}

// selectGroups 按 hvoyPricingGroups 的顺序挑出要发布的活跃分组(name 或 slug 匹配,大小写不敏感)。
func (s *HvoyPricingService) selectGroups(active []Group) []hvoySelectedGroup {
	out := make([]hvoySelectedGroup, 0, len(hvoyPricingGroups))
	for _, want := range hvoyPricingGroups {
		for i := range active {
			g := active[i]
			if strings.EqualFold(g.Name, want.Match) || strings.EqualFold(g.Slug, want.Match) {
				out = append(out, hvoySelectedGroup{group: g, publish: want.Publish})
				break
			}
		}
	}
	return out
}

// buildModel 把一条展示价(USD / 1M tokens)换算成该分组的 CNY / 1M tokens。
func (s *HvoyPricingService) buildModel(item *PricingDisplayItem, group Group, groupName string) HvoyPricingModel {
	multiplier := group.RateMultiplier
	if multiplier <= 0 {
		multiplier = 1
	}
	factor := multiplier * hvoyPricingUSDToCNY
	text := item.Pricing.Text

	m := HvoyPricingModel{
		ModelName:  item.Model,
		GroupName:  groupName,
		InputPrice: hvoyRound(text.InputPrice * factor),
		Enabled:    true,
		Note:       "",
	}
	m.OutputPrice = hvoyPricePtr(text.OutputPrice, factor)
	m.CacheInputPrice = hvoyPricePtr(text.CacheReadPrice, factor)
	m.CacheCreatePrice = hvoyPricePtr(text.CacheWritePrice, factor)
	m.CacheCreatePrice1h = s.cacheCreate1h(item.Model, text.CacheWritePrice, factor)
	return m
}

// cacheCreate1h 按 LiteLLM 目录里该模型「1h 写 / 5m 写」的官方比价,把我们的单档缓存写价折算成
// 1 小时写价;目录没有该模型、或没有 1h 档(如 GPT)时返回 null。
func (s *HvoyPricingService) cacheCreate1h(model string, cacheWriteUSDPer1M, factor float64) *float64 {
	if cacheWriteUSDPer1M <= 0 || s.display == nil || s.display.pricing == nil {
		return nil
	}
	catalog := s.display.pricing.GetModelPricing(model)
	if catalog == nil || catalog.CacheCreationInputTokenCost <= 0 || catalog.CacheCreationInputTokenCostAbove1hr <= catalog.CacheCreationInputTokenCost {
		return nil
	}
	ratio := catalog.CacheCreationInputTokenCostAbove1hr / catalog.CacheCreationInputTokenCost
	v := hvoyRound(cacheWriteUSDPer1M * ratio * factor)
	return &v
}

// hvoyPricePtr 0 视为"未提供"→ null;否则换算并四舍五入。
func hvoyPricePtr(usdPer1M, factor float64) *float64 {
	if usdPer1M <= 0 {
		return nil
	}
	v := hvoyRound(usdPer1M * factor)
	return &v
}

// hvoyRound 保留 4 位小数(CNY / 1M tokens,足够表达 0.0001 元级别的缓存价)。
func hvoyRound(v float64) float64 {
	return math.Round(v*10000) / 10000
}
