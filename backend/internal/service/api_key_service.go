package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/dgraph-io/ristretto"
	"golang.org/x/sync/singleflight"
)

var (
	ErrAPIKeyNotFound     = infraerrors.NotFound("API_KEY_NOT_FOUND", "api key not found")
	ErrGroupNotAllowed    = infraerrors.Forbidden("GROUP_NOT_ALLOWED", "user is not allowed to bind this group")
	ErrAPIKeyExists       = infraerrors.Conflict("API_KEY_EXISTS", "api key already exists")
	ErrAPIKeyTooShort     = infraerrors.BadRequest("API_KEY_TOO_SHORT", "api key must be at least 16 characters")
	ErrAPIKeyInvalidChars = infraerrors.BadRequest("API_KEY_INVALID_CHARS", "api key can only contain letters, numbers, underscores, and hyphens")
	ErrAPIKeyRateLimited  = infraerrors.TooManyRequests("API_KEY_RATE_LIMITED", "too many failed attempts, please try again later")
	ErrInvalidIPPattern   = infraerrors.BadRequest("INVALID_IP_PATTERN", "invalid IP or CIDR pattern")
	// ErrAPIKeyExpired        = infraerrors.Forbidden("API_KEY_EXPIRED", "api key has expired")
	ErrAPIKeyExpired = infraerrors.Forbidden("API_KEY_EXPIRED", "api key 已过期")
	// ErrAPIKeyQuotaExhausted = infraerrors.TooManyRequests("API_KEY_QUOTA_EXHAUSTED", "api key quota exhausted")
	ErrAPIKeyQuotaExhausted = infraerrors.TooManyRequests("API_KEY_QUOTA_EXHAUSTED", "api key 额度已用完")

	// Rate limit errors
	ErrAPIKeyRateLimit5hExceeded = infraerrors.TooManyRequests("API_KEY_RATE_5H_EXCEEDED", "api key 5小时限额已用完")
	ErrAPIKeyRateLimit1dExceeded = infraerrors.TooManyRequests("API_KEY_RATE_1D_EXCEEDED", "api key 日限额已用完")
	ErrAPIKeyRateLimit7dExceeded = infraerrors.TooManyRequests("API_KEY_RATE_7D_EXCEEDED", "api key 7天限额已用完")

	// Sub-key errors
	ErrAccountKeyRequired           = infraerrors.Forbidden("ACCOUNT_KEY_REQUIRED", "this endpoint requires an account key, not a sub key")
	ErrClientKeyRequired            = infraerrors.Forbidden("CLIENT_KEY_REQUIRED", "this endpoint requires a client key, not an account key")
	ErrInsufficientAvailableBalance = infraerrors.BadRequest("INSUFFICIENT_AVAILABLE_BALANCE", "budget exceeds available balance")
	ErrInvalidBudget                = infraerrors.BadRequest("INVALID_BUDGET", "budget must be greater than 0")
	ErrAccountKeyGroupRequired      = infraerrors.BadRequest("ACCOUNT_KEY_GROUP_REQUIRED", "account key must be bound to a group before creating sub keys")
	ErrSubKeyNotFound               = infraerrors.NotFound("SUB_KEY_NOT_FOUND", "sub key not found")
	ErrBudgetLessThanSpent          = infraerrors.BadRequest("BUDGET_LESS_THAN_SPENT", "new budget cannot be less than already spent amount")
	ErrInvalidStatus                = infraerrors.BadRequest("INVALID_STATUS", "status must be 'active' or 'disabled'")
	ErrInvalidMultiplier            = infraerrors.BadRequest("INVALID_MULTIPLIER", "budgetVirtual must be greater than or equal to paidAmount")
)

const (
	apiKeyMaxErrorsPerHour = 20
	apiKeyLastUsedMinTouch = 30 * time.Second
	// DB 写失败后的短退避，避免请求路径持续同步重试造成写风暴与高延迟。
	apiKeyLastUsedFailBackoff = 5 * time.Second
)

type APIKeyRepository interface {
	Create(ctx context.Context, key *APIKey) error
	GetByID(ctx context.Context, id int64) (*APIKey, error)
	// GetKeyAndOwnerID 仅获取 API Key 的 key 与所有者 ID，用于删除等轻量场景
	GetKeyAndOwnerID(ctx context.Context, id int64) (string, int64, error)
	GetByKey(ctx context.Context, key string) (*APIKey, error)
	// GetByKeyForAuth 认证专用查询，返回最小字段集
	GetByKeyForAuth(ctx context.Context, key string) (*APIKey, error)
	Update(ctx context.Context, key *APIKey) error
	// RotateKey 原地更换 key 字符串（secret 轮换），不改变记录 ID 及其他字段。
	RotateKey(ctx context.Context, id int64, newKey string) error
	// UpdateSubKeyBudget 定向更新 sub key 的 label/quota/multiplier/status，
	// 不触碰 quota_used（由计费路径原子累加）。带 quota_used <= quota 原子守卫。
	UpdateSubKeyBudget(ctx context.Context, id int64, name string, quota, displayMultiplier float64, status string) error
	// UpdateSubKeyChannels 定向替换 sub key 的通道集合（主通道 + 白名单）。
	UpdateSubKeyChannels(ctx context.Context, id int64, groupID *int64, groupIDs []int64) error
	Delete(ctx context.Context, id int64) error
	// DeleteWithAudit 在同一事务内先写 deleted_api_key_audits 审计、再软删除该 key。
	DeleteWithAudit(ctx context.Context, id int64) error

	ListByUserID(ctx context.Context, userID int64, params pagination.PaginationParams, filters APIKeyListFilters) ([]APIKey, *pagination.PaginationResult, error)
	VerifyOwnership(ctx context.Context, userID int64, apiKeyIDs []int64) ([]int64, error)
	CountByUserID(ctx context.Context, userID int64) (int64, error)
	ExistsByKey(ctx context.Context, key string) (bool, error)
	ListByGroupID(ctx context.Context, groupID int64, params pagination.PaginationParams) ([]APIKey, *pagination.PaginationResult, error)
	SearchAPIKeys(ctx context.Context, userID int64, keyword string, limit int) ([]APIKey, error)
	ClearGroupIDByGroupID(ctx context.Context, groupID int64) (int64, error)
	// UpdateGroupIDByUserAndGroup 将用户下绑定 oldGroupID 的所有 Key 迁移到 newGroupID
	UpdateGroupIDByUserAndGroup(ctx context.Context, userID, oldGroupID, newGroupID int64) (int64, error)
	CountByGroupID(ctx context.Context, groupID int64) (int64, error)
	ListKeysByUserID(ctx context.Context, userID int64) ([]string, error)
	ListKeysByGroupID(ctx context.Context, groupID int64) ([]string, error)

	// Sub-key methods
	ListSubKeysByUserID(ctx context.Context, userID int64, page, limit int) ([]APIKey, int64, error)
	SumSubKeyRemainingQuotaByUserID(ctx context.Context, userID int64) (float64, error)
	// GetSubKeyByIDForUser 返回属于 userID 的 sub key（parent_key_id IS NOT NULL）。
	// 若不存在或不属于该用户则返回 ErrAPIKeyNotFound。
	GetSubKeyByIDForUser(ctx context.Context, userID, subKeyID int64) (*APIKey, error)

	// Quota methods
	IncrementQuotaUsed(ctx context.Context, id int64, amount float64) (float64, error)
	UpdateLastUsed(ctx context.Context, id int64, usedAt time.Time) error

	// Rate limit methods
	IncrementRateLimitUsage(ctx context.Context, id int64, cost float64) error
	ResetRateLimitWindows(ctx context.Context, id int64) error
	GetRateLimitData(ctx context.Context, id int64) (*APIKeyRateLimitData, error)
}

