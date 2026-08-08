package service

import "context"

// #90-A2/B: provider 账号"配置面"——读/改单账号的可配置项。Portal(admin)经
// provider-internal 调用;"仅 admin"由 Portal 侧 AdminGuard 把关。
//
// 覆盖:model_mapping(白名单/映射)、priority(调度优先级)、max_sessions(会话上限)、
// intercept_warmup(拦截预热)、temp_unschedulable(临时不可调度开关+规则)。
// 不含 TLS 指纹 / 会话 ID 伪装(反封面,业务方自负)。
//
// 存储分散在 column(priority)/ extra(max_sessions)/ credentials(其余),由
// provider-connect 仓储在一个 ent 更新里 token-safe 地读改写(只改提供的字段)。

// providerConfigStore 是 ProviderConnectAccountRepository 的窄子集。
type providerConfigStore interface {
	GetAccountConfig(ctx context.Context, externalRef string) (snap *ProviderAccountConfigSnapshot, found bool, err error)
	UpdateAccountConfig(ctx context.Context, externalRef string, patch ProviderAccountConfigPatch) error
}

// ProviderAccountConfigSnapshot 是账号配置的全量快照(脱敏,不含 token)。
type ProviderAccountConfigSnapshot struct {
	ModelMapping       map[string]string
	Priority           int
	MaxSessions        int
	InterceptWarmup    bool
	TempUnschedEnabled bool
	TempUnschedRules   []TempUnschedulableRule
}

// ProviderAccountConfigPatch 是部分更新:字段为 nil = 不改该项。
type ProviderAccountConfigPatch struct {
	ModelMapping       map[string]string        // nil = 不改
	Priority           *int                     // nil = 不改
	MaxSessions        *int                     // nil = 不改
	InterceptWarmup    *bool                    // nil = 不改
	TempUnschedEnabled *bool                    // nil = 不改
	TempUnschedRules   *[]TempUnschedulableRule // nil = 不改
}

// ProviderAccountConfigInput 是服务入参(与 patch 同形)。
type ProviderAccountConfigInput = ProviderAccountConfigPatch

// ProviderAccountConfig 是回读给 Portal 的当前配置(JSON 契约,snake_case)。
type ProviderAccountConfig struct {
	ModelMapping       map[string]string       `json:"model_mapping,omitempty"`
	Priority           int                     `json:"priority"`
	MaxSessions        int                     `json:"max_sessions"`
	InterceptWarmup    bool                    `json:"intercept_warmup"`
	TempUnschedEnabled bool                    `json:"temp_unschedulable_enabled"`
	TempUnschedRules   []TempUnschedulableRule `json:"temp_unschedulable_rules"`
}

type ProviderAccountConfigService struct {
	store providerConfigStore
}

func NewProviderAccountConfigService(store providerConfigStore) *ProviderAccountConfigService {
	return &ProviderAccountConfigService{store: store}
}

func snapshotToConfig(s *ProviderAccountConfigSnapshot) *ProviderAccountConfig {
	return &ProviderAccountConfig{
		ModelMapping:       s.ModelMapping,
		Priority:           s.Priority,
		MaxSessions:        s.MaxSessions,
		InterceptWarmup:    s.InterceptWarmup,
		TempUnschedEnabled: s.TempUnschedEnabled,
		TempUnschedRules:   s.TempUnschedRules,
	}
}

// GetConfig 回读当前配置(供 Portal 编辑器展示当前值)。
func (s *ProviderAccountConfigService) GetConfig(ctx context.Context, externalRef string) (*ProviderAccountConfig, error) {
	snap, found, err := s.store.GetAccountConfig(ctx, externalRef)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrProviderAccountNotFound
	}
	return snapshotToConfig(snap), nil
}

// SetConfig 部分更新配置(nil 字段不改),回读落库后的实际值。
func (s *ProviderAccountConfigService) SetConfig(ctx context.Context, externalRef string, in ProviderAccountConfigInput) (*ProviderAccountConfig, error) {
	if err := s.store.UpdateAccountConfig(ctx, externalRef, in); err != nil {
		return nil, err // 含 ErrProviderAccountNotFound
	}
	return s.GetConfig(ctx, externalRef)
}
