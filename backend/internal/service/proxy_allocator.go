package service

import (
	"context"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"golang.org/x/sync/singleflight"
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
	RegionZh       string
	AvailableSlots int
}

// RegionCapacityTier 是脱敏后的容量档位（对外只暴露档位，不暴露精确名额，
// 避免 provider 侧推断出平台的代理容量底细）。
type RegionCapacityTier string

const (
	CapacityFull    RegionCapacityTier = "full"    // 0 名额：已满，不可选
	CapacityLimited RegionCapacityTier = "limited" // 1–5 名额：紧张
	CapacityAmple   RegionCapacityTier = "ample"   // >5 名额：充足
)

// capacityLimitedThreshold 是 limited/ample 的分界：<= 该值为紧张。
const capacityLimitedThreshold = 5

// capacityTierFromSlots 把精确名额映射为脱敏档位。
func capacityTierFromSlots(slots int) RegionCapacityTier {
	switch {
	case slots <= 0:
		return CapacityFull
	case slots <= capacityLimitedThreshold:
		return CapacityLimited
	default:
		return CapacityAmple
	}
}

// AvailableRegion 是对外（Portal → Provider）暴露的脱敏 region 能力项。
// 只含 region code / 展示名 / 容量档位；绝不含 proxy_id / IP / host / 供应商，
// 也不含精确名额数字（只给 ample/limited/full 档位，防止容量底细外泄）。
type AvailableRegion struct {
	ID        string             `json:"id"`       // region code（IATA-style，如 lax/sgp/nrt 或存量 US）
	Label     string             `json:"label"`    // 展示名（城市），未知 code 回退为 code 本身
	LabelZh   string             `json:"label_zh"` // 中文展示名（探测回写的 region_zh），无则回退英文 Label
	Available bool               `json:"available"`// 是否还有名额（capacity != full）
	Capacity  RegionCapacityTier `json:"capacity"` // 脱敏容量档位：ample / limited / full
	// AvailableSlots 是内部精确名额，仅供服务内/测试使用；json:"-" 确保它
	// 绝不出网关（脱敏边界）。
	AvailableSlots int `json:"-"`
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

// availableRegionsCacheTTL 是 AvailableRegions 展示结果的进程内缓存时长。
// available-regions 是纯展示型只读数据、天然弱一致（真正的容量仲裁在授权
// 落库时由 FOR UPDATE + 二次校验强一致保证），因此缓存不引入业务风险。
// 上千并发点击下，缓存把 DB 读压降到"每实例每 TTL 至多一次"。
const availableRegionsCacheTTL = 15 * time.Second

// availableRegionsCacheKey 单条全局键：available-regions 无参数、全局唯一。
const availableRegionsCacheKey = "available-regions"

// regionCapacityCache 是 AvailableRegions 的进程内 TTL 缓存 + singleflight
// 防击穿。刻意做成内嵌小结构而非 Redis：available-regions 是全局只读数据，
// 进程内缓存在多实例下最坏也只是"每实例每 TTL 查一次库"，代价可忽略，且
// 零新依赖、不动 wire DI。future：若需跨实例共享一份，可把 load 后的写入
// 改接 Redis（对外行为不变）。
type regionCapacityCache struct {
	ttl   time.Duration
	sf    singleflight.Group
	mu    sync.RWMutex
	data  []AvailableRegion
	expAt time.Time
}

func newRegionCapacityCache(ttl time.Duration) *regionCapacityCache {
	if ttl <= 0 {
		ttl = availableRegionsCacheTTL
	}
	return &regionCapacityCache{ttl: ttl}
}

// get 返回未过期的缓存快照。第二返回值表示是否命中。
func (c *regionCapacityCache) get() ([]AvailableRegion, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.data == nil || time.Now().After(c.expAt) {
		return nil, false
	}
	return c.data, true
}

func (c *regionCapacityCache) set(v []AvailableRegion) {
	c.mu.Lock()
	c.data = v
	c.expAt = time.Now().Add(c.ttl)
	c.mu.Unlock()
}

// getOrLoad 命中即返回；未命中时用 singleflight 保证并发下只有一个
// goroutine 真正 load（查库），其余共享其结果 —— 防止 TTL 到期瞬间的
// 缓存击穿。load 出错不写缓存，直接透传错误（fail-open：调用方仍可自行
// 决定降级）。
func (c *regionCapacityCache) getOrLoad(
	load func() ([]AvailableRegion, error),
) ([]AvailableRegion, error) {
	if v, ok := c.get(); ok {
		return v, nil
	}
	res, err, _ := c.sf.Do(availableRegionsCacheKey, func() (interface{}, error) {
		// 双检：等锁期间可能已有别的请求填好缓存。
		if v, ok := c.get(); ok {
			return v, nil
		}
		v, err := load()
		if err != nil {
			return nil, err
		}
		c.set(v)
		return v, nil
	})
	if err != nil {
		return nil, err
	}
	return res.([]AvailableRegion), nil
}

// ProxyAllocator 按 region 自动选择出口代理。
type ProxyAllocator struct {
	repo  ProxyAllocationRepository
	cache *regionCapacityCache
}

// NewProxyAllocator creates the allocator.
func NewProxyAllocator(repo ProxyAllocationRepository) *ProxyAllocator {
	return &ProxyAllocator{
		repo:  repo,
		cache: newRegionCapacityCache(availableRegionsCacheTTL),
	}
}

// AvailableRegions 返回脱敏的 region 能力列表（含可用容量）。供 Portal
// 内部 API 透传给 Provider，前端据此禁用无容量项。
//
// 结果走进程内 TTL 缓存 + singleflight（见 regionCapacityCache）：这是纯
// 展示型只读、弱一致数据，缓存不影响正确性（强一致仲裁在授权落库时），
// 却能在上千并发点击下把 DB 读压降到每实例每 TTL 至多一次。
func (a *ProxyAllocator) AvailableRegions(ctx context.Context) ([]AvailableRegion, error) {
	return a.cache.getOrLoad(func() ([]AvailableRegion, error) {
		caps, err := a.repo.RegionCapacity(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]AvailableRegion, 0, len(caps))
		for _, c := range caps {
			label := regionLabelFor(c.Region)
			labelZh := c.RegionZh
			if labelZh == "" {
				labelZh = label
			}
			out = append(out, AvailableRegion{
				ID:             c.Region,
				Label:          label,
				LabelZh:        labelZh,
				Available:      c.AvailableSlots > 0,
				Capacity:       capacityTierFromSlots(c.AvailableSlots),
				AvailableSlots: c.AvailableSlots,
			})
		}
		return out, nil
	})
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
