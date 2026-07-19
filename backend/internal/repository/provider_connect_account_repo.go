package repository

import (
	"context"
	"fmt"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	dbgroup "github.com/Wei-Shaw/sub2api/ent/group"
	dbproxyalloc "github.com/Wei-Shaw/sub2api/ent/proxyallocation"
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

	// Phase 21E-6E proxy-exclusive: record the exclusive proxy allocation in
	// the SAME transaction. The partial unique index (uq_proxy_alloc_active_
	// proxy) guarantees exclusivity — if the proxy was concurrently taken, this
	// INSERT fails, the whole create rolls back, and no half-bound account or
	// double-allocated proxy is left. Only when a proxy is actually bound.
	if in.ProxyID != nil {
		ab := tx.ProxyAllocation.Create().
			SetProxyID(*in.ProxyID).
			SetExternalProviderAccountID(in.ExternalProviderAccountID).
			SetAccountID(row.ID).
			SetAllocationStatus("assigned").
			SetAssignedAt(time.Now())
		if in.Region != nil {
			ab = ab.SetRegion(*in.Region)
		}
		if _, err := ab.Save(ctx); err != nil {
			return 0, err
		}
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

// ReleaseAllocationByExternalRef 把某 provider 账号的活跃占用（reserved/assigned）
// 置为 released，代理回到可分配池。幂等：无活跃占用时更新 0 行、返回 nil。
func (r *providerConnectAccountRepository) ReleaseAllocationByExternalRef(
	ctx context.Context, externalRef, reason string,
) error {
	_, err := r.client.ProxyAllocation.Update().
		Where(
			dbproxyalloc.ExternalProviderAccountIDEQ(externalRef),
			dbproxyalloc.AllocationStatusIn("reserved", "assigned"),
		).
		SetAllocationStatus("released").
		SetReleasedAt(time.Now()).
		SetReleaseReason(reason).
		Save(ctx)
	return err
}
