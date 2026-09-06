package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/Bengo-Hub/pagination"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/promotion"
	"github.com/bengobox/pos-service/internal/ent/promotionrule"
	promotions "github.com/bengobox/pos-service/internal/modules/promotions"
	"github.com/bengobox/pos-service/internal/platform/subscriptions"
)

// PromotionHandler handles promotion management endpoints.
type PromotionHandler struct {
	log      *zap.Logger
	client   *ent.Client
	promoSvc *promotions.Service
	subs     *subscriptions.Client
}

func NewPromotionHandler(log *zap.Logger, client *ent.Client, promoSvc *promotions.Service) *PromotionHandler {
	return &PromotionHandler{log: log, client: client, promoSvc: promoSvc}
}

// SetSubscriptionsClient injects the subscriptions S2S client used to enforce the
// storefront-banner feature gate (subscriptions.FeatureStorefrontBanner, Pro/Gold PowerSuite
// only) both at the point of write (create/update, via bannerFeatureLocked) and the point of
// read (S2SListBanners, promotions_s2s.go).
func (h *PromotionHandler) SetSubscriptionsClient(c *subscriptions.Client) {
	h.subs = c
}

// bannerFeatureLocked reports whether input turns ON the storefront banner
// (banner.show_on_storefront) while the tenant is NOT entitled to
// subscriptions.FeatureStorefrontBanner. It checks the request's JWT claims first — the
// tenant-facing POST/PUT /pos/promotions path carries a user token, so this is a real,
// non-fail-open entitlement check via the same claims.FeatureEnabled the fleet-wide
// RequireFeatureCode middleware uses. When no claims are present (the S2S create path,
// authenticated only by X-API-Key — see promotions_s2s.go) it falls back to the tenant-id
// entitlement lookup (h.subs.ConsumerHasFeature), which mirrors the HTTP gate but fails OPEN
// on a subscriptions-api outage or when no subscriptions client is wired, matching every
// other consumer-side entitlement check in this codebase (see consumer_gate.go).
func (h *PromotionHandler) bannerFeatureLocked(ctx context.Context, tid uuid.UUID, input createPromoInput) bool {
	if input.Banner == nil || !input.Banner.ShowOnStorefront {
		return false
	}
	if claims, ok := authclient.ClaimsFromContext(ctx); ok {
		return !claims.FeatureEnabled(subscriptions.FeatureStorefrontBanner)
	}
	if h.subs == nil {
		return false
	}
	return !h.subs.ConsumerHasFeature(ctx, tid.String(), subscriptions.FeatureStorefrontBanner)
}

// promotionWithRule is a Promotion embedding its discount PromotionRule (nil if none was
// ever created) — the item picker, discount summary, and edit form on the frontend all
// need the rule's scope_ids/discount_type/discount_value/buy_get fields alongside the
// promotion itself, so every promotion-returning endpoint serializes this shape instead of
// the bare ent.Promotion (which has no formal edge to PromotionRule to eager-load).
type promotionWithRule struct {
	*ent.Promotion
	Rule *ent.PromotionRule `json:"rule"`
}

// attachRules batch-fetches each promotion's discount rule (one query, not N+1) and pairs
// them up for serialization.
func (h *PromotionHandler) attachRules(ctx context.Context, promos []*ent.Promotion) []promotionWithRule {
	out := make([]promotionWithRule, len(promos))
	if len(promos) == 0 {
		return out
	}
	ids := make([]uuid.UUID, len(promos))
	for i, p := range promos {
		ids[i] = p.ID
	}
	rules, err := h.client.PromotionRule.Query().Where(promotionrule.PromotionIDIn(ids...)).All(ctx)
	if err != nil {
		h.log.Warn("attach promotion rules: query failed", zap.Error(err))
	}
	ruleByPromoID := make(map[uuid.UUID]*ent.PromotionRule, len(rules))
	for _, ru := range rules {
		ruleByPromoID[ru.PromotionID] = ru
	}
	for i, p := range promos {
		out[i] = promotionWithRule{Promotion: p, Rule: ruleByPromoID[p.ID]}
	}
	return out
}

