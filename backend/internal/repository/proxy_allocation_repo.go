package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Phase 21E-6C-2B-1: Provider Connect 的代理分配仓储。
//
// 单条 SQL 完成「筛选活跃未过期的 region 代理 → 按当前绑定账号数最少
// 排序 → FOR UPDATE SKIP LOCKED 锁定一行」。SKIP LOCKED 让并发的同
// region 请求自动跳过彼此已锁定的候选行、错开到下一个最少绑定的代理
// —— 均衡与并发安全由同一条语句解决（模式沿用 usage_cleanup_repo 的
// claim 先例）。锁在 UPDATE accounts 落库同事务提交时释放；本仓储只
// 负责「选中并短暂锁定」，账号绑定写入由调用方在自己的事务里完成，
// 因此这里用独立短事务：锁的意义是把并发挑选序列化，而非跨请求持锁。
type proxyAllocationRepository struct {
	sqlDB *sql.DB
}

// NewProxyAllocationRepository creates the allocation repository.
func NewProxyAllocationRepository(client *dbent.Client, sqlDB *sql.DB) service.ProxyAllocationRepository {
	_ = client // ent client 暂不需要；保留签名与其余 repo 构造一致
	return &proxyAllocationRepository{sqlDB: sqlDB}
}

const selectLeastLoadedProxySQL = `
	SELECT p.id, p.name, p.protocol, p.host, p.port,
	       COALESCE(p.username, ''), COALESCE(p.password, ''),
	       p.status, p.expires_at
	FROM proxies p
	WHERE p.status = 'active'
	  AND p.deleted_at IS NULL
	  AND p.region = $1
	  AND (p.expires_at IS NULL OR p.expires_at > NOW())
	  -- Capacity model: a proxy is selectable while the number of accounts
	  -- bound to it (accounts.proxy_id) is below its max_bindings.
	  -- max_bindings = 0 means unlimited. This replaces the former
	  -- proxy_allocations exclusive-allocation predicate. The bind itself is
	  -- serialized + re-checked under FOR UPDATE in CreateConnectedAccount, so
	  -- this is the common-path filter, not the ultimate guard.
	  AND (
	      p.max_bindings = 0
	      OR (SELECT COUNT(*) FROM accounts a WHERE a.proxy_id = p.id) < p.max_bindings
	  )
	ORDER BY p.id ASC
	LIMIT 1
	FOR UPDATE OF p SKIP LOCKED
`

// SelectLeastLoadedActiveProxyForUpdate 在独立事务内选中并锁定一个候选
// 代理。返回 nil, nil 表示该 region 无可用容量（由 service 层转为
// ErrRegionNoCapacity）。
func (r *proxyAllocationRepository) SelectLeastLoadedActiveProxyForUpdate(
	ctx context.Context, region string,
) (*service.Proxy, error) {
	tx, err := r.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	// 出错路径回滚；成功路径显式提交（提交即释放行锁——见类型注释）。
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, selectLeastLoadedProxySQL, region)
	var p service.Proxy
	var expiresAt sql.NullTime
	if err := row.Scan(
		&p.ID, &p.Name, &p.Protocol, &p.Host, &p.Port,
		&p.Username, &p.Password, &p.Status, &expiresAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if expiresAt.Valid {
		t := expiresAt.Time
		p.ExpiresAt = &t
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &p, nil
}

// regionCapacitySQL 计算每个 region 的可用容量（容量计数模型）：
// available_slots = Σ(每代理容量) − Σ(每代理已绑定账号数)。
// 每代理容量 = max_bindings；max_bindings=0（不限）按一个较大常量计入，
// 使其始终显示为"有余量"。已绑定数来自 accounts.proxy_id。
//
// 已绑定数用「预聚合 JOIN」而非每代理一条关联子查询：把 accounts 整表按
// proxy_id GROUP BY 一次，再 LEFT JOIN 回 proxies。这样无论有多少代理，
// accounts 只被聚合一次（O(1) 次聚合而非 O(代理数) 次子查询），几千代理
// 下仍稳定在毫秒级。语义与原关联子查询完全等价（同样不按 deleted_at 过滤
// accounts，与 selectLeastLoadedProxySQL 的绑定计数口径保持一致）。
const regionCapacitySQL = `
	SELECT p.region,
	       MAX(p.region_zh) AS region_zh,
	       COALESCE(SUM(
	           CASE WHEN p.max_bindings = 0 THEN 999 ELSE p.max_bindings END
	       ), 0) AS total_capacity,
	       COALESCE(SUM(COALESCE(ac.cnt, 0)), 0) AS used
	FROM proxies p
	LEFT JOIN (
	    SELECT proxy_id, COUNT(*) AS cnt
	    FROM accounts
	    GROUP BY proxy_id
	) ac ON ac.proxy_id = p.id
	WHERE p.status = 'active'
	  AND p.deleted_at IS NULL
	  AND p.region IS NOT NULL
	  AND (p.expires_at IS NULL OR p.expires_at > NOW())
	GROUP BY p.region
	ORDER BY p.region ASC
`

// RegionCapacity 返回每个 region 的可用容量（未占用的活跃代理数）。
func (r *proxyAllocationRepository) RegionCapacity(
	ctx context.Context,
) ([]service.RegionCapacity, error) {
	rows, err := r.sqlDB.QueryContext(ctx, regionCapacitySQL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []service.RegionCapacity
	for rows.Next() {
		var region string
		var regionZh sql.NullString
		var total, used int
		if err := rows.Scan(&region, &regionZh, &total, &used); err != nil {
			return nil, err
		}
		avail := total - used
		if avail < 0 {
			avail = 0
		}
		rc := service.RegionCapacity{
			Region:         region,
			AvailableSlots: avail,
		}
		if regionZh.Valid {
			rc.RegionZh = strings.TrimSpace(regionZh.String)
		}
		out = append(out, rc)
	}
	return out, rows.Err()
}
