package promotions

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/promotion"
	"github.com/bengobox/pos-service/internal/ent/promotionredemption"
)

// ReserveResult is the outcome of a redemption check/reservation against a promotion's
// usage_limit / max_units_per_customer caps.
type ReserveResult struct {
	Reserved bool   `json:"reserved"`
	Reason   string `json:"reason,omitempty"` // "" when reserved; "usage_limit_reached" | "customer_limit_reached" otherwise
}

// capCheck loads the promotion's caps and the existing redemption totals needed to evaluate
// them, reading through client (either s.client for a read-only preview, or a transaction-bound
// client for a real reservation so the check and the subsequent insert see a consistent view).
// Returns (nil, nil) when the promotion has neither cap configured — callers should treat that
// as "always allowed" and skip the rest of the reservation dance entirely.
func capCheck(ctx context.Context, client *ent.Client, tenantID, promotionID uuid.UUID, customerKey string, quantity float64) (*ReserveResult, error) {
	promo, err := client.Promotion.Query().
		Where(promotion.ID(promotionID), promotion.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("promotions: load promotion %s: %w", promotionID, err)
	}
	if promo.UsageLimit == nil && promo.MaxUnitsPerCustomer == nil {
		return nil, nil
	}

	if promo.UsageLimit != nil {
		total, err := client.PromotionRedemption.Query().
			Where(promotionredemption.TenantID(tenantID), promotionredemption.PromotionID(promotionID)).
			Aggregate(ent.Sum(promotionredemption.FieldQuantity)).
			Float64(ctx)
		if err != nil {
			return nil, fmt.Errorf("promotions: sum total redemptions: %w", err)
		}
		if total+quantity > float64(*promo.UsageLimit) {
			return &ReserveResult{Reserved: false, Reason: "usage_limit_reached"}, nil
		}
	}

	if promo.MaxUnitsPerCustomer != nil && customerKey != "" {
		total, err := client.PromotionRedemption.Query().
			Where(
				promotionredemption.TenantID(tenantID),
				promotionredemption.PromotionID(promotionID),
				promotionredemption.CustomerKey(customerKey),
			).
			Aggregate(ent.Sum(promotionredemption.FieldQuantity)).
			Float64(ctx)
		if err != nil {
			return nil, fmt.Errorf("promotions: sum customer redemptions: %w", err)
		}
		if total+quantity > float64(*promo.MaxUnitsPerCustomer) {
			return &ReserveResult{Reserved: false, Reason: "customer_limit_reached"}, nil
		}
	}

	return &ReserveResult{Reserved: true}, nil
}

// PreviewRedemption reports whether quantity more units of promotionID could be redeemed right
// now, WITHOUT reserving/consuming anything — a read-only check for a code-preview/apply-time
// UI (e.g. ApplyPromoCode) so a customer sees "this deal is sold out" before completing checkout,
// without a preview call permanently consuming a slot for a cart that's later abandoned.
func (s *Service) PreviewRedemption(ctx context.Context, tenantID, promotionID uuid.UUID, customerKey string, quantity float64) (*ReserveResult, error) {
	res, err := capCheck(ctx, s.client, tenantID, promotionID, customerKey, quantity)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return &ReserveResult{Reserved: true}, nil
	}
	return res, nil
}

// ReserveRedemption is the real cap-enforcement write path for Promotion.usage_limit /
// max_units_per_customer — the single choke point BOTH checkout channels call (POS terminal
// calls this directly since it's the same binary; ordering-backend calls it via the S2S reserve
// endpoint), so a cap is enforced GLOBALLY across both, not per-channel-siloed.
//
// Idempotent on (tenant_id, promotion_id, channel, order_id): a retried call with the same
// orderID (e.g. pos-api's client_reference before the real order exists yet, or a network-retried
// checkout) returns the ALREADY-RECORDED outcome rather than reserving twice. orderID should be
// whatever stable idempotency anchor the caller already has BEFORE the order is guaranteed to be
// created — this deliberately does not require a real, already-persisted order id.
func (s *Service) ReserveRedemption(ctx context.Context, tenantID, promotionID uuid.UUID, customerKey string, channel promotionredemption.Channel, orderID string, quantity float64) (*ReserveResult, error) {
	if quantity <= 0 {
		return &ReserveResult{Reserved: true}, nil
	}

	// Idempotency short-circuit: a prior successful reservation for this exact (promo, channel,
	// order) always "succeeds" again on retry, without re-summing/re-inserting.
	existing, err := s.client.PromotionRedemption.Query().
		Where(
			promotionredemption.TenantID(tenantID),
			promotionredemption.PromotionID(promotionID),
			promotionredemption.ChannelEQ(channel),
			promotionredemption.OrderID(orderID),
		).
		Exist(ctx)
	if err != nil {
		return nil, fmt.Errorf("promotions: check existing redemption: %w", err)
	}
	if existing {
		return &ReserveResult{Reserved: true}, nil
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("promotions: begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	res, cErr := capCheck(ctx, tx.Client(), tenantID, promotionID, customerKey, quantity)
	if cErr != nil {
		err = cErr
		return nil, err
	}
	if res != nil && !res.Reserved {
		if err = tx.Rollback(); err != nil {
			return nil, fmt.Errorf("promotions: rollback after cap rejection: %w", err)
		}
		return res, nil
	}

	create := tx.PromotionRedemption.Create().
		SetTenantID(tenantID).
		SetPromotionID(promotionID).
		SetChannel(channel).
		SetOrderID(orderID).
		SetQuantity(quantity)
	if customerKey != "" {
		create = create.SetCustomerKey(customerKey)
	}
	if _, cErr := create.Save(ctx); cErr != nil {
		// A concurrent reservation racing the SAME idempotency key loses here (unique
		// constraint) -- that's fine, it means another request already recorded this exact
		// order's redemption; treat as reserved rather than erroring the caller's checkout.
		if ent.IsConstraintError(cErr) {
			_ = tx.Rollback()
			return &ReserveResult{Reserved: true}, nil
		}
		err = fmt.Errorf("promotions: create redemption: %w", cErr)
		return nil, err
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("promotions: commit redemption: %w", err)
	}
	return &ReserveResult{Reserved: true}, nil
}