// ListPromotions handles GET /{tenantID}/pos/promotions
func (h *PromotionHandler) ListPromotions(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	query := h.client.Promotion.Query().Where(promotion.TenantID(tid))

	// status="" keeps the legacy active-only default; status=all lists every status
	// (the Discounts management page needs inactive/expired rows too).
	status := r.URL.Query().Get("status")
	switch status {
	case "":
		query = query.Where(promotion.Status("active"))
	case "all":
		// no status filter
	default:
		query = query.Where(promotion.Status(status))
	}
	// Outlet scoping — ONLY for the checkout-consumption case (ApplyDiscountModal's active list),
	// never for status=all (the Discounts management page, which must show every tenant-wide AND
	// every other outlet's discount so an admin can manage them all from one place). A promo with
	// no outlet_id applies everywhere; one with an outlet_id must match the requesting outlet —
	// otherwise a discount scoped to Outlet B was still manually appliable at Outlet A's checkout
	// (the auto-apply path was already outlet-filtered via ActiveHappyHours; this closes the same
	// gap on the manual "Apply Discount" list).
	if status != "all" {
		if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
			if oid, perr := uuid.Parse(oidStr); perr == nil {
				query = query.Where(promotion.Or(promotion.OutletIDIsNil(), promotion.OutletID(oid)))
			}
		}
	}
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		query = query.Where(promotion.NameContainsFold(q))
	}

	p := pagination.Parse(r)
	total, _ := query.Clone().Count(r.Context())
	promos, err := query.Order(ent.Desc(promotion.FieldStartAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list promotions failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, pagination.NewResponse(h.attachRules(r.Context(), promos), total, p))
}

// GetPromotion handles GET /{tenantID}/pos/promotions/{promoID} — single promotion with its
// discount rule, for the edit form.
func (h *PromotionHandler) GetPromotion(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	promoID, err := uuid.Parse(chi.URLParam(r, "promoID"))
	if err != nil {
		jsonError(w, "invalid promo_id", http.StatusBadRequest)
		return
	}
	promo, err := h.client.Promotion.Query().Where(promotion.ID(promoID), promotion.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "promotion not found", http.StatusNotFound)
		return
	}
	rule, _ := h.client.PromotionRule.Query().Where(promotionrule.PromotionID(promoID)).First(r.Context())
	jsonOK(w, promotionWithRule{Promotion: promo, Rule: rule})
}

type createPromoInput struct {
	PromoCode string     `json:"promoCode"`
	Name      string     `json:"name"`
	StartAt   *time.Time `json:"startAt"`
	EndAt     *time.Time `json:"endAt"`
	// Snake-case aliases: the frontends serialize start_at/end_at/promo_code (mirroring the
	// ent READ shape), while the original fields above keep the legacy camelCase contract.
	// Without these, one-time promotion dates sent as start_at/end_at were silently dropped
	// (StartAt defaulted to now, EndAt to nil). normalize() merges them.
	PromoCodeAlias string     `json:"promo_code"`
	StartAtAlias   *time.Time `json:"start_at"`
	EndAtAlias     *time.Time `json:"end_at"`
	// Happy-hour / auto-apply fields
	PromoKind   string `json:"promo_kind"`   // code | happy_hour | auto
	OutletID    string `json:"outlet_id"`    // optional outlet scope
	DaysOfWeek  []int  `json:"days_of_week"` // 0=Sun..6=Sat
	WindowStart string `json:"window_start"` // HH:MM
	WindowEnd   string `json:"window_end"`   // HH:MM
	AutoApply   bool   `json:"auto_apply"`
	// Discount rule
	ScopeType     string   `json:"scope_type"`    // all | category | item — for BOGO, the "buy" scope
	ScopeIDs      []string `json:"scope_ids"`     // inventory category ids / skus
	DiscountType  string   `json:"discount_type"` // percentage | fixed_amount | fixed_price | bogo
	DiscountValue float64  `json:"discount_value"`
	MaxDiscount   *float64 `json:"max_discount"`
	MealPeriod    string   `json:"meal_period"` // optional: breakfast|am_break|lunch|pm_break|dinner (negotiated meal rate)
	// BOGO-only ("buy X get Y [at N% off]"): meaningful when DiscountType == "bogo" and
	// ScopeType == "item". Zero values fall back to sane defaults (1 buy, 1 get, 100% off)
	// in the rule builder below.
	BuyQuantity        int     `json:"buy_quantity"`
	GetQuantity        int     `json:"get_quantity"`
	GetDiscountPercent float64 `json:"get_discount_percent"`
	// GetScopeIDs enables CROSS-ITEM BOGO: SKUs eligible for the free/discounted "get" unit
	// when they are a DIFFERENT item from ScopeIDs — e.g. ScopeIDs=Large pizzas,
	// GetScopeIDs=Small pizzas ("buy one large, get one small free"). Empty = same-SKU BOGO.
	GetScopeIDs []string `json:"get_scope_ids"`
	// GetPairMap is the CORRESPONDING cross-item pairing: each buy SKU → its one specific free
	// get SKU (e.g. "PIZ003"→"PIZ001" = buy Margherita Large, get Margherita Small free). When
	// set, the terminal auto-adds the mapped item and the evaluator frees exactly it (not the
	// cheapest get-scope item). ScopeIDs/GetScopeIDs are derived from its keys/values so the
	// scope-based paths stay consistent.
	GetPairMap map[string]string `json:"get_pair_map"`
	// Redemption caps (nil = unlimited / leave unset on create, clear on update — same
	// nil-means-unlimited convention as MaxDiscount). Enforced by promotions.Service.ReserveRedemption
	// against the promotion_redemptions ledger, shared globally across POS + ordering-frontend.
	UsageLimit          *int `json:"usage_limit"`
	MaxUnitsPerCustomer *int `json:"max_units_per_customer"`
	// Banner optionally flags this promotion to also appear as a marketing banner on the
	// customer-facing ordering storefront. Nil = caller didn't touch banner config (leave
	// whatever's already in metadata alone on update; create simply omits it). Stored via
	// read-merge-write into Promotion.metadata["banner"] — see promotions.MergeBannerMetadata.
	Banner *promotions.PromotionBannerConfig `json:"banner"`
}