// APIKeyRateLimitData holds rate limit usage and window state for an API key.
type APIKeyRateLimitData struct {
	Usage5h       float64
	Usage1d       float64
	Usage7d       float64
	Window5hStart *time.Time
	Window1dStart *time.Time
	Window7dStart *time.Time
}

// EffectiveUsage5h returns the 5h window usage, or 0 if the window has expired.
func (d *APIKeyRateLimitData) EffectiveUsage5h() float64 {
	if IsWindowExpired(d.Window5hStart, RateLimitWindow5h) {
		return 0
	}
	return d.Usage5h
}

// EffectiveUsage1d returns the 1d window usage, or 0 if the window has expired.
func (d *APIKeyRateLimitData) EffectiveUsage1d() float64 {
	if IsWindowExpired(d.Window1dStart, RateLimitWindow1d) {
		return 0
	}
	return d.Usage1d
}

// EffectiveUsage7d returns the 7d window usage, or 0 if the window has expired.
func (d *APIKeyRateLimitData) EffectiveUsage7d() float64 {
	if IsWindowExpired(d.Window7dStart, RateLimitWindow7d) {
		return 0
	}
	return d.Usage7d
}

// APIKeyQuotaUsageState captures the latest quota fields after an atomic quota update.
// It is intentionally small so repositories can return it from a single SQL statement.
type APIKeyQuotaUsageState struct {
	QuotaUsed float64
	Quota     float64
	Key       string
	Status    string
}

// APIKeyCache defines cache operations for API key service
type APIKeyCache interface {
	GetCreateAttemptCount(ctx context.Context, userID int64) (int, error)
	IncrementCreateAttemptCount(ctx context.Context, userID int64) error
	DeleteCreateAttemptCount(ctx context.Context, userID int64) error

	IncrementDailyUsage(ctx context.Context, apiKey string) error
	SetDailyUsageExpiry(ctx context.Context, apiKey string, ttl time.Duration) error

	GetAuthCache(ctx context.Context, key string) (*APIKeyAuthCacheEntry, error)
	SetAuthCache(ctx context.Context, key string, entry *APIKeyAuthCacheEntry, ttl time.Duration) error
	DeleteAuthCache(ctx context.Context, key string) error

	// Pub/Sub for L1 cache invalidation across instances
	PublishAuthCacheInvalidation(ctx context.Context, cacheKey string) error
	SubscribeAuthCacheInvalidation(ctx context.Context, handler func(cacheKey string)) error
}

// APIKeyAuthCacheInvalidator 提供认证缓存失效能力
type APIKeyAuthCacheInvalidator interface {
	InvalidateAuthCacheByKey(ctx context.Context, key string)
	InvalidateAuthCacheByUserID(ctx context.Context, userID int64)
	InvalidateAuthCacheByGroupID(ctx context.Context, groupID int64)
}

// CreateAPIKeyRequest 创建API Key请求
type CreateAPIKeyRequest struct {
	Name        string   `json:"name"`
	GroupID     *int64   `json:"group_id"`
	CustomKey   *string  `json:"custom_key"`   // 可选的自定义key
	IPWhitelist []string `json:"ip_whitelist"` // IP 白名单
	IPBlacklist []string `json:"ip_blacklist"` // IP 黑名单

	// Quota fields
	Quota         float64 `json:"quota"`           // Quota limit in USD (0 = unlimited)
	ExpiresInDays *int    `json:"expires_in_days"` // Days until expiry (nil = never expires)

	// Rate limit fields (0 = unlimited)
	RateLimit5h float64 `json:"rate_limit_5h"`
	RateLimit1d float64 `json:"rate_limit_1d"`
	RateLimit7d float64 `json:"rate_limit_7d"`
}

// UpdateAPIKeyRequest 更新API Key请求
type UpdateAPIKeyRequest struct {
	Name        *string  `json:"name"`
	GroupID     *int64   `json:"group_id"`
	Status      *string  `json:"status"`
	IPWhitelist []string `json:"ip_whitelist"` // IP 白名单（空数组清空）
	IPBlacklist []string `json:"ip_blacklist"` // IP 黑名单（空数组清空）

	// Quota fields
	Quota           *float64   `json:"quota"`       // Quota limit in USD (nil = no change, 0 = unlimited)
	ExpiresAt       *time.Time `json:"expires_at"`  // Expiration time (nil = no change)
	ClearExpiration bool       `json:"-"`           // Clear expiration (internal use)
	ResetQuota      *bool      `json:"reset_quota"` // Reset quota_used to 0

	// Rate limit fields (nil = no change, 0 = unlimited)
	RateLimit5h         *float64 `json:"rate_limit_5h"`
	RateLimit1d         *float64 `json:"rate_limit_1d"`
	RateLimit7d         *float64 `json:"rate_limit_7d"`
	ResetRateLimitUsage *bool    `json:"reset_rate_limit_usage"` // Reset all usage counters to 0
}

// APIKeyService API Key服务
// RateLimitCacheInvalidator invalidates rate limit cache entries on manual reset.
type RateLimitCacheInvalidator interface {
	InvalidateAPIKeyRateLimit(ctx context.Context, keyID int64) error
}

// LockedBalanceInvalidator drops the in-process locked_balance cache for a user
// after sub key budget mutations so the billing preflight reads fresh data.
type LockedBalanceInvalidator interface {
	InvalidateLockedBalance(userID int64)
}

type APIKeyService struct {
	apiKeyRepo            APIKeyRepository
	userRepo              UserRepository
	groupRepo             GroupRepository
	userSubRepo           UserSubscriptionRepository
	userGroupRateRepo     UserGroupRateRepository
	cache                 APIKeyCache
	rateLimitCacheInvalid RateLimitCacheInvalidator // optional: invalidate Redis rate limit cache
	lockedBalanceInvalid  LockedBalanceInvalidator  // optional: invalidate locked balance cache
	cfg                   *config.Config
	authCacheL1           *ristretto.Cache
	authCfg               apiKeyAuthCacheConfig
	authGroup             singleflight.Group
	lastUsedTouchL1       sync.Map // keyID -> nextAllowedAt(time.Time)
	lastUsedTouchSF       singleflight.Group
	// subKeyBudgetMu 按 userID 串行化「读余额/锁定 → 校验 → 写入」的 sub key
	// 预算变更（创建/增额），防止并发请求都读到同一份 locked 而双双通过校验
	// 造成超额锁定。进程内锁：单实例下完全消除竞态，多实例下大幅收窄窗口。
	subKeyBudgetMu sync.Map // userID(int64) -> *sync.Mutex
}

