package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/Bengo-Hub/pagination"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/promotion"
	"github.com/bengobox/pos-service/internal/ent/promotionredemption"
	promotions "github.com/bengobox/pos-service/internal/modules/promotions"
	"github.com/bengobox/pos-service/internal/platform/subscriptions"
)

// S2S discount endpoints — pos-api's Promotion + PromotionRule are the platform's
// DISCOUNT SOURCE OF TRUTH. Other services (inventory-api, ordering-backend,
// treasury-api, erp-api) must NOT define parallel discount/coupon entities; they
// list, create, and apply discounts against these endpoints (X-API-Key /
// INTERNAL_SERVICE_KEY, mounted under /api/v1/s2s/{tenant} — see router.go).
//
// The payload shapes are identical to the tenant-facing /pos/promotions handlers
// (createPromoInput / promotionWithRule), so the shared discount modal form used
// across frontends serializes one shape regardless of which service proxies it.

// S2SListDiscounts handles GET /api/v1/s2s/{tenant}/discounts?status=&kind=
// Returns the tenant's promotions with their discount rules attached. Defaults to
// active discounts; pass status=all for every status (management screens).
func (h *PromotionHandler) S2SListDiscounts(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(chi.URLParam(r, "tenant"))
	if err != nil {
		jsonError(w, "invalid tenant", http.StatusBadRequest)
		return
	}

	query := h.client.Promotion.Query().Where(promotion.TenantID(tid))
	switch status := r.URL.Query().Get("status"); status {
	case "all":
		// no status filter — management view
	case "":
		query = query.Where(promotion.Status("active"))
	default:
		query = query.Where(promotion.Status(status))
	}
	if kind := r.URL.Query().Get("kind"); kind != "" {
		query = query.Where(promotion.PromoKindEQ(promotion.PromoKind(kind)))
	}
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		query = query.Where(promotion.NameContainsFold(q))
	}

	p := pagination.Parse(r)
	total, _ := query.Clone().Count(r.Context())
	promos, err := query.Order(ent.Desc(promotion.FieldStartAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("s2s list discounts failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, pagination.NewResponse(h.attachRules(r.Context(), promos), total, p))
}

// S2SCreateDiscount handles POST /api/v1/s2s/{tenant}/discounts — creates a discount
// in the source of truth on behalf of another service's UI (shared discount modal).
// Body: createPromoInput (same shape as the tenant-facing POST /pos/promotions).
func (h *PromotionHandler) S2SCreateDiscount(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(chi.URLParam(r, "tenant"))
	if err != nil {
		jsonError(w, "invalid tenant", http.StatusBadRequest)
		return
	}
	var input createPromoInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.Name == "" {
		jsonError(w, "name required", http.StatusBadRequest)
		return
	}
	// S2S calls carry no user JWT (X-API-Key only), so bannerFeatureLocked falls back to the
	// tenant-id entitlement lookup (h.subs.ConsumerHasFeature) rather than claims.FeatureEnabled.
	if h.bannerFeatureLocked(r.Context(), tid, input) {
		authclient.WriteFeatureLocked(w, subscriptions.FeatureStorefrontBanner, "")
		return
	}
	promo, err := h.createPromotionFromInput(r.Context(), tid, input)
	if err != nil {
		h.log.Error("s2s create discount failed", zap.Error(err))
		jsonError(w, "failed to create discount: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	jsonOK(w, promo)
}

// S2SApplyDiscount handles POST /api/v1/s2s/{tenant}/discounts/apply
// Body: {promoCode, outlet_id, lines: [{sku, category, quantity, unit_price, added_at}]}.
// Validates the code against the caller's real cart lines and returns the rule-evaluated
// discount — the SAME evaluator (schedule/meal_period/scope/BOGO) the POS terminal uses via
// promotions.Service.ApplyPromoCode, so a code behaves identically no matter which service
// applies it (this is what ordering-backend calls instead of maintaining its own PromoCode
// evaluation — see promotions.Service.ApplyPromoCode's doc comment).
func (h *PromotionHandler) S2SApplyDiscount(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(chi.URLParam(r, "tenant"))
	if err != nil {
		jsonError(w, "invalid tenant", http.StatusBadRequest)
		return
	}
	var input struct {
		PromoCode   string                `json:"promoCode"`
		OutletID    string                `json:"outlet_id"`
		Lines       []applyPromoLineInput `json:"lines"`
		CustomerKey string                `json:"customer_key,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	var outletID *uuid.UUID
	if oid, perr := uuid.Parse(input.OutletID); perr == nil {
		outletID = &oid
	}
	result, err := h.promoSvc.ApplyPromoCode(r.Context(), tid, outletID, input.PromoCode, toTimedDiscountLines(input.Lines), input.CustomerKey)
	if err != nil {
		h.log.Error("s2s apply discount failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !result.Valid {
		jsonOK(w, map[string]any{"valid": false, "reason": result.Reason})
		return
	}
	jsonOK(w, map[string]any{
		"valid":          true,
		"promoCode":      result.PromoCode,
		"promoId":        result.PromoID,
		"discountAmount": result.DiscountAmount.StringFixed(2),
		"perSku":         result.PerSKU,
		"discountType":   result.DiscountType,
	})
}

// S2SReserveRedemption handles POST /api/v1/s2s/{tenant}/discounts/{promoId}/reserve — the real
// usage-cap enforcement write path for ordering-backend's online checkout. Body:
// {customer_key, order_id, quantity}. channel is always "ordering" here — pos-api's OWN order
// creation calls promotions.Service.ReserveRedemption directly (same binary, channel "pos"),
// this HTTP endpoint exists purely for the cross-service call. Idempotent on
// (tenant, promotion, channel, order_id): safe to retry with the same order_id.
func (h *PromotionHandler) S2SReserveRedemption(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(chi.URLParam(r, "tenant"))
	if err != nil {
		jsonError(w, "invalid tenant", http.StatusBadRequest)
		return
	}
	promoID, err := uuid.Parse(chi.URLParam(r, "promoId"))
	if err != nil {
		jsonError(w, "invalid promo_id", http.StatusBadRequest)
		return
	}
	var input struct {
		CustomerKey string  `json:"customer_key,omitempty"`
		OrderID     string  `json:"order_id"`
		Quantity    float64 `json:"quantity"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.OrderID == "" || input.Quantity <= 0 {
		jsonError(w, "order_id and a positive quantity are required", http.StatusBadRequest)
		return
	}
	res, err := h.promoSvc.ReserveRedemption(r.Context(), tid, promoID, input.CustomerKey, promotionredemption.ChannelOrdering, input.OrderID, input.Quantity)
	if err != nil {
		h.log.Error("s2s reserve redemption failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, res)
}

// s2sBanner is one active storefront-marketing-banner entry returned by S2SListBanners —
// only the fields the ordering-frontend banner widget needs to render, not the full
// promotion/rule shape (that's what /discounts already returns).
type s2sBanner struct {
	PromoID        uuid.UUID  `json:"promo_id"`
	Name           string     `json:"name"`
	BannerTitle    string     `json:"banner_title,omitempty"`
	BannerSubtitle string     `json:"banner_subtitle,omitempty"`
	BannerImageURL string     `json:"banner_image_url,omitempty"`
	CTALabel       string     `json:"cta_label,omitempty"`
	CTALink        string     `json:"cta_link,omitempty"`
	BannerColor    string     `json:"banner_color,omitempty"`
	TextColor      string     `json:"text_color,omitempty"`
	OutletID       *uuid.UUID `json:"outlet_id,omitempty"`
	// IsFlashSale + EndAt let the storefront render a live countdown; EndAt is the
	// promotion's own real end_at (already required by the active-window query above),
	// not a new field on the promotion itself.
	IsFlashSale bool       `json:"is_flash_sale,omitempty"`
	EndAt       *time.Time `json:"end_at,omitempty"`
}

// S2SListBanners handles GET /api/v1/s2s/{tenant}/discounts/banners?use_case=
// Returns active promotions flagged (via metadata["banner"].show_on_storefront, set by the
// POS Discounts page) to also surface as a marketing banner on the customer-facing ordering
// storefront (ordering-frontend, a separate app — it consumes this endpoint, never a parallel
// banner entity). "Active" mirrors the /discounts default: status=active and now within
// [start_at, end_at] (end_at nil = no upper bound). A promo whose banner.use_cases is
// non-empty only matches when the caller's use_case query param is in that list; an empty
// use_cases list means "show for every outlet use_case".
func (h *PromotionHandler) S2SListBanners(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(chi.URLParam(r, "tenant"))
	if err != nil {
		jsonError(w, "invalid tenant", http.StatusBadRequest)
		return
	}
	useCase := strings.TrimSpace(r.URL.Query().Get("use_case"))

	// Live entitlement re-check: a banner set while the tenant was on Pro/Gold must stop being
	// served the moment the tenant drops to Basic (or the feature is otherwise revoked) — no
	// separate cleanup job, just don't include it on the next read. Computed once per request
	// (one tenant per call) via the same fail-open consumer-entitlement lookup used at the point
	// of write. h.subs == nil (not wired) fails open, matching every other consumer gate.
	bannerEntitled := h.subs == nil || h.subs.ConsumerHasFeature(r.Context(), tid.String(), subscriptions.FeatureStorefrontBanner)

	now := time.Now()
	promos, err := h.client.Promotion.Query().
		Where(
			promotion.TenantID(tid),
			promotion.Status("active"),
			promotion.StartAtLTE(now),
			promotion.Or(promotion.EndAtIsNil(), promotion.EndAtGTE(now)),
		).
		Order(ent.Desc(promotion.FieldStartAt)).
		All(r.Context())
	if err != nil {
		h.log.Error("s2s list banners failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	out := make([]s2sBanner, 0, len(promos))
	for _, p := range promos {
		banner := promotions.BannerFromMetadata(p.Metadata)
		if !banner.ShowOnStorefront {
			continue
		}
		if !bannerEntitled {
			continue
		}
		if len(banner.UseCases) > 0 {
			matched := false
			for _, uc := range banner.UseCases {
				if uc == useCase {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		out = append(out, s2sBanner{
			PromoID:        p.ID,
			Name:           p.Name,
			BannerTitle:    banner.BannerTitle,
			BannerSubtitle: banner.BannerSubtitle,
			BannerImageURL: banner.BannerImageURL,
			CTALabel:       banner.CTALabel,
			CTALink:        banner.CTALink,
			BannerColor:    banner.BannerColor,
			TextColor:      banner.TextColor,
			OutletID:       p.OutletID,
		})
	}
	jsonOK(w, out)
}
