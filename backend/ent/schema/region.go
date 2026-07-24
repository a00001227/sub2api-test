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

// Region holds the schema definition for the Region entity — the egress-region
// dictionary. A proxy's `region` column stores a Region.code; display names
// (English / Chinese) are resolved from here, so region names live in exactly
// one place (no per-proxy region_zh, no static code→name map).
//
// code is an upper-case IATA-style airport/city code (e.g. SJC, NRT, ORD).
type Region struct {
	ent.Schema
}

func (Region) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "regions"},
	}
}

func (Region) Fields() []ent.Field {
	return []ent.Field{
		field.String("code").
			MaxLen(20).
			NotEmpty().
			Unique().
			Comment("Upper-case region code (IATA-style, e.g. SJC/NRT/ORD). Stored on proxies.region."),
		field.String("name_en").
			MaxLen(60).
			NotEmpty().
			Comment("English display name, e.g. \"San Jose\"."),
		field.String("name_zh").
			MaxLen(60).
			NotEmpty().
			Comment("Chinese display name, e.g. \"圣何塞\"."),
		field.Int("sort_order").
			Default(0).
			Comment("Ascending display order in pickers."),
		field.Bool("enabled").
			Default(true).
			Comment("Disabled regions are hidden from provider pickers but keep resolving names."),
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

func (Region) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("code").Unique(),
		index.Fields("enabled"),
		index.Fields("sort_order"),
	}
}