// normalize merges the snake_case alias fields into the canonical ones so every consumer
// (create, update, S2S create) sees one shape regardless of which key style the caller used.
func (in *createPromoInput) normalize() {
	if in.PromoCode == "" {
		in.PromoCode = in.PromoCodeAlias
	}
	if in.StartAt == nil {
		in.StartAt = in.StartAtAlias
	}
	if in.EndAt == nil {
		in.EndAt = in.EndAtAlias
	}
}

// pairMapScopes derives the buy scope_ids (keys) and get scope_ids (values) from a corresponding
// pair map, so the two scopes can never drift from the map that actually drives the deal. Returns
// (nil,nil) for an empty map so callers fall back to the explicitly-supplied scopes.
func pairMapScopes(m map[string]string) (buy []string, get []string) {
	if len(m) == 0 {
		return nil, nil
	}
	for k, v := range m {
		if k = strings.TrimSpace(k); k != "" {
			buy = append(buy, k)
		}
		if v = strings.TrimSpace(v); v != "" {
			get = append(get, v)
		}
	}
	sort.Strings(buy)
	sort.Strings(get)
	return buy, get
}

// CreatePromotion handles POST /{tenantID}/pos/promotions
func (h *PromotionHandler) CreatePromotion(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input createPromoInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if h.bannerFeatureLocked(r.Context(), tid, input) {
		authclient.WriteFeatureLocked(w, subscriptions.FeatureStorefrontBanner, "")
		return
	}

	promo, err := h.createPromotionFromInput(r.Context(), tid, input)
	if err != nil {
		h.log.Error("create promotion failed", zap.Error(err))
		jsonError(w, "failed to create promotion: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, promo)
}

// createPromotionFromInput persists a promotion + its discount rule from one input payload.
// Shared by the tenant-facing CreatePromotion handler and the S2S discount create endpoint
// (promotions_s2s.go) so the discount source-of-truth write path lives in exactly one place.
func (h *PromotionHandler) createPromotionFromInput(ctx context.Context, tid uuid.UUID, input createPromoInput) (*ent.Promotion, error) {
	input.normalize()
	if input.PromoCode == "" {
		input.PromoCode = "PROMO-" + uuid.New().String()[:8]
	}

	builder := h.client.Promotion.Create().
		SetTenantID(tid).
		SetName(input.Name).
		SetPromoCode(input.PromoCode).
		SetAutoApply(input.AutoApply).
		SetStatus("active")
	if input.PromoKind != "" {
		builder = builder.SetPromoKind(promotion.PromoKind(input.PromoKind))
	}
	if input.WindowStart != "" {
		builder = builder.SetWindowStart(input.WindowStart)
	}
	if input.WindowEnd != "" {
		builder = builder.SetWindowEnd(input.WindowEnd)
	}
	if len(input.DaysOfWeek) > 0 {
		builder = builder.SetDaysOfWeek(input.DaysOfWeek)
	}
	if oid, perr := uuid.Parse(input.OutletID); perr == nil {
		builder = builder.SetOutletID(oid)
	}
	if input.StartAt != nil {
		builder.SetStartAt(*input.StartAt)
	}
	if input.EndAt != nil {
		builder.SetEndAt(*input.EndAt)
	}
	if input.Banner != nil {
		builder = builder.SetMetadata(promotions.MergeBannerMetadata(nil, *input.Banner))
	}
	builder = builder.SetNillableUsageLimit(input.UsageLimit).SetNillableMaxUnitsPerCustomer(input.MaxUnitsPerCustomer)
	promo, err := builder.Save(ctx)
	if err != nil {
		return nil, err
	}

	// Persist the discount rule when provided (scope + discount type/value/cap). BOGO
	// carries its deal in buy/get quantities rather than DiscountValue, so it must trigger
	// this block on its own even when DiscountValue is left at 0.
	if input.DiscountValue > 0 || input.ScopeType != "" || input.DiscountType == "bogo" {
		scopeType := input.ScopeType
		if scopeType == "" {
			scopeType = "all"
		}
		discountType := input.DiscountType
		if discountType == "" {
			discountType = "percentage"
		}
		scopeIDs, getScopeIDs := input.ScopeIDs, input.GetScopeIDs
		if b, g := pairMapScopes(input.GetPairMap); b != nil {
			scopeIDs, getScopeIDs = b, g
		}
		rb := h.client.PromotionRule.Create().
			SetPromotionID(promo.ID).
			SetRuleType("discount").
			SetScopeType(promotionrule.ScopeType(scopeType)).
			SetScopeIds(scopeIDs).
			SetGetScopeIds(getScopeIDs).
			SetGetPairMap(input.GetPairMap).
			SetDiscountType(promotionrule.DiscountType(discountType)).
			SetDiscountValue(input.DiscountValue)
		if input.MaxDiscount != nil {
			rb = rb.SetMaxDiscount(*input.MaxDiscount)
		}
		if input.MealPeriod != "" {
			rb = rb.SetMealPeriod(promotionrule.MealPeriod(input.MealPeriod))
		}
		if input.ScopeType == "" && len(input.ScopeIDs) > 0 {
			rb = rb.SetScopeType(promotionrule.ScopeTypeItem)
		}
		if discountType == "bogo" {
			buyQty, getQty, getPct := input.BuyQuantity, input.GetQuantity, input.GetDiscountPercent
			if buyQty <= 0 {
				buyQty = 1
			}
			if getQty <= 0 {
				getQty = 1
			}
			if getPct <= 0 {
				getPct = 100
			}
			rb = rb.SetBuyQuantity(buyQty).SetGetQuantity(getQty).SetGetDiscountPercent(getPct)
		}
		if _, rerr := rb.Save(ctx); rerr != nil {
			h.log.Error("create promotion rule failed", zap.Error(rerr))
		}
	}

	return promo, nil
}

// GetActiveHappyHours handles GET /{tenantID}/pos/promotions/happy-hour/active —
// returns auto-apply happy-hour promotions live right now for the request's outlet.
func (h *PromotionHandler) GetActiveHappyHours(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	var outletID *uuid.UUID
	if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, perr := uuid.Parse(oidStr); perr == nil {
			outletID = &oid
		}
	}
	active, err := h.promoSvc.ActiveHappyHours(r.Context(), tid, outletID, time.Now())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Embed each promo's rule (scope_ids/discount_type/buy-get) — the terminal's
	// add-to-cart waiter alert and the happy-hour "Active now" card both need to know
	// WHICH items are covered and WHAT the deal is, not just that something is live.
	jsonOK(w, h.attachRules(r.Context(), active))
}

// applyPromoLineInput is one cart line the promo-code evaluator scopes/schedules against — the
// same minimal shape ANY caller (POS terminal, Add Sale, ordering-backend over S2S) already has
// in hand for its cart, so the evaluator can enforce item/category scope, BOGO pairing, and
// meal_period exactly as it does for auto-apply discounts, instead of an amount-only flat calc.
type applyPromoLineInput struct {
	SKU       string     `json:"sku"`
	Category  string     `json:"category"`
	Quantity  float64    `json:"quantity"`
	UnitPrice float64    `json:"unit_price"`
	AddedAt   *time.Time `json:"added_at"` // zero/omitted = "now"
}

func toTimedDiscountLines(in []applyPromoLineInput) []promotions.TimedDiscountLine {
	out := make([]promotions.TimedDiscountLine, 0, len(in))
	for _, l := range in {
		unit := decimal.NewFromFloat(l.UnitPrice)
		tl := promotions.TimedDiscountLine{
			DiscountLine: promotions.DiscountLine{
				SKU:       l.SKU,
				Category:  l.Category,
				Quantity:  l.Quantity,
				UnitPrice: unit,
				Total:     unit.Mul(decimal.NewFromFloat(l.Quantity)),
			},
		}
		if l.AddedAt != nil {
			tl.AddedAt = *l.AddedAt
		}
		out = append(out, tl)
	}
	return out
}

// ApplyPromoCode handles POST /{tenantID}/pos/promotions/apply — validates a promo code against
// the caller's real cart lines and returns the rule-evaluated discount (see promotions.Service.
// ApplyPromoCode for why this is lines-based, not a flat amount).
func (h *PromotionHandler) ApplyPromoCode(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
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
	oidStr := input.OutletID
	if oidStr == "" {
		oidStr = httpware.GetOutletID(r.Context())
	}
	if oid, perr := uuid.Parse(oidStr); perr == nil {
		outletID = &oid
	}

	result, err := h.promoSvc.ApplyPromoCode(r.Context(), tid, outletID, input.PromoCode, toTimedDiscountLines(input.Lines), input.CustomerKey)
	if err != nil {
		h.log.Error("apply promo code failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if !result.Valid {
		jsonOK(w, map[string]any{
			"valid":  false,
			"reason": result.Reason,
		})
		return
	}

	jsonOK(w, map[string]any{
		"valid":          true,
		"promoCode":      result.PromoCode,
		"promoId":        result.PromoID,
		"discountAmount": result.DiscountAmount.StringFixed(2),
		"perSku":         result.PerSKU,
	})
}

// UpdatePromotion handles PATCH /{tenantID}/pos/promotions/{promoID} — edits an existing
// promotion (happy hour or otherwise) and upserts its discount rule. Reuses createPromoInput
// so the create and edit forms on the frontend can share one payload shape; every field is
// applied unconditionally (the edit form always submits the full, current state — there is
// no partial-patch semantics here, matching how the create endpoint already works).
func (h *PromotionHandler) UpdatePromotion(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	promoID, err := uuid.Parse(chi.URLParam(r, "promoID"))
	if err != nil {
		jsonError(w, "invalid promo_id", http.StatusBadRequest)
		return
	}
	existing, err := h.client.Promotion.Query().Where(promotion.ID(promoID), promotion.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "promotion not found", http.StatusNotFound)
		return
	}

	var input createPromoInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	input.normalize()

	if h.bannerFeatureLocked(r.Context(), tid, input) {
		authclient.WriteFeatureLocked(w, subscriptions.FeatureStorefrontBanner, "")
		return
	}

	upd := existing.Update().
		SetName(input.Name).
		SetAutoApply(input.AutoApply).
		SetDaysOfWeek(input.DaysOfWeek).
		SetWindowStart(input.WindowStart).
		SetWindowEnd(input.WindowEnd)
	if input.PromoKind != "" {
		upd = upd.SetPromoKind(promotion.PromoKind(input.PromoKind))
	}
	if input.StartAt != nil {
		upd = upd.SetStartAt(*input.StartAt)
	}
	if input.EndAt != nil {
		upd = upd.SetEndAt(*input.EndAt)
	} else {
		upd = upd.ClearEndAt()
	}
	if oid, perr := uuid.Parse(input.OutletID); perr == nil {
		upd = upd.SetOutletID(oid)
	} else {
		upd = upd.ClearOutletID()
	}
	if input.Banner != nil {
		upd = upd.SetMetadata(promotions.MergeBannerMetadata(existing.Metadata, *input.Banner))
	}
	if input.UsageLimit != nil {
		upd = upd.SetUsageLimit(*input.UsageLimit)
	} else {
		upd = upd.ClearUsageLimit()
	}
	if input.MaxUnitsPerCustomer != nil {
		upd = upd.SetMaxUnitsPerCustomer(*input.MaxUnitsPerCustomer)
	} else {
		upd = upd.ClearMaxUnitsPerCustomer()
	}
	promo, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("update promotion failed", zap.Error(err))
		jsonError(w, "failed to update promotion: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Upsert the discount rule (same field set as CreatePromotion — one rule per promotion).
	scopeType := input.ScopeType
	if scopeType == "" {
		scopeType = "all"
	}
	discountType := input.DiscountType
	if discountType == "" {
		discountType = "percentage"
	}
	scopeIDs, getScopeIDs := input.ScopeIDs, input.GetScopeIDs
	if b, g := pairMapScopes(input.GetPairMap); b != nil {
		scopeIDs, getScopeIDs = b, g
	}
	rule, rerr := h.client.PromotionRule.Query().Where(promotionrule.PromotionID(promoID)).First(r.Context())
	var rb *ent.PromotionRuleUpdateOne
	if rerr == nil && rule != nil {
		rb = rule.Update().
			SetScopeType(promotionrule.ScopeType(scopeType)).
			SetScopeIds(scopeIDs).
			SetGetScopeIds(getScopeIDs).
			SetDiscountType(promotionrule.DiscountType(discountType)).
			SetDiscountValue(input.DiscountValue)
		// Set the corresponding pair map, or clear it when the deal is no longer a paired
		// cross-item BOGO (edit form unticked "different free item" / switched discount type).
		if len(input.GetPairMap) > 0 {
			rb = rb.SetGetPairMap(input.GetPairMap)
		} else {
			rb = rb.ClearGetPairMap()
		}
	}
	if rb == nil {
		created, cerr := h.client.PromotionRule.Create().
			SetPromotionID(promoID).
			SetRuleType("discount").
			SetScopeType(promotionrule.ScopeType(scopeType)).
			SetScopeIds(scopeIDs).
			SetGetScopeIds(getScopeIDs).
			SetGetPairMap(input.GetPairMap).
			SetDiscountType(promotionrule.DiscountType(discountType)).
			SetDiscountValue(input.DiscountValue).
			Save(r.Context())
		if cerr != nil {
			h.log.Error("update promotion: create rule failed", zap.Error(cerr))
		} else {
			rule = created
		}
	} else {
		if input.MaxDiscount != nil {
			rb = rb.SetMaxDiscount(*input.MaxDiscount)
		} else {
			rb = rb.ClearMaxDiscount()
		}
		if input.MealPeriod != "" {
			rb = rb.SetMealPeriod(promotionrule.MealPeriod(input.MealPeriod))
		} else {
			rb = rb.ClearMealPeriod()
		}
		if discountType == "bogo" {
			buyQty, getQty, getPct := input.BuyQuantity, input.GetQuantity, input.GetDiscountPercent
			if buyQty <= 0 {
				buyQty = 1
			}
			if getQty <= 0 {
				getQty = 1
			}
			if getPct <= 0 {
				getPct = 100
			}
			rb = rb.SetBuyQuantity(buyQty).SetGetQuantity(getQty).SetGetDiscountPercent(getPct)
		}
		if _, uerr := rb.Save(r.Context()); uerr != nil {
			h.log.Error("update promotion rule failed", zap.Error(uerr))
		}
	}

	rule, _ = h.client.PromotionRule.Query().Where(promotionrule.PromotionID(promoID)).First(r.Context())
	jsonOK(w, promotionWithRule{Promotion: promo, Rule: rule})
}

// DeletePromotion handles DELETE /{tenantID}/pos/promotions/{promoID} — soft-deletes by
// setting status=inactive rather than hard-deleting, since a PromotionApplication audit row
// may reference this promotion from a past sale (a hard delete would orphan that FK-less
// reference and break "which promo discounted this order" history).
func (h *PromotionHandler) DeletePromotion(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	promoID, err := uuid.Parse(chi.URLParam(r, "promoID"))
	if err != nil {
		jsonError(w, "invalid promo_id", http.StatusBadRequest)
		return
	}
	n, err := h.client.Promotion.Update().
		Where(promotion.ID(promoID), promotion.TenantID(tid)).
		SetStatus("inactive").
		Save(r.Context())
	if err != nil {
		h.log.Error("delete promotion failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if n == 0 {
		jsonError(w, "promotion not found", http.StatusNotFound)
		return
	}
	jsonOK(w, map[string]any{"deleted": true})
}