// SetLockedBalanceInvalidator sets the optional locked balance cache invalidator.
func (s *APIKeyService) SetLockedBalanceInvalidator(inv LockedBalanceInvalidator) {
	s.lockedBalanceInvalid = inv
}

// invalidateLockedBalance drops the billing-side locked_balance cache (no-op
// when the invalidator is not wired).
func (s *APIKeyService) invalidateLockedBalance(userID int64) {
	if s.lockedBalanceInvalid != nil {
		s.lockedBalanceInvalid.InvalidateLockedBalance(userID)
	}
}

// lockSubKeyBudget 获取指定用户的 sub key 预算变更锁，返回解锁函数。
func (s *APIKeyService) lockSubKeyBudget(userID int64) func() {
	v, _ := s.subKeyBudgetMu.LoadOrStore(userID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// NewAPIKeyService 创建API Key服务实例
func NewAPIKeyService(
	apiKeyRepo APIKeyRepository,
	userRepo UserRepository,
	groupRepo GroupRepository,
	userSubRepo UserSubscriptionRepository,
	userGroupRateRepo UserGroupRateRepository,
	cache APIKeyCache,
	cfg *config.Config,
) *APIKeyService {
	svc := &APIKeyService{
		apiKeyRepo:        apiKeyRepo,
		userRepo:          userRepo,
		groupRepo:         groupRepo,
		userSubRepo:       userSubRepo,
		userGroupRateRepo: userGroupRateRepo,
		cache:             cache,
		cfg:               cfg,
	}
	svc.initAuthCache(cfg)
	return svc
}

// SetRateLimitCacheInvalidator sets the optional rate limit cache invalidator.
// Called after construction (e.g. in wire) to avoid circular dependencies.
func (s *APIKeyService) SetRateLimitCacheInvalidator(inv RateLimitCacheInvalidator) {
	s.rateLimitCacheInvalid = inv
}

func (s *APIKeyService) compileAPIKeyIPRules(apiKey *APIKey) {
	if apiKey == nil {
		return
	}
	apiKey.CompiledIPWhitelist = ip.CompileIPRules(apiKey.IPWhitelist)
	apiKey.CompiledIPBlacklist = ip.CompileIPRules(apiKey.IPBlacklist)
}

// GenerateKey 生成随机API Key
func (s *APIKeyService) GenerateKey() (string, error) {
	// 生成32字节随机数据
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate random bytes: %w", err)
	}

	// 转换为十六进制字符串并添加前缀
	prefix := s.cfg.Default.APIKeyPrefix
	if prefix == "" {
		prefix = "sk-"
	}

	key := prefix + hex.EncodeToString(bytes)
	return key, nil
}

// EnsureDefaultAccountKey 为新注册用户创建默认密钥（幂等）。
// 如果该用户已有密钥，直接返回 (nil, false, nil)，不重复创建。
// 创建失败不影响注册流程，调用方应忽略错误并继续响应。
func (s *APIKeyService) EnsureDefaultAccountKey(ctx context.Context, userID int64) (*APIKey, bool, error) {
	count, err := s.apiKeyRepo.CountByUserID(ctx, userID)
	if err != nil {
		return nil, false, fmt.Errorf("count api keys: %w", err)
	}
	if count > 0 {
		return nil, false, nil
	}

	apiKey, err := s.Create(ctx, userID, CreateAPIKeyRequest{
		Name: "Default",
	})
	if err != nil {
		return nil, false, fmt.Errorf("create default api key: %w", err)
	}
	return apiKey, true, nil
}

// ValidateCustomKey 验证自定义API Key格式
func (s *APIKeyService) ValidateCustomKey(key string) error {
	// 检查长度
	if len(key) < 16 {
		return ErrAPIKeyTooShort
	}

	// 检查字符：只允许字母、数字、下划线、连字符
	for _, c := range key {
		if (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '_' || c == '-' {
			continue
		}
		return ErrAPIKeyInvalidChars
	}

	return nil
}

// checkAPIKeyRateLimit 检查用户创建自定义Key的错误次数是否超限
func (s *APIKeyService) checkAPIKeyRateLimit(ctx context.Context, userID int64) error {
	if s.cache == nil {
		return nil
	}

	count, err := s.cache.GetCreateAttemptCount(ctx, userID)
	if err != nil {
		// Redis 出错时不阻止用户操作
		return nil
	}

	if count >= apiKeyMaxErrorsPerHour {
		return ErrAPIKeyRateLimited
	}

	return nil
}

// incrementAPIKeyErrorCount 增加用户创建自定义Key的错误计数
func (s *APIKeyService) incrementAPIKeyErrorCount(ctx context.Context, userID int64) {
	if s.cache == nil {
		return
	}

	_ = s.cache.IncrementCreateAttemptCount(ctx, userID)
}

// canUserBindGroup 检查用户是否可以绑定指定分组
// 对于订阅类型分组：检查用户是否有有效订阅
// 对于标准类型分组：使用原有的 AllowedGroups 和 IsExclusive 逻辑
func (s *APIKeyService) canUserBindGroup(ctx context.Context, user *User, group *Group) bool {
	// 订阅类型分组：需要有效订阅
	if group.IsSubscriptionType() {
		_, err := s.userSubRepo.GetActiveByUserIDAndGroupID(ctx, user.ID, group.ID)
		return err == nil // 有有效订阅则允许
	}
	// 标准类型分组：使用原有逻辑
	return user.CanBindGroup(group.ID, group.IsExclusive)
}

// Create 创建API Key
func (s *APIKeyService) Create(ctx context.Context, userID int64, req CreateAPIKeyRequest) (*APIKey, error) {
	// 验证用户存在
	user, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}

	// 验证 IP 白名单格式
	if len(req.IPWhitelist) > 0 {
		if invalid := ip.ValidateIPPatterns(req.IPWhitelist); len(invalid) > 0 {
			return nil, fmt.Errorf("%w: %v", ErrInvalidIPPattern, invalid)
		}
	}

	// 验证 IP 黑名单格式
	if len(req.IPBlacklist) > 0 {
		if invalid := ip.ValidateIPPatterns(req.IPBlacklist); len(invalid) > 0 {
			return nil, fmt.Errorf("%w: %v", ErrInvalidIPPattern, invalid)
		}
	}

	// 验证分组权限（如果指定了分组）
	if req.GroupID != nil {
		group, err := s.groupRepo.GetByID(ctx, *req.GroupID)
		if err != nil {
			return nil, fmt.Errorf("get group: %w", err)
		}

		// 检查用户是否可以绑定该分组
		if !s.canUserBindGroup(ctx, user, group) {
			return nil, ErrGroupNotAllowed
		}
	}

	var key string

	// 判断是否使用自定义Key
	if req.CustomKey != nil && *req.CustomKey != "" {
		// 检查限流（仅对自定义key进行限流）
		if err := s.checkAPIKeyRateLimit(ctx, userID); err != nil {
			return nil, err
		}

		// 验证自定义Key格式
		if err := s.ValidateCustomKey(*req.CustomKey); err != nil {
			return nil, err
		}

		// 检查Key是否已存在
		exists, err := s.apiKeyRepo.ExistsByKey(ctx, *req.CustomKey)
		if err != nil {
			return nil, fmt.Errorf("check key exists: %w", err)
		}
		if exists {
			// Key已存在，增加错误计数
			s.incrementAPIKeyErrorCount(ctx, userID)
			return nil, ErrAPIKeyExists
		}

		key = *req.CustomKey
	} else {
		// 生成随机API Key
		var err error
		key, err = s.GenerateKey()
		if err != nil {
			return nil, fmt.Errorf("generate key: %w", err)
		}
	}

	// 创建API Key记录
	apiKey := &APIKey{
		UserID:      userID,
		Key:         key,
		Name:        html.EscapeString(req.Name),
		GroupID:     req.GroupID,
		Status:      StatusActive,
		IPWhitelist: req.IPWhitelist,
		IPBlacklist: req.IPBlacklist,
		Quota:       req.Quota,
		QuotaUsed:   0,
		RateLimit5h: req.RateLimit5h,
		RateLimit1d: req.RateLimit1d,
		RateLimit7d: req.RateLimit7d,
	}

	// Set expiration time if specified
	if req.ExpiresInDays != nil && *req.ExpiresInDays > 0 {
		expiresAt := time.Now().AddDate(0, 0, *req.ExpiresInDays)
		apiKey.ExpiresAt = &expiresAt
	}

	if err := s.apiKeyRepo.Create(ctx, apiKey); err != nil {
		return nil, fmt.Errorf("create api key: %w", err)
	}

	s.InvalidateAuthCacheByKey(ctx, apiKey.Key)
	s.compileAPIKeyIPRules(apiKey)

	return apiKey, nil
}

// List 获取用户的API Key列表
func (s *APIKeyService) List(ctx context.Context, userID int64, params pagination.PaginationParams, filters APIKeyListFilters) ([]APIKey, *pagination.PaginationResult, error) {
	keys, pagination, err := s.apiKeyRepo.ListByUserID(ctx, userID, params, filters)
	if err != nil {
		return nil, nil, fmt.Errorf("list api keys: %w", err)
	}
	return keys, pagination, nil
}

func (s *APIKeyService) VerifyOwnership(ctx context.Context, userID int64, apiKeyIDs []int64) ([]int64, error) {
	if len(apiKeyIDs) == 0 {
		return []int64{}, nil
	}

	validIDs, err := s.apiKeyRepo.VerifyOwnership(ctx, userID, apiKeyIDs)
	if err != nil {
		return nil, fmt.Errorf("verify api key ownership: %w", err)
	}
	return validIDs, nil
}

// GetByID 根据ID获取API Key
func (s *APIKeyService) GetByID(ctx context.Context, id int64) (*APIKey, error) {
	apiKey, err := s.apiKeyRepo.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get api key: %w", err)
	}
	s.compileAPIKeyIPRules(apiKey)
	return apiKey, nil
}

