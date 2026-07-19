package schema

import (
	"fmt"

	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// proxyAllocationStatuses 是代理独占分配的合法状态集（Phase 21E-6E proxy-exclusive）。
// reserved → 预留（V1 未使用；为将来 OAuth 授权窗口预留占位保留）
// assigned → 已分配给某 provider 账号并绑定 proxy（活跃占用）
// released → 账号移除后归还，代理回到可分配池（终态）
var proxyAllocationStatuses = map[string]struct{}{
	"reserved": {},
	"assigned": {},
	"released": {},
}

func validateProxyAllocationStatus(value string) error {
	if _, ok := proxyAllocationStatuses[value]; ok {
		return nil
	}
	return fmt.Errorf("invalid proxy allocation status %q", value)
}

// ProxyAllocation 记录一个 provider 账号对一个代理资源的独占占用。
// 独占不变量由两个部分唯一索引在 DB 层保证：
//   - (proxy_id) WHERE status IN(reserved,assigned)：一个代理至多一个活跃占用
//   - (external_provider_account_id) WHERE status IN(...)：一个账号至多一个活跃占用
//
// 仅 provider 账号（external_provider_account_id 非空）进入此表；admin 账号
// 不使用，继续共享代理。
type ProxyAllocation struct {
	ent.Schema
}

func (ProxyAllocation) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "proxy_allocations"},
	}
}

func (ProxyAllocation) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
	}
}

func (ProxyAllocation) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("proxy_id"),
		field.String("external_provider_account_id").
			MaxLen(64).
			NotEmpty(),
		field.Int64("account_id").
			Optional().Nillable().
			Comment("Sub2API account id; set once the account row exists."),
		field.String("region").
			MaxLen(20).
			Optional().Nillable().
			Comment("Egress region label snapshot (IATA-style, e.g. lax/sgp/nrt)."),
		field.String("allocation_status").
			MaxLen(20).
			Default("assigned").
			Validate(validateProxyAllocationStatus),
		field.Time("assigned_at").
			Optional().Nillable(),
		field.Time("released_at").
			Optional().Nillable(),
		field.String("release_reason").
			MaxLen(64).
			Optional().Nillable(),
	}
}

func (ProxyAllocation) Indexes() []ent.Index {
	return []ent.Index{
		// 独占：一个 proxy 至多一个活跃占用（部分唯一，SQL 迁移创建）。
		// 一个账号至多一个活跃占用（部分唯一，SQL 迁移创建）。
		// ent 的部分索引条件需在 migration 层用原生 SQL 表达，这里声明
		// 普通索引用于容量聚合与查询。
		index.Fields("region", "allocation_status"),
		index.Fields("proxy_id", "allocation_status"),
		index.Fields("external_provider_account_id", "allocation_status"),
	}
}
