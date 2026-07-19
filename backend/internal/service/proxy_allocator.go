package service

import (
	"context"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Phase 21E-6C-2B-1: Provider Connect 的自动代理分配。
//
// 独立于 ProxyService —— 那是 admin CRUD 面；这里是 Provider Connect
// 扩展层的选择策略，规则刻意保持第一版最简（region 匹配 + 绑定账号
// 数最少 + id ASC），不做 reputation/延迟权重/动态路由。
//
// 并发安全由 repository 层的 SELECT ... FOR UPDATE SKIP LOCKED 保证：
// 两个并发的同 region 请求会锁住各自的候选行并自动错开，不会长期
// 竞争同一个 proxy（见 ProxyAllocationRepository 实现）。

var (
	// ErrRegionRequired region 参数为空。
	ErrRegionRequired = infraerrors.BadRequest("REGION_REQUIRED", "region is required")
	// ErrRegionNoCapacity 该区域没有可分配的活跃代理。
	ErrRegionNoCapacity = infraerrors.NotFound("REGION_NO_CAPACITY", "no active proxy available in region")
)

// ProxyAllocationRepository 是分配器需要的最小仓储面。
// SelectLeastLoadedActiveProxyForUpdate 必须在单事务内以
// FOR UPDATE SKIP LOCKED 语义完成「筛选 + 锁定 + 返回」。
type ProxyAllocationRepository interface {
	SelectLeastLoadedActiveProxyForUpdate(ctx context.Context, region string) (*Proxy, error)
	// RegionCapacity 返回每个 region 的可用容量（未被占用的活跃代理数）。
	RegionCapacity(ctx context.Context) ([]RegionCapacity, error)
}

// RegionCapacity 是仓储层返回的单个 region 容量（未脱敏前只有 region+slots）。
type RegionCapacity struct {
	Region         string
	AvailableSlots int
}

// AvailableRegion 是对外（Portal → Provider）暴露的脱敏 region 能力项。
// 只含 region code / 展示名 / 可用槽位；绝不含 proxy_id / IP / host / 供应商。
type AvailableRegion struct {
	ID             string `json:"id"`              // region code（IATA-style，如 lax/sgp/nrt 或存量 US）
	Label          string `json:"label"`           // 展示名（城市），未知 code 回退为 code 本身
	Available      bool   `json:"available"`       // available_slots > 0
	AvailableSlots int    `json:"available_slots"` // 独占语义下的可导入账号数
}

// regionLabels 是 region code → 展示名的静态映射（IATA-style，对齐 DeRouter 的
// /proxy-regions）。未命中则 label 回退为 code 本身，不阻塞新增 region。
var regionLabels = map[string]string{
	"sgp": "Singapore", "atl": "Atlanta", "bom": "Mumbai", "cgk": "Jakarta",
	"jnb": "Johannesburg", "lax": "Los Angeles", "lon": "London", "mex": "Mexico City",
	"nrt": "Tokyo", "nyc": "New York", "pdx": "Portland", "syd": "Sydney",
	"tpe": "Taipei", "yyz": "Toronto",
	// 兼容存量粗粒度标签
	"US": "United States", "JP": "Japan", "SG": "Singapore", "EU": "Europe",
}

func regionLabelFor(code string) string {
	if l, ok := regionLabels[code]; ok {
		return l
	}
	return code
}

// ProxyAllocator 按 region 自动选择出口代理。
type ProxyAllocator struct {
	repo ProxyAllocationRepository
}

// NewProxyAllocator creates the allocator.
func NewProxyAllocator(repo ProxyAllocationRepository) *ProxyAllocator {
	return &ProxyAllocator{repo: repo}
}

// AvailableRegions 返回脱敏的 region 能力列表（含可用容量）。供 Portal
// 内部 API 透传给 Provider，前端据此禁用无容量项。
func (a *ProxyAllocator) AvailableRegions(ctx context.Context) ([]AvailableRegion, error) {
	caps, err := a.repo.RegionCapacity(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]AvailableRegion, 0, len(caps))
	for _, c := range caps {
		out = append(out, AvailableRegion{
			ID:             c.Region,
			Label:          regionLabelFor(c.Region),
			Available:      c.AvailableSlots > 0,
			AvailableSlots: c.AvailableSlots,
		})
	}
	return out, nil
}

// SelectProxy 为指定 region 选择一个活跃、未过期、当前绑定账号数最少的代理。
// region 大小写不敏感（存储约定大写标签，如 US/JP/SG/EU）。
// 无可用代理返回 ErrRegionNoCapacity —— 调用方不得静默降级为直连。
func (a *ProxyAllocator) SelectProxy(ctx context.Context, region string) (*Proxy, error) {
	region = strings.ToUpper(strings.TrimSpace(region))
	if region == "" {
		return nil, ErrRegionRequired
	}
	proxy, err := a.repo.SelectLeastLoadedActiveProxyForUpdate(ctx, region)
	if err != nil {
		return nil, err
	}
	if proxy == nil {
		return nil, ErrRegionNoCapacity
	}
	return proxy, nil
}
