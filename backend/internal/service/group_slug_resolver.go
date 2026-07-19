package service

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// groupSlugReserved 保留字集合：分组 slug 不得与系统路由的首段冲突，
// 否则前缀重写会劫持系统端点。管理端创建/更新分组时校验。
var groupSlugReserved = map[string]struct{}{
	"api": {}, "v1": {}, "v1beta": {}, "health": {}, "setup": {},
	"responses": {}, "chat": {}, "embeddings": {}, "images": {},
	"messages": {}, "backend-api": {}, "antigravity": {},
	"balance": {}, "sub-key": {}, "sub-keys": {}, "usage-logs": {},
	"assets": {}, "static": {}, "favicon.ico": {}, "index.html": {},
	"admin": {}, "login": {}, "register": {}, "docs": {}, "public": {},
	"ws": {}, "metrics": {}, "robots.txt": {},
}

// groupSlugPattern slug 格式：小写字母/数字开头结尾，中间可含连字符，1-64 位。
var groupSlugPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

// IsReservedGroupSlug 判断 slug 是否为路由保留字。
func IsReservedGroupSlug(slug string) bool {
	_, ok := groupSlugReserved[strings.ToLower(strings.TrimSpace(slug))]
	return ok
}

// ValidateGroupSlug 校验 slug 格式与保留字。空串合法（表示不开放 URL 选组）。
func ValidateGroupSlug(slug string) error {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil
	}
	if !groupSlugPattern.MatchString(slug) {
		return ErrInvalidGroupSlug
	}
	if IsReservedGroupSlug(slug) {
		return ErrReservedGroupSlug
	}
	return nil
}

// groupSlugCacheTTL slug→分组 全量缓存的刷新周期。分组数量小（几十个量级），
// 全量加载一次成本极低；管理员改 slug 后最多 15 秒生效。
const groupSlugCacheTTL = 15 * time.Second

// GroupSlugResolver 将 URL slug 解析为分组，供前缀重写中间件在路由前使用。
// 内部维护「全部启用分组中带 slug 的」内存快照，避免每个请求打 DB。
type GroupSlugResolver struct {
	groupRepo GroupRepository

	mu        sync.RWMutex
	bySlug    map[string]Group // 值拷贝存储；Resolve 返回逐请求副本
	expiresAt time.Time

	sf singleflight.Group
}

// NewGroupSlugResolver 创建 slug 解析器，并登记为进程内活动实例，
// 供 admin 分组增删改后立即失效缓存（见 invalidateActiveGroupSlugResolver）。
func NewGroupSlugResolver(groupRepo GroupRepository) *GroupSlugResolver {
	r := &GroupSlugResolver{groupRepo: groupRepo}
	activeGroupSlugResolver.Store(r)
	return r
}

// activeGroupSlugResolver 进程内活动的解析器实例。admin service 不直接持有
// 解析器（避免构造函数连锁改动），通过该指针在分组变更后触发缓存失效。
var activeGroupSlugResolver atomic.Pointer[GroupSlugResolver]

// invalidateActiveGroupSlugResolver 分组增删改后调用：立即失效 slug 缓存，
// 让 URL 前缀路由即时反映变更（否则最长等待一个 TTL 周期）。
func invalidateActiveGroupSlugResolver() {
	if r := activeGroupSlugResolver.Load(); r != nil {
		r.Invalidate()
	}
}

// Resolve 按 slug 查找启用中的分组。返回的 *Group 是每次调用的独立副本，
// 调用方（认证中间件）可安全写入 apiKey.Group 而不会污染共享缓存。
// 未命中返回 (nil, nil)；仅在缓存为空且回源失败时返回错误。
func (r *GroupSlugResolver) Resolve(ctx context.Context, slug string) (*Group, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return nil, nil
	}

	m, err := r.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	g, ok := m[slug]
	if !ok {
		return nil, nil
	}
	// 逐请求副本（浅拷贝；map/slice 字段全链路只读）
	cp := g
	return &cp, nil
}

// HasSlug 快速判断某段路径是否是已注册的 slug（重写中间件的热路径入口）。
func (r *GroupSlugResolver) HasSlug(ctx context.Context, slug string) bool {
	m, err := r.snapshot(ctx)
	if err != nil {
		return false
	}
	_, ok := m[strings.ToLower(slug)]
	return ok
}

// Invalidate 立即失效缓存（管理员创建/更新/删除分组后调用）。
func (r *GroupSlugResolver) Invalidate() {
	r.mu.Lock()
	r.expiresAt = time.Time{}
	r.mu.Unlock()
}

func (r *GroupSlugResolver) snapshot(ctx context.Context) (map[string]Group, error) {
	r.mu.RLock()
	if time.Now().Before(r.expiresAt) {
		m := r.bySlug
		r.mu.RUnlock()
		return m, nil
	}
	stale := r.bySlug
	r.mu.RUnlock()

	value, err, _ := r.sf.Do("reload", func() (any, error) {
		groups, err := r.groupRepo.ListActive(ctx)
		if err != nil {
			return nil, err
		}
		m := make(map[string]Group, len(groups))
		for i := range groups {
			s := strings.ToLower(strings.TrimSpace(groups[i].Slug))
			if s == "" {
				continue
			}
			m[s] = groups[i]
		}
		r.mu.Lock()
		r.bySlug = m
		r.expiresAt = time.Now().Add(groupSlugCacheTTL)
		r.mu.Unlock()
		return m, nil
	})
	if err != nil {
		// 回源失败：有旧快照就降级用旧的，避免 DB 抖动打断前缀路由
		if stale != nil {
			return stale, nil
		}
		return nil, err
	}
	return value.(map[string]Group), nil
}