// GetByKey 根据Key字符串获取API Key（用于认证）
func (s *APIKeyService) GetByKey(ctx context.Context, key string) (*APIKey, error) {
	cacheKey := s.authCacheKey(key)

	if entry, ok := s.getAuthCacheEntry(ctx, cacheKey); ok {
		if apiKey, used, err := s.applyAuthCacheEntry(key, entry); used {
			if err != nil {
				return nil, fmt.Errorf("get api key: %w", err)
			}
			s.compileAPIKeyIPRules(apiKey)
			return apiKey, nil
		}
	}

	if s.authCfg.singleflight {
		value, err, _ := s.authGroup.Do(cacheKey, func() (any, error) {
			return s.loadAuthCacheEntry(ctx, key, cacheKey)
		})
		if err != nil {
			return nil, err
		}
		entry, _ := value.(*APIKeyAuthCacheEntry)
		if apiKey, used, err := s.applyAuthCacheEntry(key, entry); used {
			if err != nil {
				return nil, fmt.Errorf("get api key: %w", err)
			}
			s.compileAPIKeyIPRules(apiKey)
			return apiKey, nil
		}
	} else {
		entry, err := s.loadAuthCacheEntry(ctx, key, cacheKey)
		if err != nil {
			return nil, err
		}
		if apiKey, used, err := s.applyAuthCacheEntry(key, entry); used {
			if err != nil {
				return nil, fmt.Errorf("get api key: %w", err)
			}
			s.compileAPIKeyIPRules(apiKey)
			return apiKey, nil
		}
	}

	apiKey, err := s.apiKeyRepo.GetByKeyForAuth(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("get api key: %w", err)
	}
	apiKey.Key = key
	s.compileAPIKeyIPRules(apiKey)
	return apiKey, nil
}

