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
}

// NewProviderConnectAccountRepository creates the repository.
func NewProviderConnectAccountRepository(client *dbent.Client) service.ProviderConnectAccountRepository {
	return &providerConnectAccountRepository{client: client}
}

func (r *providerConnectAccountRepository) CreateConnectedAccount(
	ctx context.Context, in service.CreateConnectedAccountInput,
) (int64, error) {
	// 1) 解析该 platform 下所有活跃分组（决策 A：无组则拒绝创建）。
	//    先查再建，避免建了账号却绑不了组、留下不可消费的死账号。
	groupIDs, err := r.activeGroupIDsByPlatform(ctx, in.Platform)
	if err != nil {
		return 0, err
	}
	if len(groupIDs) == 0 {
		return 0, fmt.Errorf("no active group for platform %q: cannot create schedulable provider account", in.Platform)
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
		if err := ensureProxyBindingCapacity(ctx, tx, *in.ProxyID); err != nil {
			return 0, err
		}
	}

	builder := tx.Account.Create().
		SetName(in.Name).
		SetPlatform(in.Platform).
		SetType(service.AccountTypeOAuth).
		SetCredentials(normalizeJSONMap(in.Credentials)).
		SetStatus(service.StatusActive).
		SetExternalProviderAccountID(in.ExternalProviderAccountID)
	if in.ProxyID != nil {
		builder = builder.SetProxyID(*in.ProxyID)
	}

	row, err := builder.Save(ctx)
	if err != nil {
		return 0, err
	}

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

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return row.ID, nil
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

// ensureProxyBindingCapacity 在事务内校验代理还有绑定余量。先 FOR UPDATE 锁定
// 代理行以串行化对同一代理的并发绑定，再统计已绑定的活跃账号数；当
// max_bindings>0 且已达上限时返回 PROXY_CAPACITY_FULL。max_bindings=0 视为不限。
func ensureProxyBindingCapacity(ctx context.Context, tx *dbent.Tx, proxyID int64) error {
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
	// Count accounts currently bound to this proxy. sub2api hard-deletes
	// accounts, so a removed account drops out of the count automatically and
	// frees capacity; a disabled account still holds its proxy (it may resume),
	// so it is intentionally counted.
	bound, err := tx.Account.Query().
		Where(account.ProxyIDEQ(proxyID)).
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
