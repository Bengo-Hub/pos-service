package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// Promotion holds the schema definition for the Promotion entity.
type Promotion struct {
	ent.Schema
}

// Fields of the Promotion.
func (Promotion) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Outlet scope; nil = applies to all outlets for this tenant"),
		field.String("name").
			NotEmpty(),
		field.String("description").
			Optional(),
		field.String("promo_code").
			Unique().
			Optional().
			Nillable(),
		field.Enum("promo_kind").
			Values("code", "happy_hour", "auto").
			Default("code").
			Comment("code = manual promo code; happy_hour = time-windowed auto discount; auto = always-on auto discount"),
		field.JSON("days_of_week", []int{}).
			Optional().
			Comment("Days the promo is active (0=Sun..6=Sat); empty = all days. Used by happy_hour"),
		field.String("window_start").
			Optional().
			Comment("Daily activation start time HH:MM (happy_hour)"),
		field.String("window_end").
			Optional().
			Comment("Daily activation end time HH:MM (happy_hour)"),
		field.Bool("auto_apply").
			Default(false).
			Comment("Apply automatically at checkout without a code (happy_hour/auto)"),
		field.String("status").
			Default("active"),
		field.Time("start_at").
			Default(time.Now),
		field.Time("end_at").
			Optional().
			Nillable(),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
		// Redemption caps (2026-09-06 flash-sale plan, real enforcement pass) — checked/tracked
		// via PromotionRedemption + Service.ReserveRedemption, GLOBALLY across both channels
		// (POS terminal + ordering-frontend), since this is the single discount source of truth.
		// nil/0 = unlimited, matching every other optional-cap field in this codebase.
		field.Int("usage_limit").
			Optional().
			Nillable().
			Comment("Total redemption cap across all channels combined; nil/0 = unlimited"),
		field.Int("max_units_per_customer").
			Optional().
			Nillable().
			Comment("Per-customer redemption cap, matched by phone/customer key; nil/0 = unlimited"),
	}
}