// Update 更新API Key
func (s *APIKeyService) Update(ctx context.Context, id int64, userID int64, req UpdateAPIKeyRequest) (*APIKey, error) {
	apiKey, err := s.apiKeyRepo.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get api key: %w", err)
	}

	// 验证所有权
	if apiKey.UserID != userID {
		return nil, ErrInsufficientPerms
	}

	// 验证 IP 白名单格式
	if len(req.IPWhitelist) > 0 {
		if invalid := ip.ValidateIPPatterns(req.IPWhitelist); len(invalid) > 0 {
			return nil, fmt.Errorf("%w: %v", ErrInvalidIPPattern, invalid)
		}
	}

	// 验证 IP 黑名单格式
	if len(req.IPBlacklist) > 0 {
		if invalid := ip.ValidateIPPatterns(req.IPBlacklist); len(invalid) > 0 {
			return nil, fmt.Errorf("%w: %v", ErrInvalidIPPattern, invalid)
		}
	}

	// 更新字段
	if req.Name != nil {
		apiKey.Name = html.EscapeString(*req.Name)
	}

	if req.GroupID != nil {
		// 验证分组权限
		user, err := s.userRepo.GetByID(ctx, userID)
		if err != nil {
			return nil, fmt.Errorf("get user: %w", err)
		}

		group, err := s.groupRepo.GetByID(ctx, *req.GroupID)
		if err != nil {
			return nil, fmt.Errorf("get group: %w", err)
		}

		if !s.canUserBindGroup(ctx, user, group) {
			return nil, ErrGroupNotAllowed
		}

		apiKey.GroupID = req.GroupID
	}

	if req.Status != nil {
		apiKey.Status = *req.Status
		// 如果状态改变，清除Redis缓存
		if s.cache != nil {
			_ = s.cache.DeleteCreateAttemptCount(ctx, apiKey.UserID)
		}
	}

	// Update quota fields
	if req.Quota != nil {
		apiKey.Quota = *req.Quota
		// If quota is increased and status was quota_exhausted, reactivate
		if apiKey.Status == StatusAPIKeyQuotaExhausted && *req.Quota > apiKey.QuotaUsed {
			apiKey.Status = StatusActive
		}
	}
	if req.ResetQuota != nil && *req.ResetQuota {
		apiKey.QuotaUsed = 0
		// If resetting quota and status was quota_exhausted, reactivate
		if apiKey.Status == StatusAPIKeyQuotaExhausted {
			apiKey.Status = StatusActive
		}
	}
	if req.ClearExpiration {
		apiKey.ExpiresAt = nil
		// If clearing expiry and status was expired, reactivate
		if apiKey.Status == StatusAPIKeyExpired {
			apiKey.Status = StatusActive
		}
	} else if req.ExpiresAt != nil {
		apiKey.ExpiresAt = req.ExpiresAt
		// If extending expiry and status was expired, reactivate
		if apiKey.Status == StatusAPIKeyExpired && time.Now().Before(*req.ExpiresAt) {
			apiKey.Status = StatusActive
		}
	}

	// 更新 IP 限制（空数组会清空设置）
	apiKey.IPWhitelist = req.IPWhitelist
	apiKey.IPBlacklist = req.IPBlacklist

	// Update rate limit configuration
	if req.RateLimit5h != nil {
		apiKey.RateLimit5h = *req.RateLimit5h
	}
	if req.RateLimit1d != nil {
		apiKey.RateLimit1d = *req.RateLimit1d
	}
	if req.RateLimit7d != nil {
		apiKey.RateLimit7d = *req.RateLimit7d
	}
	resetRateLimit := req.ResetRateLimitUsage != nil && *req.ResetRateLimitUsage
	if resetRateLimit {
		apiKey.Usage5h = 0
		apiKey.Usage1d = 0
		apiKey.Usage7d = 0
		apiKey.Window5hStart = nil
		apiKey.Window1dStart = nil
		apiKey.Window7dStart = nil
	}

	if err := s.apiKeyRepo.Update(ctx, apiKey); err != nil {
		return nil, fmt.Errorf("update api key: %w", err)
	}

	s.InvalidateAuthCacheByKey(ctx, apiKey.Key)
	s.compileAPIKeyIPRules(apiKey)

	// Invalidate Redis rate limit cache so reset takes effect immediately
	if resetRateLimit && s.rateLimitCacheInvalid != nil {
		_ = s.rateLimitCacheInvalid.InvalidateAPIKeyRateLimit(ctx, apiKey.ID)
	}

	return apiKey, nil
}

// Delete 删除API Key
func (s *APIKeyService) Delete(ctx context.Context, id int64, userID int64) error {
	key, ownerID, err := s.apiKeyRepo.GetKeyAndOwnerID(ctx, id)
	if err != nil {
		return fmt.Errorf("get api key: %w", err)
	}

	// 验证当前用户是否为该 API Key 的所有者
	if ownerID != userID {
		return ErrInsufficientPerms
	}

	// 事务内:写审计 + 软删除(tombstone)。
	if err := s.apiKeyRepo.DeleteWithAudit(ctx, id); err != nil {
		return fmt.Errorf("delete api key: %w", err)
	}

	// 删除成功后再清理缓存,避免"缓存已清但删除失败"的竞态。
	if s.cache != nil {
		_ = s.cache.DeleteCreateAttemptCount(ctx, userID)
	}
	s.InvalidateAuthCacheByKey(ctx, key)
	s.lastUsedTouchL1.Delete(id)

	return nil
}

// ValidateKey 验证API Key是否有效（用于认证中间件）
func (s *APIKeyService) ValidateKey(ctx context.Context, key string) (*APIKey, *User, error) {
	// 获取API Key
	apiKey, err := s.GetByKey(ctx, key)
	if err != nil {
		return nil, nil, err
	}

	// 检查API Key状态
	if !apiKey.IsActive() {
		return nil, nil, infraerrors.Unauthorized("API_KEY_INACTIVE", "api key is not active")
	}

	// 获取用户信息
	user, err := s.userRepo.GetByID(ctx, apiKey.UserID)
	if err != nil {
		return nil, nil, fmt.Errorf("get user: %w", err)
	}

	// 检查用户状态
	if !user.IsActive() {
		return nil, nil, ErrUserNotActive
	}

	return apiKey, user, nil
}

// TouchLastUsed 通过防抖更新 api_keys.last_used_at，减少高频写放大。
// 该操作为尽力而为，不应阻塞主请求链路。
func (s *APIKeyService) TouchLastUsed(ctx context.Context, keyID int64) error {
	if keyID <= 0 {
		return nil
	}

	now := time.Now()
	if v, ok := s.lastUsedTouchL1.Load(keyID); ok {
		if nextAllowedAt, ok := v.(time.Time); ok && now.Before(nextAllowedAt) {
			return nil
		}
	}

	_, err, _ := s.lastUsedTouchSF.Do(strconv.FormatInt(keyID, 10), func() (any, error) {
		latest := time.Now()
		if v, ok := s.lastUsedTouchL1.Load(keyID); ok {
			if nextAllowedAt, ok := v.(time.Time); ok && latest.Before(nextAllowedAt) {
				return nil, nil
			}
		}

		if err := s.apiKeyRepo.UpdateLastUsed(ctx, keyID, latest); err != nil {
			s.lastUsedTouchL1.Store(keyID, latest.Add(apiKeyLastUsedFailBackoff))
			return nil, fmt.Errorf("touch api key last used: %w", err)
		}
		s.lastUsedTouchL1.Store(keyID, latest.Add(apiKeyLastUsedMinTouch))
		return nil, nil
	})
	return err
}

// IncrementUsage 增加API Key使用次数（可选：用于统计）
func (s *APIKeyService) IncrementUsage(ctx context.Context, keyID int64) error {
	// 使用Redis计数器
	if s.cache != nil {
		cacheKey := fmt.Sprintf("apikey:usage:%d:%s", keyID, timezone.Now().Format("2006-01-02"))
		if err := s.cache.IncrementDailyUsage(ctx, cacheKey); err != nil {
			return fmt.Errorf("increment usage: %w", err)
		}
		// 设置24小时过期
		_ = s.cache.SetDailyUsageExpiry(ctx, cacheKey, 24*time.Hour)
	}
	return nil
}

// GetAvailableGroups 获取用户有权限绑定的分组列表
// 返回用户可以选择的分组：
// - 标准类型分组：公开的（非专属）或用户被明确允许的
// - 订阅类型分组：用户有有效订阅的
func (s *APIKeyService) GetAvailableGroups(ctx context.Context, userID int64) ([]Group, error) {
	// 获取用户信息
	user, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}

	// 获取所有活跃分组
	allGroups, err := s.groupRepo.ListActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("list active groups: %w", err)
	}

	// 获取用户的所有有效订阅
	activeSubscriptions, err := s.userSubRepo.ListActiveByUserID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list active subscriptions: %w", err)
	}

	// 构建订阅分组 ID 集合
	subscribedGroupIDs := make(map[int64]bool)
	for _, sub := range activeSubscriptions {
		subscribedGroupIDs[sub.GroupID] = true
	}

	// 过滤出用户有权限的分组
	availableGroups := make([]Group, 0)
	for _, group := range allGroups {
		if s.canUserBindGroupInternal(user, &group, subscribedGroupIDs) {
			availableGroups = append(availableGroups, group)
		}
	}

	return availableGroups, nil
}

