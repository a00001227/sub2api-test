package schema

import (
	"fmt"

	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// providerConnectSessionStatuses 是渠道商接入会话的合法状态集。
// pending    → 会话已创建，等待渠道商完成授权
// authorized → 授权码已交换成功，账号尚未落库
// completed  → 账号已创建并绑定 external_provider_account_id
// expired    → 超过 expires_at 未完成（终态）
// failed     → 授权/交换/落库失败（终态，保留人工排查）
var providerConnectSessionStatuses = map[string]struct{}{
	"pending":    {},
	"authorized": {},
	"completed":  {},
	"expired":    {},
	"failed":     {},
}

func validateProviderConnectSessionStatus(value string) error {
	if _, ok := providerConnectSessionStatuses[value]; ok {
		return nil
	}
	return fmt.Errorf("invalid provider connect session status %q", value)
}

// ProviderConnectSession 记录 Provider Portal 渠道商账号的授权接入过程
// （Phase 21E-6C-2A：本阶段仅建表，OAuth 接入属后续阶段）。
// 与 pending_auth_sessions（用户登录后决策会话）无关——这是账号资产
// 接入域的会话，生命周期跨分钟级且必须在进程重启后存活，故落库而非
// 复用内存 SessionStore。
type ProviderConnectSession struct {
	ent.Schema
}

func (ProviderConnectSession) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "provider_connect_sessions"},
	}
}

func (ProviderConnectSession) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
	}
}

func (ProviderConnectSession) Fields() []ent.Field {
	return []ent.Field{
		field.String("external_provider_account_id").
			MaxLen(64).
			NotEmpty().
			Comment("Portal 侧账号引用（pa_<uuid>）。同一引用可多次开启会话（重试），非唯一。"),
		field.String("provider_type").
			MaxLen(32).
			NotEmpty().
			Comment("目标 AI 平台类型（claude/codex/openai/gemini …）。"),
		field.String("region").
			MaxLen(20).
			Optional().Nillable().
			Comment("请求的出口区域偏好；NULL = 未指定，由分配策略决定。"),
		field.Int64("proxy_id").
			Optional().Nillable().
			Comment("分配到的代理 id；pending 早期可为 NULL，分配后写入。"),
		field.String("status").
			MaxLen(20).
			Default("pending").
			Validate(validateProviderConnectSessionStatus).
			Comment("pending | authorized | completed | expired | failed"),
		field.String("oauth_session_id").
			MaxLen(128).
			Optional().Nillable().
			Comment("关联的 OAuth 内存会话 id；ExchangeCode 的必需入参。"),
		field.Int64("sub2api_account_id").
			Optional().Nillable().
			Comment("完成后创建的 Sub2API 账号 id；completed 时写入，幂等重入据此返回已有结果。"),
		field.String("callback_url").
			MaxLen(512).
			NotEmpty().
			Comment("Portal 事件回调地址（webhook 目标）。"),
		field.Time("expires_at").
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).
			Comment("会话过期时间；过期未完成的会话由后续阶段的清扫置为 expired。"),
		field.Time("completed_at").
			Optional().Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).
			Comment("到达 completed 终态的时间。"),
	}
}

func (ProviderConnectSession) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("external_provider_account_id"),
		index.Fields("status"),
		index.Fields("expires_at"),
	}
}
