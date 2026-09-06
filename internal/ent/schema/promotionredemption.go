package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// PromotionRedemption is the real usage-cap enforcement ledger for Promotion.usage_limit /
// max_units_per_customer (2026-09-06 flash-sale plan). Distinct from the pre-existing
// PromotionApplication (a bare audit row with no tenant_id/customer/channel/idempotency —
// confirmed via grep to never actually be written by any real order-finalize path): this table
// is the one both checkout channels (POS terminal via Service.ReserveRedemption directly, and
// ordering-backend via the S2S reserve endpoint) write to, so a cap is enforced GLOBALLY across
// both, not per-channel-siloed.
type PromotionRedemption struct {
	ent.Schema
}

func (PromotionRedemption) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("promotion_id", uuid.UUID{}),
		field.String("customer_key").
			Optional().
			Comment("Phone/customer-id/loyalty key identifying who redeemed; empty = anonymous/unknown (still counts toward the total cap, exempt from the per-customer cap)"),
		field.Enum("channel").
			Values("pos", "ordering").
			Comment("Which checkout surface this redemption came from"),
		field.String("order_id").
			Comment("Caller-supplied idempotency anchor: pos-api's client_reference/order_number (order not yet created when reserved), or ordering-backend's order/reservation reference. Plain string, not a foreign key -- the two channels use different id shapes."),
		field.Float("quantity").
			Comment("Units redeemed in this reservation; summed against usage_limit/max_units_per_customer"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

func (PromotionRedemption) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "promotion_id"),
		index.Fields("tenant_id", "promotion_id", "customer_key"),
		// Idempotency guard: retrying the same order/channel must never double-count a
		// redemption (e.g. a network-retried checkout, or an order-create call that fails
		// after reservation and is legitimately resubmitted with the same client reference).
		index.Fields("tenant_id", "promotion_id", "channel", "order_id").
			Unique().
			StorageKey("promotionredemption_idempotency"),
	}
}