// canUserBindGroupInternal 内部方法，检查用户是否可以绑定分组（使用预加载的订阅数据）
func (s *APIKeyService) canUserBindGroupInternal(user *User, group *Group, subscribedGroupIDs map[int64]bool) bool {
	// 订阅类型分组：需要有效订阅
	if group.IsSubscriptionType() {
		return subscribedGroupIDs[group.ID]
	}
	// 标准类型分组：使用原有逻辑
	return user.CanBindGroup(group.ID, group.IsExclusive)
}

func (s *APIKeyService) SearchAPIKeys(ctx context.Context, userID int64, keyword string, limit int) ([]APIKey, error) {
	keys, err := s.apiKeyRepo.SearchAPIKeys(ctx, userID, keyword, limit)
	if err != nil {
		return nil, fmt.Errorf("search api keys: %w", err)
	}
	return keys, nil
}

// GetUserGroupRates 获取用户的专属分组倍率配置
// 返回 map[groupID]rateMultiplier
func (s *APIKeyService) GetUserGroupRates(ctx context.Context, userID int64) (map[int64]float64, error) {
	if s.userGroupRateRepo == nil {
		return nil, nil
	}
	rates, err := s.userGroupRateRepo.GetByUserID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get user group rates: %w", err)
	}
	return rates, nil
}

// CheckAPIKeyQuotaAndExpiry checks if the API key is valid for use (not expired, quota not exhausted)
// Returns nil if valid, error if invalid
func (s *APIKeyService) CheckAPIKeyQuotaAndExpiry(apiKey *APIKey) error {
	// Check expiration
	if apiKey.IsExpired() {
		return ErrAPIKeyExpired
	}

	// Check quota
	if apiKey.IsQuotaExhausted() {
		return ErrAPIKeyQuotaExhausted
	}

	return nil
}

// UpdateQuotaUsed updates the quota_used field after a request
// Also checks if quota is exhausted and updates status accordingly
func (s *APIKeyService) UpdateQuotaUsed(ctx context.Context, apiKeyID int64, cost float64) error {
	if cost <= 0 {
		return nil
	}

	type quotaStateReader interface {
		IncrementQuotaUsedAndGetState(ctx context.Context, id int64, amount float64) (*APIKeyQuotaUsageState, error)
	}

	if repo, ok := s.apiKeyRepo.(quotaStateReader); ok {
		state, err := repo.IncrementQuotaUsedAndGetState(ctx, apiKeyID, cost)
		if err != nil {
			return fmt.Errorf("increment quota used: %w", err)
		}
		if state != nil && state.Status == StatusAPIKeyQuotaExhausted && strings.TrimSpace(state.Key) != "" {
			s.InvalidateAuthCacheByKey(ctx, state.Key)
		}
		return nil
	}

	// Use repository to atomically increment quota_used
	newQuotaUsed, err := s.apiKeyRepo.IncrementQuotaUsed(ctx, apiKeyID, cost)
	if err != nil {
		return fmt.Errorf("increment quota used: %w", err)
	}

	// Check if quota is now exhausted and update status if needed
	apiKey, err := s.apiKeyRepo.GetByID(ctx, apiKeyID)
	if err != nil {
		return nil // Don't fail the request, just log
	}

	// If quota is set and now exhausted, update status
	if apiKey.Quota > 0 && newQuotaUsed >= apiKey.Quota {
		apiKey.Status = StatusAPIKeyQuotaExhausted
		if err := s.apiKeyRepo.Update(ctx, apiKey); err != nil {
			return nil // Don't fail the request
		}
		// Invalidate cache so next request sees the new status
		s.InvalidateAuthCacheByKey(ctx, apiKey.Key)
	}

	return nil
}

// GetRateLimitData returns rate limit usage and window state for an API key.
func (s *APIKeyService) GetRateLimitData(ctx context.Context, id int64) (*APIKeyRateLimitData, error) {
	return s.apiKeyRepo.GetRateLimitData(ctx, id)
}

// UpdateRateLimitUsage atomically increments rate limit usage counters in the DB.
func (s *APIKeyService) UpdateRateLimitUsage(ctx context.Context, apiKeyID int64, cost float64) error {
	if cost <= 0 {
		return nil
	}
	return s.apiKeyRepo.IncrementRateLimitUsage(ctx, apiKeyID, cost)
}

// GetLockedBalance returns the sum of remaining quota across all active sub keys for a user.
func (s *APIKeyService) GetLockedBalance(ctx context.Context, userID int64) (float64, error) {
	return s.apiKeyRepo.SumSubKeyRemainingQuotaByUserID(ctx, userID)
}

// CreateSubKeyOptions 客户密钥创建的通道配置（均可选）。
type CreateSubKeyOptions struct {
	// GroupID 客户密钥的主通道；nil = 继承账号密钥的绑定分组。
	GroupID *int64
	// AllowedGroupIDs 额外允许通过 URI 前缀选择的通道白名单（不含主通道）。
	AllowedGroupIDs []int64
}

// maxSubKeyAllowedGroups 白名单上限，防御性限制。
const maxSubKeyAllowedGroups = 32

// resolveSubKeyGroups 校验并规范化客户密钥的主通道与白名单。
// 每个分组都要求：存在、启用、且账号主有权绑定（专属组/订阅组按既有规则）。
func (s *APIKeyService) resolveSubKeyGroups(ctx context.Context, user *User, defaultGroupID *int64, opts CreateSubKeyOptions) (*int64, []int64, error) {
	mainGroupID := defaultGroupID
	if opts.GroupID != nil {
		mainGroupID = opts.GroupID
	}
	validate := func(groupID int64) error {
		group, err := s.groupRepo.GetByID(ctx, groupID)
		if err != nil {
			return fmt.Errorf("get group %d: %w", groupID, err)
		}
		if !group.IsActive() {
			return ErrGroupNotAllowed
		}
		if !s.canUserBindGroup(ctx, user, group) {
			return ErrGroupNotAllowed
		}
		return nil
	}
	if opts.GroupID != nil {
		if err := validate(*opts.GroupID); err != nil {
			return nil, nil, err
		}
	}
	if len(opts.AllowedGroupIDs) > maxSubKeyAllowedGroups {
		return nil, nil, ErrGroupNotAllowed
	}
	seen := make(map[int64]struct{}, len(opts.AllowedGroupIDs))
	allowed := make([]int64, 0, len(opts.AllowedGroupIDs))
	for _, id := range opts.AllowedGroupIDs {
		if id <= 0 {
			return nil, nil, ErrGroupNotAllowed
		}
		if mainGroupID != nil && id == *mainGroupID {
			continue // 主通道隐式允许，白名单去重
		}
		if _, dup := seen[id]; dup {
			continue
		}
		if err := validate(id); err != nil {
			return nil, nil, err
		}
		seen[id] = struct{}{}
		allowed = append(allowed, id)
	}
	return mainGroupID, allowed, nil
}

