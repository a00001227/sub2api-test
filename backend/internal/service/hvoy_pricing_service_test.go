package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type hvoyGroupListerStub struct{ groups []Group }

func (s *hvoyGroupListerStub) ListActive(context.Context) ([]Group, error) { return s.groups, nil }

type hvoySiteNamerStub struct{ name string }

func (s *hvoySiteNamerStub) GetSiteName(context.Context) string { return s.name }

func hvoyPtr(v float64) *float64  { return &v }
func hvoyStrPtr(v string) *string { return &v }

func newHvoyServiceForTest(t *testing.T, records []*PricingModelRecord, groups []Group) *HvoyPricingService {
	t.Helper()
	display := NewPricingDisplayService(&stubPricingModelRepo{enabled: records}, nil)
	return &HvoyPricingService{
		display: display,
		groups:  &hvoyGroupListerStub{groups: groups},
		site:    &hvoySiteNamerStub{name: "EiRouter"},
		now:     func() time.Time { return time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC) },
	}
}

// 价格 = 展示价(USD/1M) × 分组倍率 × 6.7;模型按平台归到对应分组,group_name 发分组名;未列出的分组不发;
// 图片模型与非终端用户价不发;顶层结构与 hvoy schema 1.1 一致。
func TestHvoyPricingService_Build(t *testing.T) {
	// 存储为 USD / token:claude 3/15/0.3/3.75 per 1M;gpt 1.25/10/0.125/0(无写价)
	records := []*PricingModelRecord{
		{Model: "claude-sonnet-4-5", ModelType: ModelTypeText, UserType: UserTypeEndUser, Enabled: true,
			InputPrice: hvoyPtr(3e-6), OutputPrice: hvoyPtr(15e-6), CacheReadPrice: hvoyPtr(0.3e-6), CacheWritePrice: hvoyPtr(3.75e-6), SortOrder: 1},
		{Model: "gpt-5", ModelType: ModelTypeText, UserType: UserTypeEndUser, Enabled: true,
			InputPrice: hvoyPtr(1.25e-6), OutputPrice: hvoyPtr(10e-6), CacheReadPrice: hvoyPtr(0.125e-6), SortOrder: 2},
		{Model: "gpt-image-1", ModelType: ModelTypeImage, UserType: UserTypeEndUser, Enabled: true,
			ImagePricingJSON: hvoyStrPtr(`{"1k":0.01}`), SortOrder: 3},
		{Model: "claude-opus-4-1", ModelType: ModelTypeText, UserType: UserTypeChannelUser, Enabled: true,
			InputPrice: hvoyPtr(15e-6), OutputPrice: hvoyPtr(75e-6), SortOrder: 4},
	}
	groups := []Group{
		{ID: 1, Name: "EiRouter-Claude-Standard", Slug: "eirouter-claude-standard", Platform: PlatformAnthropic, RateMultiplier: 1.0},
		{ID: 2, Name: "EiRouter-GPT-Standard", Slug: "eirouter-gpt-standard", Platform: PlatformOpenAI, RateMultiplier: 1.2},
		{ID: 3, Name: "Internal-Claude", Slug: "internal", Platform: PlatformAnthropic, RateMultiplier: 0.5},
	}
	svc := newHvoyServiceForTest(t, records, groups)

	resp, err := svc.Build(context.Background())
	require.NoError(t, err)
	require.Equal(t, "1.1", resp.SchemaVersion)
	require.True(t, resp.Success)
	require.Equal(t, "CNY", resp.Data.Currency)
	require.Equal(t, "per_1m_tokens", resp.Data.PriceUnit)
	require.Equal(t, "EiRouter", resp.Data.SiteName)
	require.Equal(t, "eirouter.ai", resp.Data.SiteDomain)
	require.Equal(t, "2026-09-16T08:00:00Z", resp.Data.UpdatedAt)

	require.Len(t, resp.Data.Models, 2)

	claude := resp.Data.Models[0]
	require.Equal(t, "claude-sonnet-4-5", claude.ModelName)
	require.Equal(t, "EiRouter-Claude-Standard", claude.GroupName)
	require.InDelta(t, 3*6.7, claude.InputPrice, 1e-9)
	require.InDelta(t, 15*6.7, *claude.OutputPrice, 1e-9)
	require.InDelta(t, 0.3*6.7, *claude.CacheInputPrice, 1e-9)
	require.InDelta(t, 3.75*6.7, *claude.CacheCreatePrice, 1e-9)
	require.Nil(t, claude.CacheCreatePrice1h) // 无 LiteLLM 目录 → null
	require.True(t, claude.Enabled)

	gpt := resp.Data.Models[1]
	require.Equal(t, "gpt-5", gpt.ModelName)
	require.Equal(t, "EiRouter-GPT-Standard", gpt.GroupName)
	require.InDelta(t, 1.25*1.2*6.7, gpt.InputPrice, 1e-9)
	require.InDelta(t, 10*1.2*6.7, *gpt.OutputPrice, 1e-9)
	require.Nil(t, gpt.CacheCreatePrice) // 未设写价 → null
	require.Nil(t, gpt.CacheCreatePrice1h)

	// JSON 字段名与 hvoy 规范一致,null 字段显式输出
	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	for _, key := range []string{`"schema_version":"1.1"`, `"model_name":"gpt-5"`, `"group_name":"EiRouter-GPT-Standard"`, `"cache_create_price":null`, `"cache_create_price_1h":null`, `"enabled":true`, `"note":""`} {
		require.Contains(t, string(raw), key)
	}
}

// 列出的分组不存在 / 不活跃 → 没有对应条目,但接口仍成功返回空列表。
func TestHvoyPricingService_Build_NoPublishedGroups(t *testing.T) {
	records := []*PricingModelRecord{
		{Model: "claude-sonnet-4-5", ModelType: ModelTypeText, UserType: UserTypeEndUser, Enabled: true, InputPrice: hvoyPtr(3e-6)},
	}
	svc := newHvoyServiceForTest(t, records, []Group{{ID: 9, Name: "Other", Slug: "other", Platform: PlatformAnthropic}})
	resp, err := svc.Build(context.Background())
	require.NoError(t, err)
	require.True(t, resp.Success)
	require.Empty(t, resp.Data.Models)
}
