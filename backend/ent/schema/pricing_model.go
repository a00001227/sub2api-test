package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
)

// PricingModel 统一 Pricing Display System 的数据实体。
//
// 单表设计，同时支持 text 和 image 两种模型类型。
// image 模型的 resolution 定价通过 image_pricing_json 字段以 JSON 存储，支持动态 key。
type PricingModel struct {
	ent.Schema
}

func (PricingModel) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "pricing_models"},
	}
}

func (PricingModel) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
	}
}

func (PricingModel) Fields() []ent.Field {
	return []ent.Field{
		field.String("model").
			MaxLen(200).
			NotEmpty(),

		field.String("model_type").
			MaxLen(20).
			Default("text"),

		field.String("user_type").
			MaxLen(20).
			Default("end_user"),

		field.Bool("enabled").
			Default(true),

		// TEXT MODEL fields
		field.Float("input_price").
			Optional().
			Nillable(),

		field.Float("output_price").
			Optional().
			Nillable(),

		field.Float("cache_read_price").
			Optional().
			Nillable(),

		field.Float("cache_write_price").
			Optional().
			Nillable(),

		field.Float("official_input_price").
			Optional().
			Nillable(),

		field.Float("official_output_price").
			Optional().
			Nillable(),

		// IMAGE MODEL field — stores JSON: {"1k": 0.005, "2k": 0.01}
		field.String("image_pricing_json").
			Optional().
			Nillable(),

		// Computed by backend PricingDisplayService
		field.Float("saving_percent").
			Default(0),
	}
}

func (PricingModel) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("model", "user_type").
			Unique(),
		index.Fields("enabled"),
	}
}
