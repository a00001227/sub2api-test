package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Feedback holds the schema definition for user feedback / support tickets.
//
// 删除策略：硬删除（随用户外键级联删除）
type Feedback struct {
	ent.Schema
}

func (Feedback) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "feedbacks"},
	}
}

func (Feedback) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("user_id").
			Comment("提交反馈的用户ID"),
		field.String("type").
			MaxLen(40).
			NotEmpty().
			Comment("反馈类型: api_error, billing, key_mgmt, feature"),
		field.String("content").
			SchemaType(map[string]string{dialect.Postgres: "text"}).
			NotEmpty().
			Comment("反馈内容描述"),
		field.String("request_id").
			MaxLen(200).
			Optional().
			Nillable().
			Comment("关联的请求ID（可选）"),
		field.String("status").
			MaxLen(20).
			Default("pending").
			Comment("状态: pending(未处理), resolved(已处理)"),
		field.String("admin_reply").
			SchemaType(map[string]string{dialect.Postgres: "text"}).
			Optional().
			Nillable().
			Comment("管理员回复内容"),
		field.Time("replied_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).
			Comment("回复时间"),
		field.Time("created_at").
			Immutable().
			Default(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (Feedback) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("status"),
		index.Fields("created_at"),
		index.Fields("user_id"),
	}
}