// CreateSubKey creates a sub key under an account key.
// budgetVirtual is the customer-facing budget; paidAmount is the real amount
// locked from the account balance (quota). displayMultiplier = budgetVirtual/paidAmount.
func (s *APIKeyService) CreateSubKey(ctx context.Context, accountKey *APIKey, label string, budgetVirtual, paidAmount float64, opts CreateSubKeyOptions) (*APIKey, error) {
	if accountKey.ParentKeyID != nil {
		return nil, ErrAccountKeyRequired
	}
	if accountKey.GroupID == nil {
		// Default account keys are created at registration without a group.
		// Fall back to the user's first available group so sub key creation
		// works out of the box; only fail when the user truly has none.
		groups, err := s.GetAvailableGroups(ctx, accountKey.UserID)
		if err != nil {
			return nil, fmt.Errorf("resolve default group: %w", err)
		}
		if len(groups) == 0 {
			return nil, ErrAccountKeyGroupRequired
		}
		groupID := groups[0].ID
		accountKey.GroupID = &groupID
	}
	if budgetVirtual <= 0 || paidAmount <= 0 {
		return nil, ErrInvalidBudget
	}
	if budgetVirtual < paidAmount {
		return nil, ErrInvalidMultiplier
	}
	displayMultiplier := budgetVirtual / paidAmount

	// Serialize balance check + insert per user (see lockSubKeyBudget).
	unlock := s.lockSubKeyBudget(accountKey.UserID)
	defer unlock()

	user, err := s.userRepo.GetByID(ctx, accountKey.UserID)
	if err != nil {
		return nil, err
	}

	// 通道解析：主通道（默认继承账号密钥分组）+ 额外白名单，全部校验权限
	mainGroupID, allowedGroupIDs, err := s.resolveSubKeyGroups(ctx, user, accountKey.GroupID, opts)
	if err != nil {
		return nil, err
	}

	locked, err := s.GetLockedBalance(ctx, accountKey.UserID)
	if err != nil {
		return nil, err
	}

	available := user.Balance - locked
	if paidAmount > available {
		return nil, ErrInsufficientAvailableBalance
	}

	key, err := s.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}

	subKey := &APIKey{
		UserID:            accountKey.UserID,
		Key:               key,
		Name:              html.EscapeString(label),
		GroupID:           mainGroupID,
		ParentKeyID:       &accountKey.ID,
		Status:            StatusAPIKeyActive,
		Quota:             paidAmount,
		DisplayMultiplier: displayMultiplier,
		AllowedGroupIDs:   allowedGroupIDs,
	}
	if err := s.apiKeyRepo.Create(ctx, subKey); err != nil {
		return nil, fmt.Errorf("create sub key: %w", err)
	}

	s.InvalidateAuthCacheByKey(ctx, subKey.Key)
	s.invalidateLockedBalance(accountKey.UserID)
	return subKey, nil
}

// ListSubKeys returns sub keys belonging to the given account key's user.
func (s *APIKeyService) ListSubKeys(ctx context.Context, accountKey *APIKey, page, limit int) ([]APIKey, int64, error) {
	if accountKey.ParentKeyID != nil {
		return nil, 0, ErrAccountKeyRequired
	}
	return s.apiKeyRepo.ListSubKeysByUserID(ctx, accountKey.UserID, page, limit)
}

// UpdateSubKeyRequest holds the optional fields that can be changed on a sub key.
type UpdateSubKeyRequest struct {
	Label         *string  // new name
	BudgetVirtual *float64 // new customer-facing budget
	PaidAmount    *float64 // new real locked amount (quota)
	Status        *string  // "active" or "disabled"
	// GroupID 主通道变更（nil = 不变）
	GroupID *int64
	// AllowedGroupIDs 通道白名单整体替换（nil = 不变；空切片 = 清空，锁回主通道）
	AllowedGroupIDs *[]int64
}

// EffectiveDisplayMultiplier returns the key's display multiplier, treating
// missing/zero values as 1 for legacy compatibility.
func EffectiveDisplayMultiplier(k *APIKey) float64 {
	if k == nil || k.DisplayMultiplier <= 0 {
		return 1
	}
	return k.DisplayMultiplier
}

// subKeyContributesToLocked returns true when a sub key's remaining quota counts
// towards locked_balance (mirrors SumSubKeyRemainingQuotaByUserID filter).
func subKeyContributesToLocked(k *APIKey) bool {
	if k.Quota <= 0 {
		return false
	}
	return k.Status == StatusAPIKeyActive || k.Status == StatusAPIKeyQuotaExhausted
}

// subKeyRemaining returns max(quota - quota_used, 0) for a sub key.
func subKeyRemaining(k *APIKey) float64 {
	r := k.Quota - k.QuotaUsed
	if r < 0 {
		return 0
	}
	return r
}

