package repository

import (
	"context"
	"fmt"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	dbgroup "github.com/Wei-Shaw/sub2api/ent/group"
	dbproxy "github.com/Wei-Shaw/sub2api/ent/proxy"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Phase 21E-6C-2C / 21E-6E group-bind: Provider Connect 完成流程的账号创建仓储。
//
// 创建一个 type=oauth、带 external_provider_account_id 的账号行，并绑定到
// 该 platform 下所有活跃分组（account_groups），使账号进入调度池、可被
// gateway 消费。ent schema 的默认值（schedulable=true / concurrency=3 /
// priority=50 / rate_multiplier=1.0 / auto_pause_on_expired=true）在 Create
// 时自动套用，与 admin 建的账号一致。
//
// group 绑定是 21E-6E 的核心修复：此前直写不绑组，账号进不了
// ListSchedulableByGroupID 的调度池 → 永不被消费 → 无收益。现按 platform
// 绑定所有活跃分组（gateway 按 platform 过滤，跨 platform 组绑了也无效）。
//
// 决策 A：若该 platform 没有任何活跃分组，则创建失败（不产生无法被消费的
// 死账号），由 service 层作为 ACCOUNT_CREATE_FAILED 上抛。
//
// 幂等由 accounts.external_provider_account_id 的部分唯一索引保证：并发/重放
// 的第二次 Create 命中唯一冲突并报错，由 service 层反查收敛。
type providerConnectAccountRepository struct {
	client *dbent.Client
	// edgeMode: on a cell, serving picks UNGROUPED local accounts (the edge
	// trusted-forward key has GroupID=nil → ListSchedulableUngrouped). So on a
	// cell we must NOT auto-bind groups — the central "bind all active groups"
	// rule (needed for group-based ListSchedulableByGroupID on central) would
	// make the account invisible to edge serving. See #85 invariant + CELL
	// handoff gotcha #4.
	edgeMode bool
}

// NewProviderConnectAccountRepository creates the repository. edgeMode gates the
// group-binding behavior (central binds all active platform groups; edge leaves
// the account ungrouped).
func NewProviderConnectAccountRepository(client *dbent.Client, edgeMode bool) service.ProviderConnectAccountRepository {
	return &providerConnectAccountRepository{client: client, edgeMode: edgeMode}
}

func (r *providerConnectAccountRepository) CreateConnectedAccount(
	ctx context.Context, in service.CreateConnectedAccountInput,
) (int64, error) {
	// 1) 解析该 platform 下所有活跃分组（决策 A：无组则拒绝创建）。
	//    先查再建，避免建了账号却绑不了组、留下不可消费的死账号。
	//    EDGE_MODE（cell）例外：serving 按「未分组本地号」选号（信任转发 key
	//    GroupID=nil → ListSchedulableUngrouped），因此 cell 上不绑组、也不要求
	//    有组——号保持未分组才能被 edge serving 选到（#85 不变量）。
	var groupIDs []int64
	if !r.edgeMode {
		var err error
		groupIDs, err = r.activeGroupIDsByPlatform(ctx, in.Platform)
		if err != nil {
			return 0, err
		}
		if len(groupIDs) == 0 {
			return 0, fmt.Errorf("no active group for platform %q: cannot create schedulable provider account", in.Platform)
		}
	}

	// 2) 事务内：建账号 + 绑定所有活跃分组，保证原子性。
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// Capacity check (replaces the former proxy_allocations exclusive INSERT):
	// lock the proxy row FOR UPDATE to serialize concurrent binds to the same
	// proxy, then ensure the accounts already bound are below max_bindings
	// (1 = exclusive, N = shared up to N, 0 = unlimited). Count + insert run in
	// one tx so two concurrent binds can't both slip past the limit.
	if in.ProxyID != nil {
		if err := ensureProxyBindingCapacity(ctx, tx, *in.ProxyID, in.Platform); err != nil {
			return 0, err
		}
	}

	// Phase 21I: provider 账号默认启用 DeRouter 五档 pacing，默认档 = humanized
	// （拟人，封号风险最低）。RPM/RPH/并发由档位容量表权威决定，不再写入
	// base_rpm/base_rph/window_cost_limit（预算刹车已删除）。max_sessions 仍按
	// 订阅等级分级（会话限制与档位正交）。admin 建的账号不受影响（无 pacing_mode）。
	tier, _ := in.Credentials["rate_limit_tier"].(string)
	profile := service.PacingProfileForTier(tier)
	defaultMode := service.PacingModeHumanized

	// 接入即默认配置(#90-A1):在既有 pacing/会话默认之上,叠加平台默认——拦截预热
	// (省 token)+ 临时不可调度规则(命中即临时绕开坏号,请求侧自愈)。仅 anthropic
	// 预置(关键词按 Anthropic 响应体英文串);其它平台留给「每账号配置编辑器」(#90-B)。
	// TLS 指纹 / 会话 ID 伪装属反封面(业务方自负),本项目不设,见 helper 内占位。
	extra := map[string]any{
		"pacing_mode":  defaultMode,
		"max_sessions": profile.MaxSessions,
	}
	creds := normalizeJSONMap(in.Credentials)
	extraDefaults, credDefaults := defaultProviderAccountConfig(in.Platform)
	for k, v := range extraDefaults {
		if _, exists := extra[k]; !exists {
			extra[k] = v
		}
	}
	for k, v := range credDefaults {
		if _, exists := creds[k]; !exists { // 不覆盖调用方已带的键
			creds[k] = v
		}
	}

	builder := tx.Account.Create().
		SetName(in.Name).
		SetPlatform(in.Platform).
		SetType(service.AccountTypeOAuth).
		SetCredentials(creds).
		SetStatus(service.StatusActive).
		SetExternalProviderAccountID(in.ExternalProviderAccountID).
		SetConcurrency(service.PacingModeConcurrencyFor(defaultMode)).
		SetExtra(extra)
	if in.ProxyID != nil {
		builder = builder.SetProxyID(*in.ProxyID)
	}

	row, err := builder.Save(ctx)
	if err != nil {
		return 0, err
	}

	// EDGE_MODE: groupIDs is empty → leave the account ungrouped (no binding).
	if len(groupIDs) > 0 {
		agBuilders := make([]*dbent.AccountGroupCreate, 0, len(groupIDs))
		for i, gid := range groupIDs {
			agBuilders = append(agBuilders, tx.AccountGroup.Create().
				SetAccountID(row.ID).
				SetGroupID(gid).
				SetPriority(i+1),
			)
		}
		if _, err := tx.AccountGroup.CreateBulk(agBuilders...).Save(ctx); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return row.ID, nil
}

// defaultProviderAccountConfig 返回接入时要叠加到号上的平台默认配置(#90-A1):
// 返回 (extra 追加, credentials 追加)。覆盖 anthropic(claude)与 openai(chatgpt/codex)
// ——两者能力面一致,拦截预热两条路径(claude / codex-responses)都认。gemini 暂不预置。
// temp_unschedulable 关键词按上游响应体英文串(小写子串)通配:429/503 对两平台都命中,
// 529(overloaded)是 anthropic 专属,对 openai 是无害空转(永不匹配)。
func defaultProviderAccountConfig(platform string) (extra map[string]any, creds map[string]any) {
	extra = map[string]any{}
	creds = map[string]any{}
	if platform != service.PlatformAnthropic && platform != service.PlatformOpenAI {
		return extra, creds
	}
	// 拦截预热:标题生成等预热请求返回 mock,不耗上游 token(默认开)。
	creds["intercept_warmup_requests"] = true
	// 临时不可调度:错误码 + 关键词同时命中 → 临时禁用 5 分钟(见 ratelimit_service
	// tryTempUnschedulable:rule.ErrorCode==statusCode 且关键词是响应体小写子串)。
	// 关键词与时长均沿用 sub2api admin 原预设(EditAccountModal.vue tempUnschedPresets):
	//   529 "overloaded, too many" 60min / 429 "rate limit, too many requests" 10min /
	//   503 "unavailable, maintenance" 30min。
	creds["temp_unschedulable_enabled"] = true
	creds["temp_unschedulable_rules"] = []any{
		map[string]any{"error_code": 529, "keywords": []any{"overloaded", "too many"}, "duration_minutes": 60, "description": "过载"},
		map[string]any{"error_code": 429, "keywords": []any{"rate limit", "too many requests"}, "duration_minutes": 10, "description": "限流"},
		map[string]any{"error_code": 503, "keywords": []any{"unavailable", "maintenance"}, "duration_minutes": 30, "description": "维护"},
	}
	// —— 反封面(业务方自负),本项目不设置;若要默认开启,你自行在此填 ——
	// extra["enable_tls_fingerprint"] = true
	// extra["tls_fingerprint_profile_id"] = <profileID>
	// extra["session_id_masking_enabled"] = true
	return extra, creds
}

// activeGroupIDsByPlatform 返回指定 platform 下所有活跃（未软删）分组 id。
func (r *providerConnectAccountRepository) activeGroupIDsByPlatform(
	ctx context.Context, platform string,
) ([]int64, error) {
	rows, err := r.client.Group.Query().
		Where(
			dbgroup.PlatformEQ(platform),
			dbgroup.StatusEQ(service.StatusActive),
		).
		IDs(ctx)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *providerConnectAccountRepository) FindAccountIDByExternalRef(
	ctx context.Context, externalRef string,
) (int64, bool, error) {
	row, err := r.client.Account.Query().
		Where(account.ExternalProviderAccountIDEQ(externalRef)).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return row.ID, true, nil
}

// ReleaseAllocationByExternalRef 在容量计数模型下为 no-op：绑定关系即
// accounts.proxy_id，代理的可用容量由"活跃账号计数 < max_bindings"实时决定，
// 账号被移除/失效后计数自动下降、代理自动回到可用池，无需显式释放记录。
// 保留此方法以满足 completion service 的接口契约（幂等、始终成功）。
func (r *providerConnectAccountRepository) ReleaseAllocationByExternalRef(
	ctx context.Context, externalRef, reason string,
) error {
	_ = ctx
	_ = externalRef
	_ = reason
	return nil
}

// GetAccountConfig 读单账号配置全量快照(#90-A2/B)。未找到 → (nil,false,nil)。
// 复用规范 getter 解析分散在 column/extra/credentials 的各项(脱敏,不含 token)。
func (r *providerConnectAccountRepository) GetAccountConfig(
	ctx context.Context, externalRef string,
) (*service.ProviderAccountConfigSnapshot, bool, error) {
	row, err := r.client.Account.Query().
		Where(account.ExternalProviderAccountIDEQ(externalRef)).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	acc := &service.Account{
		Credentials: row.Credentials,
		Extra:       row.Extra,
		Priority:    row.Priority,
	}
	return &service.ProviderAccountConfigSnapshot{
		ModelMapping:       acc.GetModelMapping(),
		Priority:           row.Priority,
		MaxSessions:        acc.GetMaxSessions(),
		InterceptWarmup:    acc.IsInterceptWarmupEnabled(),
		TempUnschedEnabled: acc.IsTempUnschedulableEnabled(),
		TempUnschedRules:   acc.GetTempUnschedulableRules(),
	}, true, nil
}

// UpdateAccountConfig token-safe 部分更新单账号配置(#90-A2/B):读全量行 → 只改
// patch 里提供(非 nil)的字段(分散在 priority 列 / extra.max_sessions /
// credentials.{model_mapping,intercept_warmup_requests,temp_unschedulable_*})
// → 一次 ent 更新写回,绝不动 OAuth token 与其它未涉及的键。
func (r *providerConnectAccountRepository) UpdateAccountConfig(
	ctx context.Context, externalRef string, patch service.ProviderAccountConfigPatch,
) error {
	row, err := r.client.Account.Query().
		Where(account.ExternalProviderAccountIDEQ(externalRef)).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return service.ErrProviderAccountNotFound
		}
		return err
	}

	upd := r.client.Account.UpdateOneID(row.ID)

	// credentials 相关:model_mapping / intercept_warmup / temp_unschedulable_*。
	credsChanged := false
	creds := map[string]any{}
	for k, v := range row.Credentials {
		creds[k] = v
	}
	if patch.ModelMapping != nil {
		mm := make(map[string]any, len(patch.ModelMapping))
		for k, v := range patch.ModelMapping {
			mm[k] = v
		}
		creds["model_mapping"] = mm
		credsChanged = true
	}
	if patch.InterceptWarmup != nil {
		creds["intercept_warmup_requests"] = *patch.InterceptWarmup
		credsChanged = true
	}
	if patch.TempUnschedEnabled != nil {
		creds["temp_unschedulable_enabled"] = *patch.TempUnschedEnabled
		credsChanged = true
	}
	if patch.TempUnschedRules != nil {
		rules := make([]any, 0, len(*patch.TempUnschedRules))
		for _, rule := range *patch.TempUnschedRules {
			kw := make([]any, 0, len(rule.Keywords))
			for _, k := range rule.Keywords {
				kw = append(kw, k)
			}
			rules = append(rules, map[string]any{
				"error_code":       rule.ErrorCode,
				"keywords":         kw,
				"duration_minutes": rule.DurationMinutes,
				"description":      rule.Description,
			})
		}
		creds["temp_unschedulable_rules"] = rules
		credsChanged = true
	}
	if credsChanged {
		upd = upd.SetCredentials(creds)
	}

	// extra.max_sessions。
	if patch.MaxSessions != nil {
		extra := map[string]any{}
		for k, v := range row.Extra {
			extra[k] = v
		}
		extra["max_sessions"] = *patch.MaxSessions
		upd = upd.SetExtra(extra)
	}

	// priority 列。
	if patch.Priority != nil {
		upd = upd.SetPriority(*patch.Priority)
	}

	_, err = upd.Save(ctx)
	return err
}