// UpdateSubKey modifies label, budget, and/or status of a sub key owned by accountKey.
func (s *APIKeyService) UpdateSubKey(ctx context.Context, accountKey *APIKey, subKeyID int64, req UpdateSubKeyRequest) (*APIKey, error) {
	if accountKey.ParentKeyID != nil {
		return nil, ErrAccountKeyRequired
	}

	// Serialize balance check + write per user (see lockSubKeyBudget). Taken
	// before the read so the snapshot used for validation stays coherent with
	// the write against concurrent Create/Update on the same account.
	unlock := s.lockSubKeyBudget(accountKey.UserID)
	defer unlock()

	subKey, err := s.apiKeyRepo.GetSubKeyByIDForUser(ctx, accountKey.UserID, subKeyID)
	if err != nil {
		if errors.Is(err, ErrAPIKeyNotFound) {
			return nil, ErrSubKeyNotFound
		}
		return nil, err
	}

	// snapshot of current contribution to locked_balance
	oldContributes := subKeyContributesToLocked(subKey)
	oldRemaining := subKeyRemaining(subKey)

	if req.Label != nil {
		subKey.Name = html.EscapeString(*req.Label)
	}

	newStatus := subKey.Status
	if req.Status != nil {
		switch *req.Status {
		case StatusAPIKeyActive, StatusAPIKeyDisabled:
			newStatus = *req.Status
		default:
			return nil, ErrInvalidStatus
		}
	}

	// Resolve new quota (paidAmount) and display multiplier.
	//  - both budgetVirtual & paidAmount → quota = paidAmount, multiplier = bv/pa
	//  - only budgetVirtual              → quota unchanged, multiplier = bv/quota
	//  - only paidAmount                 → multiplier unchanged, quota = paidAmount
	newQuota := subKey.Quota
	newMultiplier := EffectiveDisplayMultiplier(subKey)
	switch {
	case req.BudgetVirtual != nil && req.PaidAmount != nil:
		bv, pa := *req.BudgetVirtual, *req.PaidAmount
		if bv <= 0 || pa <= 0 {
			return nil, ErrInvalidBudget
		}
		if bv < pa {
			return nil, ErrInvalidMultiplier
		}
		newQuota = pa
		newMultiplier = bv / pa
	case req.BudgetVirtual != nil:
		bv := *req.BudgetVirtual
		if bv <= 0 {
			return nil, ErrInvalidBudget
		}
		if bv < subKey.Quota {
			return nil, ErrInvalidMultiplier
		}
		if subKey.Quota > 0 {
			newMultiplier = bv / subKey.Quota
		}
	case req.PaidAmount != nil:
		pa := *req.PaidAmount
		if pa <= 0 {
			return nil, ErrInvalidBudget
		}
		newQuota = pa
	}
	if newQuota < subKey.QuotaUsed {
		return nil, ErrBudgetLessThanSpent
	}

	// Top-up revives an exhausted key: once the budget rises above the spent
	// amount the key must be usable again. An explicit req.Status still wins.
	if req.Status == nil && newStatus == StatusAPIKeyQuotaExhausted && newQuota > subKey.QuotaUsed {
		newStatus = StatusAPIKeyActive
	}

	// Compute effect on locked_balance
	subKey.Status = newStatus
	subKey.Quota = newQuota
	subKey.DisplayMultiplier = newMultiplier
	newContributes := subKeyContributesToLocked(subKey)
	newRemaining := subKeyRemaining(subKey)

	// Only need balance check when the sub key will contribute more than before
	if newContributes && (!oldContributes || newRemaining > oldRemaining) {
		currentLocked, err := s.GetLockedBalance(ctx, accountKey.UserID)
		if err != nil {
			return nil, err
		}
		// Adjust for this key's old contribution (GetLockedBalance already includes it)
		var oldContrib float64
		if oldContributes {
			oldContrib = oldRemaining
		}
		projectedLocked := currentLocked - oldContrib + newRemaining

		user, err := s.userRepo.GetByID(ctx, accountKey.UserID)
		if err != nil {
			return nil, err
		}
		if projectedLocked > user.Balance {
			return nil, ErrInsufficientAvailableBalance
		}
	}

	// 通道集合变更（主通道 + 白名单，任一提供即整体重解析）。校验必须先于
	// 任何写入：预算写入后再因通道无效而报错，会留下"提示失败但预算已变"
	// 的部分更新。白名单未提供时沿用现有值重新校验：换主通道后隐式允许项
	// 会被去重，且所有通道都按当前权限重新验证。
	channelsResolved := false
	var newMainGroupID *int64
	var newAllowed []int64
	if req.GroupID != nil || req.AllowedGroupIDs != nil {
		user, err := s.userRepo.GetByID(ctx, accountKey.UserID)
		if err != nil {
			return nil, err
		}
		allowedIn := subKey.AllowedGroupIDs
		if req.AllowedGroupIDs != nil {
			allowedIn = *req.AllowedGroupIDs
		}
		newMainGroupID, newAllowed, err = s.resolveSubKeyGroups(ctx, user, subKey.GroupID, CreateSubKeyOptions{
			GroupID:         req.GroupID,
			AllowedGroupIDs: allowedIn,
		})
		if err != nil {
			return nil, err
		}
		channelsResolved = true
	}

	// 任何一步写入发生后，无论后续成败都必须让 auth 快照与锁定余额缓存失效，
	// 否则部分写入会被旧缓存掩盖（配额检查用旧 quota、可用余额显示旧值）。
	wrote := false
	defer func() {
		if wrote {
			s.InvalidateAuthCacheByKey(ctx, subKey.Key)
			s.invalidateLockedBalance(accountKey.UserID)
		}
	}()

	// Targeted write: quota_used and usage counters are owned by the billing
	// path — a full-row Update would overwrite concurrent consumption with the
	// stale snapshot read above. The repo re-checks quota_used <= quota
	// atomically inside the UPDATE.
	if err := s.apiKeyRepo.UpdateSubKeyBudget(ctx, subKey.ID, subKey.Name, newQuota, newMultiplier, newStatus); err != nil {
		if errors.Is(err, ErrAPIKeyNotFound) {
			return nil, ErrSubKeyNotFound
		}
		return nil, err
	}
	wrote = true

	if channelsResolved {
		if err := s.apiKeyRepo.UpdateSubKeyChannels(ctx, subKey.ID, newMainGroupID, newAllowed); err != nil {
			if errors.Is(err, ErrAPIKeyNotFound) {
				return nil, ErrSubKeyNotFound
			}
			return nil, err
		}
	}

	// Re-read so the response reflects the true post-update state (including
	// any quota_used growth during this call).
	updated, err := s.apiKeyRepo.GetSubKeyByIDForUser(ctx, accountKey.UserID, subKeyID)
	if err != nil {
		if errors.Is(err, ErrAPIKeyNotFound) {
			return nil, ErrSubKeyNotFound
		}
		return nil, err
	}
	return updated, nil
}

// DeleteSubKey soft-deletes a sub key owned by accountKey.
func (s *APIKeyService) DeleteSubKey(ctx context.Context, accountKey *APIKey, subKeyID int64) error {
	if accountKey.ParentKeyID != nil {
		return ErrAccountKeyRequired
	}

	subKey, err := s.apiKeyRepo.GetSubKeyByIDForUser(ctx, accountKey.UserID, subKeyID)
	if err != nil {
		if errors.Is(err, ErrAPIKeyNotFound) {
			return ErrSubKeyNotFound
		}
		return err
	}

	if err := s.apiKeyRepo.Delete(ctx, subKey.ID); err != nil {
		return fmt.Errorf("delete sub key: %w", err)
	}
	s.InvalidateAuthCacheByKey(ctx, subKey.Key)
	s.invalidateLockedBalance(accountKey.UserID)
	return nil
}

// GetSubKeyByIDForUser returns a sub key owned by userID. Wraps ErrAPIKeyNotFound → ErrSubKeyNotFound.
func (s *APIKeyService) GetSubKeyByIDForUser(ctx context.Context, userID, subKeyID int64) (*APIKey, error) {
	key, err := s.apiKeyRepo.GetSubKeyByIDForUser(ctx, userID, subKeyID)
	if err != nil {
		if errors.Is(err, ErrAPIKeyNotFound) {
			return nil, ErrSubKeyNotFound
		}
		return nil, err
	}
	return key, nil
}

// RotateAccountKey swaps the secret of an account key in place. The record ID,
// parent_key_id links from sub keys, quota and usage stay untouched — only the
// key string changes. Returns the new full key.
func (s *APIKeyService) RotateAccountKey(ctx context.Context, userID, keyID int64) (string, error) {
	apiKey, err := s.apiKeyRepo.GetByID(ctx, keyID)
	if err != nil {
		return "", err
	}
	if apiKey.UserID != userID {
		return "", ErrAPIKeyNotFound
	}
	if apiKey.ParentKeyID != nil {
		return "", ErrAccountKeyRequired
	}

	oldKey := apiKey.Key
	newKey, err := s.GenerateKey()
	if err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}

	if err := s.apiKeyRepo.RotateKey(ctx, keyID, newKey); err != nil {
		return "", err
	}

	// Old secret must stop authenticating immediately.
	s.InvalidateAuthCacheByKey(ctx, oldKey)
	s.InvalidateAuthCacheByKey(ctx, newKey)
	return newKey, nil
}