// ensureProxyBindingCapacity 在事务内校验代理对某个平台还有绑定余量。先
// FOR UPDATE 锁定代理行以串行化对同一代理的并发绑定，再统计该代理上**同平台**
// 已绑定的账号数；当 max_bindings>0 且已达上限时返回 PROXY_CAPACITY_FULL。
// max_bindings=0 视为不限。
//
// 容量按 platform 分桶（方案 B3）：max_bindings 的语义是"每个平台各自的上限"，
// 而非该代理的总账号数。因此 claude 与 codex 各占独立的名额、互不挤占：
// max_bindings=1 时，同一个代理可以绑 1 个 claude + 1 个 openai。
func ensureProxyBindingCapacity(ctx context.Context, tx *dbent.Tx, proxyID int64, platform string) error {
	p, err := tx.Proxy.Query().
		Where(dbproxy.IDEQ(proxyID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return service.ErrProxyNotFound
		}
		return err
	}
	if p.MaxBindings <= 0 {
		return nil // unlimited
	}
	// Count accounts of THIS platform currently bound to this proxy. sub2api
	// hard-deletes accounts, so a removed account drops out of the count
	// automatically and frees capacity; a disabled account still holds its
	// proxy (it may resume), so it is intentionally counted.
	bound, err := tx.Account.Query().
		Where(
			account.ProxyIDEQ(proxyID),
			account.PlatformEQ(platform),
		).
		Count(ctx)
	if err != nil {
		return err
	}
	if bound >= p.MaxBindings {
		return infraerrors.Conflict("PROXY_CAPACITY_FULL",
			"proxy binding capacity reached")
	}
	return nil
}
