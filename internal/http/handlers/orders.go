package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	entfacility "github.com/bengobox/pos-service/internal/ent/facility"
	"github.com/bengobox/pos-service/internal/ent/outletsetting"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	"github.com/bengobox/pos-service/internal/ent/promotionredemption"
	"github.com/bengobox/pos-service/internal/ent/tender"
	outletmw "github.com/bengobox/pos-service/internal/http/middleware"
	"github.com/bengobox/pos-service/internal/modules/inventory"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/payments"
	"github.com/bengobox/pos-service/internal/modules/promotions"
	"github.com/bengobox/pos-service/internal/modules/treasury"
	"github.com/bengobox/pos-service/internal/platform/subscriptions"
)

// POSOrderHandler handles POS order CRUD endpoints.
type POSOrderHandler struct {
	log        *zap.Logger
	client     *ent.Client
	orderSvc   *orders.Service
	subsClient *subscriptions.Client
	auditSvc   *audit.Service
	// rbac backs the per-cashier visibility scoping (pos.orders.view_own) on order reads.
	rbac outletmw.PermissionChecker
	// terminalSecret verifies manager-PIN step-up approval tokens for sensitive actions.
	terminalSecret []byte
	// inventoryClient propagates order-line price corrections to the inventory catalog
	// (EditOrderLine's update_catalog_price option). Optional — nil skips propagation.
	inventoryClient *inventory.Client
	// treasuryClient is used ONLY to check whether a specific order actually has a KRA
	// eTIMS-signed invoice before a void-refusal message mentions eTIMS at all — many
	// tenants are not eTIMS-integrated, and an unconditional "KRA" mention scared them
	// into thinking they were being monitored. Optional — nil means the message never
	// mentions eTIMS (safer default than a false claim).
	treasuryClient *treasury.Client
	// promoSvc enforces Promotion.usage_limit/max_units_per_customer at order-creation time when
	// the order carries a PromotionID (see reserveOrderPromotion). Optional — nil skips the
	// check entirely (no caps enforced), same as before this existed.
	promoSvc *promotions.Service
	// paymentSvc lets a total-reducing line edit re-evaluate payment completion (see
	// recheckCompletionAfterTotalsChange): voiding/reducing a line on a still-open order
	// after a partial payment was already taken can drop the new total to at-or-below what's
	// already been collected, and nothing else re-checks that order for completion afterward
	// (order #000504, urban-loft, 2026-08-13 — stuck "open" with paid_total==total_amount
	// since a line was voided 29 minutes after a partial mpesa_manual payment exactly closed
	// the gap). Optional — nil skips the recheck, same as before this fix existed.
	paymentSvc *payments.Service
	// readClient, when set, is used ONLY for the All-Sales list query (see rc()) — a heavy,
	// staleness-tolerant read routed to a replica when one is configured. Every other method on
	// this handler (order creation/edits/voids, everything else) always uses client (primary),
	// unchanged.
	readClient *ent.Client
}

func NewPOSOrderHandler(log *zap.Logger, client *ent.Client, orderSvc *orders.Service, subsClient *subscriptions.Client) *POSOrderHandler {
	return &POSOrderHandler{log: log, client: client, orderSvc: orderSvc, subsClient: subsClient}
}

// SetReadClient wires an optional read-replica Ent client for the All-Sales list query. Nil (the
// default) means rc() falls back to the primary client — zero behavior change when unset.
func (h *POSOrderHandler) SetReadClient(c *ent.Client) { h.readClient = c }

// rc returns the read-replica client when one is configured, else the primary — see readClient.
func (h *POSOrderHandler) rc() *ent.Client {
	if h.readClient != nil {
		return h.readClient
	}
	return h.client
}

// SetAuditService wires the centralized audit trail for void/line-removal events.
func (h *POSOrderHandler) SetAuditService(a *audit.Service) { h.auditSvc = a }

// SetInventoryClient wires the inventory S2S client used to propagate order-line price
// corrections to the catalog (EditOrderLine's update_catalog_price option).
func (h *POSOrderHandler) SetInventoryClient(c *inventory.Client) { h.inventoryClient = c }

// SetPaymentService wires the payments service so a total-reducing line edit can recheck
// payment completion afterward (see recheckCompletionAfterTotalsChange).
func (h *POSOrderHandler) SetPaymentService(p *payments.Service) { h.paymentSvc = p }

// recheckCompletionAfterTotalsChange re-evaluates whether an order's already-completed
// payments now cover its (just-reduced) total, driving it through the real completion path
// if so. Voiding or shrinking a line on a still-open order never removes money already
// collected, only the total owed — so a line change that happens to close the remaining gap
// must trigger the same completion check a payment submission would, or the order is left
// stuck open forever with paid_total already equal to total_amount (see paymentSvc's doc
// comment on POSOrderHandler for the incident this fixes). Best-effort: the line
// change itself already committed, so a recheck failure is only logged, never surfaced as
// an error on this response.
func (h *POSOrderHandler) recheckCompletionAfterTotalsChange(ctx context.Context, tid, orderID uuid.UUID) {
	if h.paymentSvc == nil {
		return
	}
	if err := h.paymentSvc.RecheckCompletion(ctx, tid, orderID); err != nil {
		h.log.Warn("recheck order completion after totals change failed",
			zap.String("order_id", orderID.String()), zap.Error(err))
	}
}

// SetTreasuryClient wires the treasury S2S client used only to confirm fiscal status before a
// void-refusal message mentions KRA eTIMS (see isFiscalized).
func (h *POSOrderHandler) SetTreasuryClient(c *treasury.Client) { h.treasuryClient = c }

// SetPromotionsService wires the redemption-cap enforcement used by reserveOrderPromotion.
func (h *POSOrderHandler) SetPromotionsService(p *promotions.Service) { h.promoSvc = p }

// isFiscalized reports whether this order has a KRA eTIMS-signed treasury invoice — the same
// criterion saledelete.Service.isFiscalized uses. Best-effort: nil client or a lookup error
// means "don't claim eTIMS involvement", never the reverse, so a non-integrated tenant is never
// told their sale was reported to KRA.
func (h *POSOrderHandler) isFiscalized(ctx context.Context, tenantSlug string, orderID uuid.UUID) bool {
	if h.treasuryClient == nil {
		return false
	}
	inv, err := h.treasuryClient.GetInvoiceByReference(ctx, tenantSlug, "pos_order", orderID.String())
	return err == nil && inv != nil && inv.ID != ""
}

// finalizedVoidRefusalMessage builds the "can't void a finalized sale" message, only mentioning
// KRA eTIMS when this specific order actually has a signed invoice — see isFiscalized.
func (h *POSOrderHandler) finalizedVoidRefusalMessage(ctx context.Context, tenantSlug string, orderID uuid.UUID) string {
	msg := "this sale is already finalized and can't be voided directly — use Edit Sale to adjust its items/amounts, or Delete Sale to remove it entirely"
	if h.isFiscalized(ctx, tenantSlug, orderID) {
		msg += " (this sale was reported to KRA eTIMS, so removing it issues a credit note rather than a plain void)"
	}
	return msg
}

// SetRBAC wires the local RBAC fallback used by the per-cashier visibility scoping.
func (h *POSOrderHandler) SetRBAC(rbac outletmw.PermissionChecker) { h.rbac = rbac }

// ownOrdersPredicate returns the visibility predicate for principals limited to their OWN
// sales (REQ-007): users holding pos.orders.view_own but none of view/change/manage see
// only orders they created, plus shared ACTIVE orders (open / pending_payment) so till
// hand-offs — a cashier settling a waiter's open bill — keep working. Full-view principals
// (and superusers/platform owners, via HasServicePermission's bypass) get no restriction.
func (h *POSOrderHandler) ownOrdersPredicate(r *http.Request) (predicate.POSOrder, bool) {
	// Shared with the All-Sales export (report_all_sales.go) so list and export scope identically.
	return ownOrdersScope(r, h.rbac, h.client)
}

// SetTerminalSecret wires the HMAC secret used to verify manager step-up tokens.
func (h *POSOrderHandler) SetTerminalSecret(s []byte) { h.terminalSecret = s }

// createOrderLineInput is a single line in the order create request body.
type createOrderLineInput struct {
	CatalogItemID uuid.UUID              `json:"catalog_item_id"`
	SKU           string                 `json:"sku"`
	Name          string                 `json:"name"`
	Category      string                 `json:"category,omitempty"` // item category name; drives KDS routing (kitchen vs bar)
	Quantity      float64                `json:"quantity"`
	UnitPrice     float64                `json:"unit_price"`
	TotalPrice    float64                `json:"total_price"`
	CourseNumber  int                    `json:"course_number"` // 0=fire immediately, 1=Starter, 2=Main, 3=Dessert
	Metadata      map[string]interface{} `json:"metadata"`
	// Per-line tax exactly as the till charged it (treasury-enriched catalog), so the server's
	// payable equals what the customer actually paid at the till.
	TaxStatus        string   `json:"tax_status,omitempty"`
	TaxCodeID        string   `json:"tax_code_id,omitempty"`
	PriceIncludesTax bool     `json:"price_includes_tax,omitempty"`
	TaxRate          *float64 `json:"tax_rate,omitempty"`
}

// lineModifierWire is the shape pos-ui sends under metadata.modifiers — one entry per
// selected modifier option, resolved to its catalog name/price at selection time (see
// terminal-context.tsx SelectedModifierDetail). Kept private to this file; orders.Service
// only knows the resolved orders.LineModifierInput shape.
type lineModifierWire struct {
	GroupID         string  `json:"group_id"`
	GroupName       string  `json:"group_name"`
	OptionID        string  `json:"option_id"`
	OptionName      string  `json:"option_name"`
	PriceAdjustment float64 `json:"price_adjustment"`
}

// parseLineModifiers decodes metadata["modifiers"] into structured LineModifierInput rows.
// Best-effort: a malformed/missing entry is skipped rather than failing the whole order —
// the price is already baked into the line's unit_price/total_price regardless, so a
// modifier that fails to parse only loses its stock-deduction/audit row, not the sale.
func parseLineModifiers(meta map[string]interface{}) []orders.LineModifierInput {
	raw, ok := meta["modifiers"]
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var wire []lineModifierWire
	if err := json.Unmarshal(encoded, &wire); err != nil {
		return nil
	}
	out := make([]orders.LineModifierInput, 0, len(wire))
	for _, w := range wire {
		optID, err := uuid.Parse(w.OptionID)
		if err != nil {
			continue
		}
		out = append(out, orders.LineModifierInput{
			GroupID:         w.GroupID,
			GroupName:       w.GroupName,
			OptionID:        optID,
			OptionName:      w.OptionName,
			PriceAdjustment: w.PriceAdjustment,
		})
	}
	return out
}

// createOrderInput is the body for POST /pos/orders.
type createOrderInput struct {
	OutletID         string                 `json:"outlet_id"`
	DeviceID         string                 `json:"device_id"`
	OrderNumber      string                 `json:"order_number"`
	ClientReference  string                 `json:"client_reference,omitempty"`   // offline local_id — idempotency anchor
	OfflineCreatedAt *time.Time             `json:"offline_created_at,omitempty"` // device-clock time the sale was rung up offline
	Currency         string                 `json:"currency"`
	Lines            []createOrderLineInput `json:"lines"`
	Metadata         map[string]interface{} `json:"metadata"`
	AgeVerified      bool                   `json:"age_verified,omitempty"`   // cashier confirmed customer age for age-restricted items
	OrderSubtype     string                 `json:"order_subtype"`            // dine_in | takeaway | room_service | delivery | bar_tab | retail
	TableID          string                 `json:"table_id"`                 // hospitality dine-in table UUID
	CustomerPhone    string                 `json:"customer_phone,omitempty"` // loyalty auto-earn
	CustomerName     string                 `json:"customer_name,omitempty"`
	// ServedByUserID lets the caller carry forward WHO SERVED the sale when it doesn't match the
	// current session (UserID) — used only by the resume-and-modify supersede flow (Add Sale),
	// which creates a NEW order for the edited draft and needs the original drafter's attribution
	// to survive instead of resetting to whoever finalizes it. Blank/invalid = fall back to UserID.
	ServedByUserID string             `json:"served_by_user_id,omitempty"`
	DiscountAmount float64            `json:"discount_amount,omitempty"`  // order-level discount (e.g. loyalty redemption)
	DiscountReason string             `json:"discount_reason,omitempty"`  // free-text reason for a manual discount
	// PromotionID identifies which Promotion produced DiscountAmount, when it came from a real
	// promo code / auto-applied deal (as opposed to a discretionary manager override, which has
	// no promotion behind it) — set by pos-ui whenever it applied a discount via
	// /pos/promotions/apply or an auto-matched deal. When present, order creation enforces the
	// promotion's usage_limit/max_units_per_customer caps (see reserveOrderPromotion) before the
	// order is created; omitted or invalid, this check is skipped entirely (unchanged behavior
	// for every order that isn't tied to a capped promotion).
	PromotionID    string             `json:"promotion_id,omitempty"`
	OrderTaxAmount float64            `json:"order_tax_amount,omitempty"` // manager quick-edit: order-level tax added on top of per-line tax
	Charges        map[string]float64 `json:"charges,omitempty"`          // manager quick-edit: additional costs (packaging/service/shipping)
	ApprovalToken  string             `json:"approval_token,omitempty"`   // manager step-up token for an over-limit discount / order adjustment
	ApprovalCode   string             `json:"approval_code,omitempty"`    // manager-generated one-time code (alternative to a live step-up token)
	Source         string             `json:"source,omitempty"`           // "pos_terminal" (default) | "back_office" (Add Sale flow)
	// BusinessDate lets admin/manager backdate a sale at entry ("YYYY-MM-DD"). Requires
	// pos.orders.manage — CreateOrder rejects the whole request (403) if a caller without that
	// permission supplies a non-empty value, rather than silently dropping their explicit input.
	BusinessDate string `json:"business_date,omitempty"`
}

// reserveOrderPromotion checks and reserves promoID's usage_limit/max_units_per_customer caps
// for this order (see promotions.Service.ReserveRedemption's doc comment for the idempotency
// contract). Returns a non-empty, customer-facing rejection message when the cap has been hit;
// empty means reserved (or the promotion has no caps configured at all — the common case).
//
// Reservation quantity is the order's TOTAL line quantity, not just the lines the promotion
// actually discounted — pos-api's order payload carries only a flat DiscountAmount with no
// per-line promotion attribution (a pre-existing, separate gap; see the flash-sale plan doc),
// so attaching a capped promotion to a bill is treated as redeeming it against everything on
// that bill. This errs conservative (the cap triggers sooner, never later), which is the safe
// direction for a "prevent overselling the deal" cap.
func (h *POSOrderHandler) reserveOrderPromotion(ctx context.Context, tid, promoID uuid.UUID, input createOrderInput, lines []orders.OrderLineInput) string {
	idempotencyKey := input.ClientReference
	if idempotencyKey == "" {
		idempotencyKey = input.OrderNumber
	}
	if idempotencyKey == "" {
		idempotencyKey = uuid.NewString()
	}
	quantity := 0.0
	for _, l := range lines {
		quantity += l.Quantity
	}
	res, err := h.promoSvc.ReserveRedemption(ctx, tid, promoID, input.CustomerPhone, promotionredemption.ChannelPos, idempotencyKey, quantity)
	if err != nil {
		h.log.Error("reserve order promotion failed", zap.Error(err))
		return "" // best-effort: a reservation-check failure never blocks an otherwise-valid sale
	}
	if res.Reserved {
		return ""
	}
	if res.Reason == "customer_limit_reached" {
		return "this discount has already been used the maximum number of times for this customer"
	}
	return "this discount has reached its usage limit"
}

// updateStatusInput is the body for PATCH /pos/orders/{id}/status.
type updateStatusInput struct {
	Status string `json:"status"`
}

// ListOrders handles GET /{tenantID}/pos/orders
// Optional query params: status, limit, offset
func (h *POSOrderHandler) ListOrders(w http.ResponseWriter, r *http.Request) {
	tenantID := httpware.GetTenantID(r.Context())
	if tenantID == "" {
		jsonError(w, "tenant_id required", http.StatusBadRequest)
		return
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	filters := []predicate.POSOrder{posorder.TenantID(tid)}

	// Per-cashier scoping (REQ-007): view_own-only principals see their own orders (+ shared
	// active bills). Enforced server-side so direct API calls can't bypass the "My Sales" view.
	if ownPred, scoped := h.ownOrdersPredicate(r); scoped {
		filters = append(filters, ownPred)
	}

	// Every user-facing filter (outlet, status, staff, invoice search, source, effective-date
	// range, customer, payment status/method, shipping, subscriptions, total range, KDS station,
	// category) is built by allSalesOrderFilters — shared verbatim with the All-Sales export
	// (report_all_sales.go AllSalesDocument) so the exported document always contains exactly
	// the rows this list shows.
	loc := tenantLocation(r.Context(), h.client, tid)
	extraFilters, paymentStatusFilter := allSalesOrderFilters(r, h.client, tid, loc)
	filters = append(filters, extraFilters...)

	p := pagination.Parse(r)
	// The heavy multi-row fetch below (potentially thousands of orders + joined lines/payments)
	// is the one part of this handler routed to a read replica when configured — see rc(). Every
	// other lookup here (filters, enrichment) stays on the primary, unchanged.
	baseQ := h.rc().POSOrder.Query().Where(filters...)

	if paymentStatusFilter == "" {
		total, _ := baseQ.Clone().Count(r.Context())
		orderList, err := baseQ.WithLines().WithPayments().Order(orderByEffectiveDate(true)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
		if err != nil {
			h.log.Error("list orders failed", zap.Error(err))
			jsonError(w, "internal error", http.StatusInternalServerError)
			return
		}
		items := h.enrichOrderList(r.Context(), tid, orderList)
		jsonOK(w, pagination.NewResponse(items, total, p))
		return
	}

	// A paid/partial/due/overdue payment-status filter depends on the server-derived settlement
	// (orders.ComputeSettlement, which nets settled sell-returns on top of paid_total) — see
	// allSalesOrderFilters' doc comment for why that can't be a precise single-order SQL predicate.
	// Scan the coarse candidate set (bounded like the export/summary) and paginate the EXACT match
	// in memory, so this page can never show a row whose badge disagrees with the requested filter.
	candidates, err := baseQ.WithLines().WithPayments().Order(orderByEffectiveDate(true)).Limit(allSalesExportCap).All(r.Context())
	if err != nil {
		h.log.Error("list orders failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	matched := make([]orderListItem, 0, len(candidates))
	for _, it := range h.enrichOrderList(r.Context(), tid, candidates) {
		if it.PaymentStatus == paymentStatusFilter {
			matched = append(matched, it)
		}
	}
	total := len(matched)
	start := p.Offset
	if start > total {
		start = total
	}
	end := start + p.Limit
	if end > total {
		end = total
	}
	jsonOK(w, pagination.NewResponse(matched[start:end], total, p))
}

// ordersSummary is the aggregate footer for the All-Sales / POS-Sales list: money-column
// totals plus payment-status and payment-method breakdowns across the WHOLE filtered set
// (every page), not just the visible page. Derived through enrichOrderList so every count
// and amount matches the per-row badges exactly.
type ordersSummary struct {
	Count         int                `json:"count"`          // rows actually aggregated (== TotalMatching unless capped)
	TotalMatching int                `json:"total_matching"` // full count of rows matching the filters
	Truncated     bool               `json:"truncated"`      // TotalMatching exceeded the aggregation cap
	ItemCount     int                `json:"item_count"`
	SumTotal      float64            `json:"sum_total"`
	SumPaid       float64            `json:"sum_paid"`
	SumDue        float64            `json:"sum_due"`
	SumReturn     float64            `json:"sum_return"`
	StatusCounts  map[string]int     `json:"status_counts"`  // paid|partial|due|overdue|refunded|voided|cancelled|draft -> n
	StatusAmounts map[string]float64 `json:"status_amounts"` // same keys -> KSh total_amount (every status, incl. voided/draft, for visibility)
	MethodCounts  map[string]int     `json:"method_counts"`  // cash|card|mpesa|...|multiple -> n (settled method only)
	MethodAmounts map[string]float64 `json:"method_amounts"` // same keys -> KSh total_amount actually settled by that method
}

// nonCommittedOrderStatus reports whether a payment-status label represents an order with NO
// real financial effect — voided/cancelled were reversed, draft was never finalized. These are
// excluded from the headline Total/Paid/Due/Items sums (which must agree with the Retail
// Overview dashboard and treasury's AR figures) but still counted in StatusCounts/StatusAmounts
// so nothing is hidden from the cashier — they just aren't double-counted as real sales.
// "refunded" is deliberately NOT included: the original sale really happened and its total/paid
// stay in the headline figures; the refunded amount is surfaced separately via Sell Return.
func nonCommittedOrderStatus(ps string) bool { return orders.NonCommittedStatus(ps) }

// OrdersSummary handles GET /{tenantID}/pos/orders/summary — the totals footer for the
// All-Sales / POS-Sales list. It applies the IDENTICAL filter set as ListOrders (shared
// allSalesOrderFilters + per-cashier scoping) but aggregates every matching row instead of
// paginating, so the footer reflects the full filtered / drilled-in dataset rather than the
// 25 rows on screen. Bounded by ordersSummaryCap (== the export cap) so the footer and the
// All-Sales export never disagree; a range past the cap is flagged, never silently under-counted.
func (h *POSOrderHandler) OrdersSummary(w http.ResponseWriter, r *http.Request) {
	tenantID := httpware.GetTenantID(r.Context())
	if tenantID == "" {
		jsonError(w, "tenant_id required", http.StatusBadRequest)
		return
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	// Same predicates as ListOrders: tenant + per-cashier "My Sales" scoping + every
	// user-facing filter (shared allSalesOrderFilters), so totals track the list one-to-one.
	filters := []predicate.POSOrder{posorder.TenantID(tid)}
	if ownPred, scoped := h.ownOrdersPredicate(r); scoped {
		filters = append(filters, ownPred)
	}
	loc := tenantLocation(r.Context(), h.client, tid)
	extraFilters, paymentStatusFilter := allSalesOrderFilters(r, h.client, tid, loc)
	filters = append(filters, extraFilters...)

	baseQ := h.client.POSOrder.Query().Where(filters...)
	totalMatching, _ := baseQ.Clone().Count(r.Context())
	list, err := baseQ.WithLines().WithPayments().
		Order(orderByEffectiveDate(true)).
		Limit(allSalesExportCap).
		All(r.Context())
	if err != nil {
		h.log.Error("orders summary query failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	items := h.enrichOrderList(r.Context(), tid, list)
	truncated := totalMatching > len(list)
	// paid/partial/due/overdue depend on the server-derived settlement (sell-returns netted in,
	// per orders.ComputeSettlement) — exact-match it here rather than trusting the coarse SQL
	// predicate allSalesOrderFilters applied, for the same reason ListOrders does (see its comment).
	if paymentStatusFilter != "" {
		filtered := make([]orderListItem, 0, len(items))
		for _, it := range items {
			if it.PaymentStatus == paymentStatusFilter {
				filtered = append(filtered, it)
			}
		}
		items = filtered
		if !truncated {
			totalMatching = len(items)
		}
	}
	sum := ordersSummary{
		Count:         len(items),
		TotalMatching: totalMatching,
		Truncated:     truncated,
		StatusCounts:  map[string]int{},
		StatusAmounts: map[string]float64{},
		MethodCounts:  map[string]int{},
		MethodAmounts: map[string]float64{},
	}
	for _, it := range items {
		if it.PaymentStatus != "" {
			sum.StatusCounts[it.PaymentStatus]++
			sum.StatusAmounts[it.PaymentStatus] += it.TotalAmount
		}
		// Only settled sales contribute a method; unpaid/due sales have no tender yet and are
		// captured by the status breakdown instead (so the two breakdowns need not sum equal).
		if it.PaymentMethod != "" {
			sum.MethodCounts[it.PaymentMethod]++
			sum.MethodAmounts[it.PaymentMethod] += it.TotalAmount
		}
		// Voided/cancelled/draft rows stay visible above (status counts/amounts) but never
		// contribute to the headline Total/Paid/Due/Items figures — those must reconcile with
		// the Retail Overview dashboard (which only counts status=="completed") and with
		// treasury's AR outstanding figure, neither of which recognizes a reversed or
		// not-yet-finalized order as a real sale.
		if nonCommittedOrderStatus(it.PaymentStatus) {
			continue
		}
		sum.ItemCount += it.ItemCount
		sum.SumTotal += it.TotalAmount
		sum.SumPaid += it.TotalPaid
		sum.SumDue += it.AmountDue
		sum.SumReturn += it.ReturnTotal
	}
	jsonOK(w, sum)
}

// orderListItem wraps a POSOrder with the derived columns the All-Sales table needs
// (payment status/method, paid vs due, item count). The embedded *ent.POSOrder promotes
// all original fields + edges, so existing consumers (terminal, drafts) are unaffected.
type orderListItem struct {
	*ent.POSOrder
	ItemCount     int     `json:"item_count"`
	TotalPaid     float64 `json:"total_paid"`
	AmountDue     float64 `json:"amount_due"`
	PaymentStatus string  `json:"payment_status"` // paid | partial | due | overdue | refunded | voided | cancelled
	PaymentMethod string  `json:"payment_method"` // dominant tender type, or "multiple"
	// CashierName is the human display name of the staff member who CREATED the order row
	// (resolved from user_id via the shared resolveStaffNames helper — same mapping the
	// staff reports use). Empty when the user projection has no name for the id.
	CashierName string `json:"cashier_name"`
	// ServedByName is who is CREDITED with serving the sale (resolved from served_by_user_id,
	// falling back to user_id for pre-existing rows with no served_by_user_id set) — the field
	// that survives a resumed-and-modified draft's supersede-to-a-new-order and the one admins
	// correct via the sale-info edit tool. Prefer this over CashierName everywhere a "Cashier"/
	// "Served by" column is shown; CashierName remains available as the strict row-creator fact.
	ServedByName string `json:"served_by_name"`
	// Sell-return rollup (rejected returns excluded): lets the list flag returned
	// sales (red return arrow) and show the returned amount without N+1 lookups.
	ReturnCount  int     `json:"return_count"`
	ReturnTotal  float64 `json:"return_total"`
	ReturnStatus string  `json:"return_status,omitempty"` // pending | approved | completed (most-advanced across the order's returns)
	// Profit/MarginPct are this order's gross profit, computed via the SAME AttributeOrderLines +
	// resolveUnitCostsBySKU + LineProfit machinery every other profitability report uses (report_
	// attribution.go) — the Profitability page's Invoice tab is this same sales list with these two
	// columns added, not a separate fetch, so it can never disagree with the item/category/etc.
	// rollups for the same date range.
	Profit    float64 `json:"profit"`
	MarginPct float64 `json:"margin_pct"`
	// HasCorrectionHistory mirrors orders.HasCorrectionHistory (any return/refund/reversal on
	// record, any status) — lets pos-ui disable/relabel the Delete Sale action for a row it
	// already knows will be refused, instead of only learning after a confusing 422 whose
	// confirmation dialog just promised unconditional deletion.
	HasCorrectionHistory bool `json:"has_correction_history"`
	// HasFullReversal mirrors orders.HasFullReversal (a FULL-scope reversal already exists) —
	// the narrower, terminal condition saleedit's Edit orchestrator actually refuses on. Unlike
	// HasCorrectionHistory (any correction at all, used by Delete Sale), a PARTIAL correction
	// does NOT set this — Edit Sale explicitly still allows repeat partial corrections. Lets
	// pos-ui disable Edit Sale only for a row that would genuinely be refused.
	HasFullReversal bool `json:"has_full_reversal"`
}

// returnAgg is the per-order sell-return rollup. completedTotal is the subset of total that has
// actually SETTLED (money/AR already moved — see the 3-stage lifecycle initiate→approve→complete);
// only that portion may net down AmountDue. A pending/approved return hasn't settled anything yet.
type returnAgg struct {
	count          int
	total          float64
	status         string
	completedTotal float64
}

// arReducingRefundChannel reports whether a return's refund channel settles straight into
// treasury's CustomerBalance (offset_invoice) — the channel offered for on-account sales by
// default (see returns_policy.go's defaultRefundChannel). Only these channels get eventually
// folded into an order's own paid_total by payments/ar_reconcile.go's event-driven, reduce-only
// ReconcileCustomerOrders — everything else (cash/mpesa/bank/cheque/store_credit) never touches
// treasury AR and so never reaches paid_total any other way.
func arReducingRefundChannel(ch *posreturn.RefundChannel) bool {
	return ch != nil && *ch == posreturn.RefundChannelOffsetInvoice
}

// returnsRollup batch-loads the sell-return rollup for a set of orders (rejected returns excluded —
// a rejected request never happened financially). One query, no N+1. Shared by the list enrichment
// and the single-order (Sell Details) enrichment so both net completed returns identically.
func (h *POSOrderHandler) returnsRollup(ctx context.Context, tenantID uuid.UUID, orderIDs []uuid.UUID) map[uuid.UUID]*returnAgg {
	return returnsRollupFor(ctx, h.client, h.log, tenantID, orderIDs)
}

// returnsRollupFor is the package-level implementation so BOTH the POSOrderHandler (list/detail) and
// the ReportPDFHandler (CSV/PDF export) net completed returns from ONE code path — the export can
// never disagree with the on-screen list again (the report_all_sales "kept in sync" comment that
// wasn't).
//
// completedTotal deliberately EXCLUDES offset_invoice-channel returns (see arReducingRefundChannel)
// — every caller feeds this straight into orders.ComputeSettlement's completedReturns parameter,
// and that channel's value is already folded into the order's own paid_total by
// payments/ar_reconcile.go once its event lands, so including it here too would double-subtract
// the same return. Confirmed live 2026-08-06 (see settlement.go's doc comment for the exact
// numbers). `total`/`count`/`status` stay unfiltered — they're pure display fields
// (ReturnTotal/ReturnCount/ReturnStatus), never fed into the owed-amount math.
func returnsRollupFor(ctx context.Context, client *ent.Client, log *zap.Logger, tenantID uuid.UUID, orderIDs []uuid.UUID) map[uuid.UUID]*returnAgg {
	returnsByOrder := map[uuid.UUID]*returnAgg{}
	if len(orderIDs) == 0 {
		return returnsByOrder
	}
	returnRank := map[posreturn.Status]int{posreturn.StatusPending: 1, posreturn.StatusApproved: 2, posreturn.StatusCompleted: 3}
	rets, err := client.POSReturn.Query().
		Where(
			posreturn.TenantID(tenantID),
			posreturn.OrderIDIn(orderIDs...),
			posreturn.StatusNEQ(posreturn.StatusRejected),
		).
		All(ctx)
	if err != nil {
		log.Warn("order return rollup failed", zap.Error(err))
		return returnsByOrder
	}
	for _, ret := range rets {
		agg := returnsByOrder[ret.OrderID]
		if agg == nil {
			agg = &returnAgg{}
			returnsByOrder[ret.OrderID] = agg
		}
		agg.count++
		agg.total += ret.RefundAmount
		if ret.Status == posreturn.StatusCompleted && !arReducingRefundChannel(ret.RefundChannel) {
			agg.completedTotal += ret.RefundAmount
		}
		if returnRank[ret.Status] > returnRank[posreturn.Status(agg.status)] {
			agg.status = string(ret.Status)
		}
	}
	return returnsByOrder
}

// servedByName resolves the display name credited with SERVING an order — served_by_user_id when
// set, falling back to user_id for rows created before the field existed (nil served_by_user_id).
func servedByName(o *ent.POSOrder, staffNames map[uuid.UUID]string) string {
	if o.ServedByUserID != nil {
		if name := staffNames[*o.ServedByUserID]; name != "" {
			return name
		}
	}
	return staffNames[o.UserID]
}

// enrichSingleOrder wraps ONE order in the same orderListItem shape the All-Sales list returns —
// so the Sell Details modal (and any single-order consumer) reads the SERVER's canonical amount_due/
// payment_status/return_total instead of re-deriving them client-side (the root of the "footer says
// 8,000, list says 4,000" divergence). Delegates owed math to orders.ComputeSettlement.
func (h *POSOrderHandler) enrichSingleOrder(ctx context.Context, tenantID uuid.UUID, order *ent.POSOrder) orderListItem {
	retAgg := h.returnsRollup(ctx, tenantID, []uuid.UUID{order.ID})[order.ID]
	uids := []uuid.UUID{order.UserID}
	if order.ServedByUserID != nil {
		uids = append(uids, *order.ServedByUserID)
	}
	staffNames := resolveStaffNames(ctx, h.client, h.log, tenantID, uids)
	var completedReturns float64
	methods := map[string]struct{}{}
	for _, pay := range order.Edges.Payments {
		if !strings.EqualFold(pay.Status, "completed") {
			continue
		}
		m := ""
		if pay.PaymentData != nil {
			m, _ = pay.PaymentData["method"].(string)
		}
		if m != "" {
			methods[m] = struct{}{}
		}
	}
	if retAgg != nil {
		completedReturns = retAgg.completedTotal
	}
	st := orders.ComputeSettlement(order, completedReturns)
	correctionHistory := orders.CorrectionHistoryRollup(ctx, h.client, tenantID, []uuid.UUID{order.ID})
	fullReversal := orders.FullReversalRollup(ctx, h.client, tenantID, []uuid.UUID{order.ID})
	costBySKU := resolveUnitCostsBySKU(ctx, h.client, tenantID, collectOrderSKUs([]*ent.POSOrder{order}))
	profit, marginPct := computeOrderProfit(order, costBySKU)
	item := orderListItem{
		POSOrder:             order,
		ItemCount:            len(order.Edges.Lines),
		TotalPaid:            st.Collected,
		AmountDue:            st.AmountDue,
		PaymentStatus:        st.PaymentStatus,
		PaymentMethod:        dominantMethod(methods),
		CashierName:          staffNames[order.UserID],
		ServedByName:         servedByName(order, staffNames),
		HasCorrectionHistory: correctionHistory[order.ID],
		HasFullReversal:      fullReversal[order.ID],
		Profit:               profit,
		MarginPct:            marginPct,
	}
	if retAgg != nil {
		item.ReturnCount = retAgg.count
		item.ReturnTotal = retAgg.total
		item.ReturnStatus = retAgg.status
	}
	return item
}

// computeOrderProfit sums this order's LineProfit contribution across its attributed lines — the
// SAME per-line Revenue/Tax/cost math MostProfitableItems/GetSummary/SalesByHour use, so a
// per-order profit figure surfaced on the sales list can never disagree with the item/category/
// day/etc. rollups for the same date range. costBySKU is a pre-resolved batch (see
// resolveUnitCostsBySKU) — callers with more than one order MUST batch-resolve it once, not per row.
func computeOrderProfit(o *ent.POSOrder, costBySKU map[string]float64) (profit, marginPct float64) {
	var netRevenue float64
	for _, al := range AttributeOrderLines(o) {
		lineNetRevenue := al.Revenue - al.Tax
		netRevenue += lineNetRevenue
		profit += lineNetRevenue - costBySKU[al.SKU]*al.Quantity
	}
	if netRevenue != 0 {
		marginPct = profit / netRevenue * 100
	}
	return profit, marginPct
}

// enrichOrderList computes the derived display columns for a page of orders, resolving
// payment-method labels via a single batched Tender lookup (id → type) and cashier display
// names via a single batched User lookup (shared resolveStaffNames helper).
func (h *POSOrderHandler) enrichOrderList(ctx context.Context, tenantID uuid.UUID, list []*ent.POSOrder) []orderListItem {
	costBySKU := resolveUnitCostsBySKU(ctx, h.client, tenantID, collectOrderSKUs(list))
	// Batch-resolve cashier + served-by names so every list row (drafts, recent transactions, all
	// sales) shows WHO rang the sale up / who served it without an N+1 user lookup.
	uidSet := map[uuid.UUID]struct{}{}
	for _, o := range list {
		uidSet[o.UserID] = struct{}{}
		if o.ServedByUserID != nil {
			uidSet[*o.ServedByUserID] = struct{}{}
		}
	}
	uids := make([]uuid.UUID, 0, len(uidSet))
	for id := range uidSet {
		uids = append(uids, id)
	}
	staffNames := resolveStaffNames(ctx, h.client, h.log, tenantID, uids)

	// Collect every tender_id referenced by this page's payments, then resolve types once.
	tenderIDSet := map[uuid.UUID]struct{}{}
	for _, o := range list {
		for _, pay := range o.Edges.Payments {
			tenderIDSet[pay.TenderID] = struct{}{}
		}
	}
	tenderType := map[uuid.UUID]string{}
	if len(tenderIDSet) > 0 {
		ids := make([]uuid.UUID, 0, len(tenderIDSet))
		for id := range tenderIDSet {
			ids = append(ids, id)
		}
		if tenders, err := h.client.Tender.Query().Where(tender.IDIn(ids...)).All(ctx); err == nil {
			for _, t := range tenders {
				tenderType[t.ID] = t.Type
			}
		}
	}

	// Batched sell-return rollup: one query for the whole page (mirrors the tender batch above).
	orderIDs := make([]uuid.UUID, 0, len(list))
	for _, o := range list {
		orderIDs = append(orderIDs, o.ID)
	}
	returnsByOrder := h.returnsRollup(ctx, tenantID, orderIDs)
	correctionHistory := orders.CorrectionHistoryRollup(ctx, h.client, tenantID, orderIDs)
	fullReversal := orders.FullReversalRollup(ctx, h.client, tenantID, orderIDs)

	items := make([]orderListItem, 0, len(list))
	for _, o := range list {
		retAgg := returnsByOrder[o.ID]
		// The displayed method comes from what was actually SETTLED. The terminal stamps the real
		// method on payment_data.method (the tender is a shared generic row, so its type is not the
		// method); fall back to the tender type only for legacy per-method-tender setups.
		methods := map[string]struct{}{}
		for _, pay := range o.Edges.Payments {
			if !strings.EqualFold(pay.Status, "completed") {
				continue // only settled tenders define the method shown against a paid sale
			}
			m := ""
			if pay.PaymentData != nil {
				m, _ = pay.PaymentData["method"].(string)
			}
			if m == "" {
				m = tenderType[pay.TenderID]
			}
			if m != "" {
				methods[m] = struct{}{}
			}
		}
		// Owed-state is derived ONCE, by the canonical orders.ComputeSettlement choke point, so
		// this list, the Sell Details endpoint, the CSV/register/P&L exports and the treasury→POS
		// reconciler can never disagree about the same sale. It nets a COMPLETED return (refund/
		// credit-note/offset-invoice already settled — see the 3-stage lifecycle) out of what is
		// still owed; pending/approved-but-not-yet-completed returns are excluded (nothing settled
		// yet). Badge upgrades to "overdue" past the stamped payment_due_date, all inside the helper.
		var completedReturns float64
		if retAgg != nil {
			completedReturns = retAgg.completedTotal
		}
		st := orders.ComputeSettlement(o, completedReturns)
		profit, marginPct := computeOrderProfit(o, costBySKU)
		item := orderListItem{
			POSOrder:             o,
			ItemCount:            len(o.Edges.Lines),
			TotalPaid:            st.Collected,
			AmountDue:            st.AmountDue,
			PaymentStatus:        st.PaymentStatus,
			PaymentMethod:        dominantMethod(methods),
			CashierName:          staffNames[o.UserID], // "" when unknown — the UI falls back gracefully
			ServedByName:         servedByName(o, staffNames),
			HasCorrectionHistory: correctionHistory[o.ID],
			HasFullReversal:      fullReversal[o.ID],
			Profit:               profit,
			MarginPct:            marginPct,
		}
		if agg := retAgg; agg != nil {
			item.ReturnCount = agg.count
			item.ReturnTotal = agg.total
			item.ReturnStatus = agg.status
		}
		items = append(items, item)
	}
	return items
}

// isOrderOverdue / isOnAccount / derivePaymentStatus delegate to the canonical
// orders.ComputeSettlement choke point (internal/modules/orders/settlement.go) so the list, the
// exports and the reconciler share ONE owed-amount definition. Kept as thin package-local adapters
// only because the SQL predicate helpers above and the report handlers already reference these names.
func isOrderOverdue(meta map[string]any) bool { return orders.IsOrderOverdue(meta) }

func isOnAccount(meta map[string]any) bool { return orders.IsOnAccount(meta) }

func derivePaymentStatus(status string, total, paid, completedReturns float64, onAccount bool) string {
	return orders.DerivePaymentStatus(status, total, paid, completedReturns, onAccount)
}

// dominantMethod returns the single tender type used, "multiple" if several, "" if none.
func dominantMethod(methods map[string]struct{}) string {
	switch len(methods) {
	case 0:
		return ""
	case 1:
		for m := range methods {
			return m
		}
	}
	return "multiple"
}

// parseDateParam parses a from/to query value as RFC3339 or YYYY-MM-DD. Date-only
// values are interpreted in loc (the tenant timezone) so the day boundary matches
// the tenant's wall clock; when endOfDay is true a date-only value snaps to the end
// of that local day (inclusive range).
func parseDateParam(v string, endOfDay bool, loc *time.Location) *time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	if loc == nil {
		loc = time.UTC
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return &t
	}
	// datetime-local (HTML <input type="datetime-local">) — carries a wall-clock time but no
	// zone, so interpret it in the outlet/tenant timezone. Seconds are optional. When a time is
	// present we DON'T snap `to` to end-of-day (the caller asked for a precise minute boundary).
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, v, loc); err == nil {
			return &t
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", v, loc); err == nil {
		if endOfDay {
			t = t.Add(24*time.Hour - time.Nanosecond)
		}
		return &t
	}
	return nil
}

// GetOrder handles GET /{tenantID}/pos/orders/{orderID}
func (h *POSOrderHandler) GetOrder(w http.ResponseWriter, r *http.Request) {
	tenantID := httpware.GetTenantID(r.Context())
	if tenantID == "" {
		jsonError(w, "tenant_id required", http.StatusBadRequest)
		return
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}

	// Tenant (+ own-visibility) is the security boundary for a single-order read. The outlet
	// context is deliberately NOT applied here: the All-Sales list spans outlets
	// (outlet_id=all), and scoping this read to the caller's header/HQ outlet made every
	// cross-outlet row 404 when opened (Sell Details / Return-by-Invoice defect).
	whereArgs := []predicate.POSOrder{posorder.ID(orderID), posorder.TenantID(tid)}
	if ownPred, scoped := h.ownOrdersPredicate(r); scoped {
		whereArgs = append(whereArgs, ownPred)
	}
	order, err := h.client.POSOrder.Query().
		Where(whereArgs...).
		WithLines(func(q *ent.POSOrderLineQuery) { q.WithModifiers() }).
		WithPayments().
		WithEvents().
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		h.log.Error("get order failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Return the enriched item (canonical amount_due/payment_status/return_total) — NOT the raw
	// entity — so the Sell Details modal reads the SAME server-computed owed figure as the All-Sales
	// list. The embedded *ent.POSOrder still promotes every original field + edge, so existing
	// consumers (terminal, drafts) that read total_amount/edges.lines/edges.payments are unaffected.
	jsonOK(w, h.enrichSingleOrder(r.Context(), tid, order))
}

// GetOrderByNumber handles GET /{tenantID}/pos/orders/by-number/{orderNumber} — used by the POS
// "Return by Invoice" flow to look up a prior sale by its order/invoice number.
func (h *POSOrderHandler) GetOrderByNumber(w http.ResponseWriter, r *http.Request) {
	tenantID := httpware.GetTenantID(r.Context())
	if tenantID == "" {
		jsonError(w, "tenant_id required", http.StatusBadRequest)
		return
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	orderNumber := chi.URLParam(r, "orderNumber")
	if orderNumber == "" {
		jsonError(w, "order_number required", http.StatusBadRequest)
		return
	}

	// Tenant-scoped only — no outlet predicate (a receipt from any outlet must resolve; see
	// GetOrder) and no view_own narrowing: Return-by-Invoice legitimately looks up sales rung
	// up by OTHER cashiers (a customer returns goods to whoever is at the till). Knowing the
	// exact receipt number is the lookup credential here.
	whereArgs := []predicate.POSOrder{posorder.OrderNumber(orderNumber), posorder.TenantID(tid)}
	order, err := h.client.POSOrder.Query().
		Where(whereArgs...).
		WithLines(func(q *ent.POSOrderLineQuery) { q.WithModifiers() }).
		WithPayments().
		WithEvents().
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		h.log.Error("get order by number failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Enriched item (see GetOrder) so Return-by-Invoice / Sell Details read the canonical amount_due.
	jsonOK(w, h.enrichSingleOrder(r.Context(), tid, order))
}

// CreateOrder handles POST /{tenantID}/pos/orders
func (h *POSOrderHandler) CreateOrder(w http.ResponseWriter, r *http.Request) {
	tenantID := httpware.GetTenantID(r.Context())
	if tenantID == "" {
		jsonError(w, "tenant_id required", http.StatusBadRequest)
		return
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	// Subscription enforcement: skip for platform owners, block expired tenants.
	if !httpware.IsPlatformOwner(r.Context()) && h.subsClient != nil {
		bearerToken := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		tenantSlug := ""
		if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
			tenantSlug = claims.GetTenantSlug()
		}
		if !h.subsClient.IsSubscriptionActive(r.Context(), tenantID, tenantSlug, bearerToken) {
			jsonError(w, "active subscription required", http.StatusPaymentRequired)
			return
		}

		// Metered limit: count this sale against max_orders_per_day. Over limit with no
		// overage opt-in → 402 with the structured limit body so pos-ui opens the extra-usage
		// modal. Exempt tokens and infra errors fail open (ReportUsage returns Allowed=true).
		exempt := false
		if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
			exempt = claims.IsGatingExempt()
		}
		if !exempt {
			if dec := h.subsClient.ReportUsage(r.Context(), tenantID, subscriptions.MetricOrders, "pos-api", 1); !dec.Allowed {
				status := dec.Status
				if status == 0 {
					status = http.StatusPaymentRequired
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(dec.Body)
				return
			}
		}
	}

	var input createOrderInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Backdate-at-entry (admin/manager only) — checked here, before any other work, so a caller
	// without pos.orders.manage gets a clear 403 rather than having their explicit input silently
	// dropped further down the pipeline.
	if strings.TrimSpace(input.BusinessDate) != "" && !outletmw.HasServicePermission(r, h.rbac, "pos.orders.manage") {
		jsonError(w, "only admins/managers may backdate a sale", http.StatusForbidden)
		return
	}

	// Get user ID from auth claims
	var userID uuid.UUID
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims.Subject != "" {
		userID, _ = uuid.Parse(claims.Subject)
	}

	// Age-restricted items: block unless the cashier has confirmed the customer's
	// age. Enforced server-side (defence in depth — the client age prompt can be
	// bypassed).
	if !input.AgeVerified {
		skus := make([]string, 0, len(input.Lines))
		for _, l := range input.Lines {
			if l.SKU != "" {
				skus = append(skus, l.SKU)
			}
		}
		if len(skus) > 0 {
			ageOverrides, _ := h.client.POSCatalogOverride.Query().
				Where(
					entoverride.TenantID(tid),
					entoverride.InventorySkuIn(skus...),
					entoverride.MinimumAgeGT(0),
				).All(r.Context())
			if len(ageOverrides) > 0 {
				blocked := make([]string, 0, len(ageOverrides))
				for _, o := range ageOverrides {
					blocked = append(blocked, o.InventorySku)
				}
				jsonError(w, "age verification required for: "+strings.Join(blocked, ", "), http.StatusUnprocessableEntity)
				return
			}
		}
	}

	// Parse optional UUID fields — fall back to zero UUID if missing/invalid.
	outletID, _ := uuid.Parse(input.OutletID)
	deviceID, _ := uuid.Parse(input.DeviceID)

	// If outlet_id not in body, try the X-Outlet-ID header set by pos-ui.
	if outletID == uuid.Nil {
		if hv := r.Header.Get("X-Outlet-ID"); hv != "" {
			outletID, _ = uuid.Parse(hv)
		}
	}

	// Resolve the outlet's discount/override limits + pricing policy + whether the caller
	// may bypass them (managers/admins), used by the discount and price-override gates.
	maxPct := 100.0
	maxAmount := 0.0                 // 0 = no absolute-amount limit
	requireApprovalBelowBase := true // selling below base needs a manager step-up
	if outletID != uuid.Nil {
		if s, sErr := h.client.OutletSetting.Query().Where(outletsetting.OutletID(outletID)).Only(r.Context()); sErr == nil {
			maxPct = s.MaxDiscountPercent
			maxAmount = s.MaxDiscountAmount
			requireApprovalBelowBase = s.RequireApprovalBelowBase
		}
	}
	callerIsManager := overrideRoles[requesterRole(r)]
	if !callerIsManager {
		if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims != nil {
			callerIsManager = claims.IsPlatformOwner || hasOverrideRole(claims.Roles)
		}
	}

	// Manual-discount gate: a discount above max_discount_percent OR above the absolute
	// max_discount_amount (when configured) requires a manager step-up; over-limit
	// discounts are recorded as order.discount_override.
	if input.DiscountAmount > 0 && !callerIsManager {
		var subtotal float64
		for _, l := range input.Lines {
			subtotal += l.TotalPrice
		}
		discountPct := 0.0
		if subtotal > 0 {
			discountPct = input.DiscountAmount / subtotal * 100
		}
		overAmount := maxAmount > 0 && input.DiscountAmount > maxAmount+0.001
		if discountPct > maxPct+0.001 || overAmount {
			approverID, valid := uuid.Nil, false
			if input.ApprovalToken != "" && len(h.terminalSecret) > 0 {
				approverID, valid = verifyApprovalToken(input.ApprovalToken, "order.discount_override", h.terminalSecret)
			}
			if !valid && input.ApprovalCode != "" {
				approverID, valid = redeemActionApprovalCode(r.Context(), h.client, h.log, tid, outletID, "order.discount_override", input.ApprovalCode)
			}
			if !valid {
				respondJSON(w, http.StatusUnprocessableEntity, map[string]any{
					"error":             "manager approval required: discount exceeds the allowed limit",
					"approval_required": true, "action": "order.discount_override",
				})
				return
			}
			if h.auditSvc != nil {
				oid := outletID
				amt := input.DiscountAmount
				h.auditSvc.Record(r.Context(), audit.Entry{
					TenantID: tid, OutletID: &oid, ActorUserID: userID, ApproverID: &approverID,
					Action: "order.discount_override", EntityType: "pos_order", Reason: input.DiscountReason, Amount: &amt,
					After: map[string]any{"discount_percent": discountPct, "max_percent": maxPct, "max_amount": maxAmount},
				})
			}
		}
	}

	// Order-adjustment gate: order-level tax edits and additional charges (packaging/service/
	// shipping) are a manager/admin quick-edit. Non-managers need a manager step-up token
	// (order.adjustment), mirroring the discount gate; adjustments are audited.
	chargesSum := 0.0
	for _, v := range input.Charges {
		if v > 0 {
			chargesSum += v
		}
	}
	if (input.OrderTaxAmount > 0 || chargesSum > 0) && !callerIsManager {
		approverID, valid := uuid.Nil, false
		if input.ApprovalToken != "" && len(h.terminalSecret) > 0 {
			approverID, valid = verifyApprovalToken(input.ApprovalToken, "order.adjustment", h.terminalSecret)
		}
		if !valid && input.ApprovalCode != "" {
			approverID, valid = redeemActionApprovalCode(r.Context(), h.client, h.log, tid, outletID, "order.adjustment", input.ApprovalCode)
		}
		if !valid {
			respondJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":             "manager approval required: order tax / additional charges are a manager adjustment",
				"approval_required": true, "action": "order.adjustment",
			})
			return
		}
		if h.auditSvc != nil {
			oid := outletID
			amt := input.OrderTaxAmount + chargesSum
			h.auditSvc.Record(r.Context(), audit.Entry{
				TenantID: tid, OutletID: &oid, ActorUserID: userID, ApproverID: &approverID,
				Action: "order.adjustment", EntityType: "pos_order", Amount: &amt,
				After: map[string]any{"order_tax_amount": input.OrderTaxAmount, "charges": input.Charges},
			})
		}
	}

	// Per-line price-override gate, driven by the outlet's PRICING POLICY:
	//  - below base (metadata.original_price): needs a manager step-up while
	//    require_approval_below_base is ON (the default) — cashiers never markdown on
	//    their own authority; toggling it OFF allows free markdowns.
	//  - above base: ALWAYS allowed, no approval, ever (2026-07-27 policy: a markup is
	//    never a loss to the business, so it never needs a step-up — only markdowns and
	//    over-limit discounts do). allow_price_above_base only controls whether the pos-ui
	//    price-edit control is even shown to a non-manager; it is not an approval gate.
	// The outlet's max_discount_percent governs the separate ORDER-level discount gate
	// above, not per-line edits. The gate keys off original_price alone — a client
	// "forgetting" the price_override flag no longer bypasses it.
	if !callerIsManager {
		needApproval := false
		type ovLine struct {
			sku        string
			orig, unit float64
			dev        float64
		}
		var overrides []ovLine
		for _, l := range input.Lines {
			// Non-billable lines are zeroed server-side by design — a zero price is not a markdown.
			if metaBool(l.Metadata, "non_billable") {
				continue
			}
			orig := readFloatMeta(l.Metadata, "original_price")
			if orig <= 0 {
				continue
			}
			below := l.UnitPrice < orig-0.005
			if below && requireApprovalBelowBase {
				dev := (orig - l.UnitPrice) / orig * 100
				overrides = append(overrides, ovLine{sku: l.SKU, orig: orig, unit: l.UnitPrice, dev: dev})
				needApproval = true
			}
		}
		if needApproval {
			approverID, valid := uuid.Nil, false
			if input.ApprovalToken != "" && len(h.terminalSecret) > 0 {
				approverID, valid = verifyApprovalToken(input.ApprovalToken, "price.override", h.terminalSecret)
			}
			if !valid && input.ApprovalCode != "" {
				approverID, valid = redeemActionApprovalCode(r.Context(), h.client, h.log, tid, outletID, "price.override", input.ApprovalCode)
			}
			if !valid {
				respondJSON(w, http.StatusUnprocessableEntity, map[string]any{
					"error":             "manager approval required: selling below the preset price needs a manager",
					"approval_required": true, "action": "price.override",
				})
				return
			}
			if h.auditSvc != nil {
				oid := outletID
				for _, o := range overrides {
					amt := o.orig - o.unit
					h.auditSvc.Record(r.Context(), audit.Entry{
						TenantID: tid, OutletID: &oid, ActorUserID: userID, ApproverID: &approverID,
						Action: "price.override", EntityType: "pos_order_line", EntityID: o.sku, Amount: &amt,
						After: map[string]any{"original_price": o.orig, "new_price": o.unit, "deviation_percent": o.dev, "max_percent": maxPct},
					})
				}
			}
		}
	}

	// Min selling-price hard guardrail: a line priced BELOW the item's configured floor
	// (carried on the catalog item, echoed by the till in line metadata as min_price) is
	// blocked unless a manager approves it (price.override). This is absolute — independent
	// of the discount-percent gate above. Managers bypass (override authority).
	//
	// 2026-07-27 policy fix: this used to ALSO gate the ceiling (max_price) the exact same
	// way — a markup got the identical approval_required 422 as a markdown. But nothing in
	// pos-ui's inline/GoDigital payment bar (createOrderAsync, used by every tender incl.
	// Cash) actually handled that 422: it only showed a toast and left the cashier stuck at
	// the "Confirm Cash" screen with no ApprovalDialog and no way to proceed (the
	// approval-retry wiring only ever existed on the OLDER handlePlaceOrder path). Rather
	// than also wire the ceiling case into the new flow, the ceiling check is removed
	// entirely per product decision: a markup is never a loss to the business and must
	// never require a manager step-up — only markdowns (below min_price) and over-limit
	// discounts do. max_price / max_selling_price no longer gates anything server-side.
	if !callerIsManager {
		type bandLine struct {
			sku   string
			price float64
			min   float64
		}
		var underMin []bandLine
		for _, l := range input.Lines {
			min := readFloatMeta(l.Metadata, "min_price")
			if min > 0 && l.UnitPrice < min {
				underMin = append(underMin, bandLine{sku: l.SKU, price: l.UnitPrice, min: min})
			}
		}
		if len(underMin) > 0 {
			approverID, valid := uuid.Nil, false
			if input.ApprovalToken != "" && len(h.terminalSecret) > 0 {
				approverID, valid = verifyApprovalToken(input.ApprovalToken, "price.override", h.terminalSecret)
			}
			if !valid && input.ApprovalCode != "" {
				approverID, valid = redeemActionApprovalCode(r.Context(), h.client, h.log, tid, outletID, "price.override", input.ApprovalCode)
			}
			if !valid {
				respondJSON(w, http.StatusUnprocessableEntity, map[string]any{
					"error":             "manager approval required: a line price is below the allowed minimum",
					"approval_required": true, "action": "price.override",
				})
				return
			}
			if h.auditSvc != nil {
				oid := outletID
				for _, o := range underMin {
					price := o.price
					h.auditSvc.Record(r.Context(), audit.Entry{
						TenantID: tid, OutletID: &oid, ActorUserID: userID, ApproverID: &approverID,
						Action: "price.override", EntityType: "pos_order_line", EntityID: o.sku, Reason: "out_of_band", Amount: &price,
						After: map[string]any{"unit_price": o.price, "min_price": o.min},
					})
				}
			}
		}
	}

	// Convert handler input to service request
	lines := make([]orders.OrderLineInput, len(input.Lines))
	for i, l := range input.Lines {
		lines[i] = orders.OrderLineInput{
			CatalogItemID:    l.CatalogItemID,
			SKU:              l.SKU,
			Name:             l.Name,
			Category:         l.Category,
			Quantity:         l.Quantity,
			UnitPrice:        l.UnitPrice,
			TotalPrice:       l.TotalPrice,
			CourseNumber:     l.CourseNumber,
			Metadata:         l.Metadata,
			Modifiers:        parseLineModifiers(l.Metadata),
			TaxStatus:        l.TaxStatus,
			TaxCodeID:        l.TaxCodeID,
			PriceIncludesTax: l.PriceIncludesTax,
			TaxRate:          l.TaxRate,
		}
	}

	servedByUserID, _ := uuid.Parse(input.ServedByUserID) // zero value on blank/invalid = fall back to userID

	if promoID, perr := uuid.Parse(input.PromotionID); perr == nil && h.promoSvc != nil {
		if rejectReason := h.reserveOrderPromotion(r.Context(), tid, promoID, input, lines); rejectReason != "" {
			respondJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error": rejectReason, "code": "promotion_limit_reached",
			})
			return
		}
	}

	order, err := h.orderSvc.CreateOrder(r.Context(), orders.CreateOrderRequest{
		TenantID:         tid,
		OutletID:         outletID,
		DeviceID:         deviceID,
		UserID:           userID,
		ServedByUserID:   servedByUserID,
		OrderNumber:      input.OrderNumber,
		ClientReference:  input.ClientReference,
		OfflineCreatedAt: input.OfflineCreatedAt,
		Currency:         input.Currency,
		Lines:            lines,
		Metadata:         input.Metadata,
		OrderSubtype:     input.OrderSubtype,
		TableID:          input.TableID,
		CustomerPhone:    input.CustomerPhone,
		CustomerName:     input.CustomerName,
		DiscountAmount:   input.DiscountAmount,
		OrderTaxAmount:   input.OrderTaxAmount,
		Charges:          input.Charges,
		Source:           input.Source,
		BusinessDate:     input.BusinessDate,
	})
	if err != nil {
		if errors.Is(err, orders.ErrInvalidOrderSubtype) {
			jsonError(w, "invalid order_subtype: must be one of dine_in, takeaway, room_service, delivery, bar_tab, retail", http.StatusBadRequest)
			return
		}
		if errors.Is(err, orders.ErrInvalidBusinessDate) {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.log.Error("create order failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.autoAssignFacilityBookingsForOrder(r.Context(), order, lines)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(order)
}

// autoAssignFacilityBookingsForOrder is the "ring up + auto-assign" side effect of a completed
// sale: for each order line that resolves to a Facility (Facility.InventoryItemID ==
// line.CatalogItemID — the same inventory SERVICE item link the Facilities admin form writes),
// it creates a FacilityBooking consuming line.Quantity seats/guests against that facility for
// today, linked back via PosOrderID. This is how selling a co-working day-pass at the till
// becomes a tracked, capacity-counted assignment with no separate front-desk step.
//
// Deliberately best-effort and non-blocking: a completed, PAID sale must never be rejected or
// rolled back because of a bookkeeping side-table write. If this oversells a shared facility it
// logs a warning (an ops signal) rather than failing the sale — strict capacity rejection only
// happens on the explicit "Assign Space" flow (HotelHandler.BookFacility), which front-desk uses
// for time-boxed reservations. No-ops silently when the tenant lacks the facility_booking
// feature or no line matches a Facility (the overwhelmingly common case for ordinary sales).
func (h *POSOrderHandler) autoAssignFacilityBookingsForOrder(ctx context.Context, order *ent.POSOrder, lines []orders.OrderLineInput) {
	claims, ok := authclient.ClaimsFromContext(ctx)
	if !ok || !claims.HasFeature(subscriptions.FeatureFacilityBooking) {
		return
	}
	for _, line := range lines {
		if line.CatalogItemID == uuid.Nil || line.Quantity <= 0 {
			continue
		}
		fac, err := h.client.Facility.Query().
			Where(
				entfacility.TenantID(order.TenantID),
				entfacility.InventoryItemID(line.CatalogItemID),
				entfacility.IsActive(true),
			).
			Only(ctx)
		if err != nil {
			continue // not a bookable-space line — true for nearly every order line
		}

		seats := int(line.Quantity)
		if seats < 1 {
			seats = 1
		}
		guestName := "Walk-in"
		if order.CustomerName != nil && *order.CustomerName != "" {
			guestName = *order.CustomerName
		}
		phone := ""
		if order.CustomerPhone != nil {
			phone = *order.CustomerPhone
		}
		sessionDate := time.Now()

		booking, err := h.client.FacilityBooking.Create().
			SetTenantID(order.TenantID).
			SetFacilityID(fac.ID).
			SetOutletID(order.OutletID).
			SetGuestName(guestName).
			SetPhone(phone).
			SetSessionDate(sessionDate).
			SetGuestsCount(seats).
			SetSeats(seats).
			SetAmount(line.TotalPrice).
			SetBookedBy(order.UserID).
			SetPosOrderID(order.ID).
			SetNotes("Auto-assigned from POS sale " + order.OrderNumber).
			Save(ctx)
		if err != nil {
			h.log.Warn("auto-assign facility booking failed",
				zap.Error(err), zap.String("order_id", order.ID.String()), zap.String("facility_id", fac.ID.String()))
			continue
		}

		if fac.BookingMode == entfacility.BookingModeShared && fac.Capacity > 0 {
			booked := 0
			for _, b := range sameDayConfirmedFacilityBookings(ctx, h.client, order.TenantID, fac.ID, sessionDate) {
				s := b.Seats
				if s < 1 {
					s = 1
				}
				booked += s
			}
			if booked > fac.Capacity {
				h.log.Warn("facility oversold by auto-assigned booking",
					zap.String("facility_id", fac.ID.String()), zap.String("booking_id", booking.ID.String()),
					zap.Int("booked_seats", booked), zap.Int("capacity", fac.Capacity))
			}
		}
	}
}

// shippingInput is the body for PATCH /pos/orders/{orderID}/shipping (Edit Shipping action).
type shippingInput struct {
	ShippingStatus  string   `json:"shipping_status,omitempty"` // ordered|packed|shipped|delivered|cancelled
	ShippingAddress string   `json:"shipping_address,omitempty"`
	ShippingDetails string   `json:"shipping_details,omitempty"` // courier/vehicle/instructions free text
	ShippingAmount  *float64 `json:"shipping_amount,omitempty"`
	TrackingNumber  string   `json:"tracking_number,omitempty"`
	DeliveredTo     string   `json:"delivered_to,omitempty"`
	DeliveryPerson  string   `json:"delivery_person,omitempty"`
	DeliveryPhone   string   `json:"delivery_phone,omitempty"`
}

// UpdateShipping handles PATCH /{tenantID}/pos/orders/{orderID}/shipping — the All-Sales
// "Edit Shipping" action. Shipping details live in the order metadata (no dedicated columns);
// the All-Sales "Shipping Status" filter reads metadata.shipping_status. For delivery orders
// the frontend separately dispatches to logistics via the existing assign-rider flow.
func (h *POSOrderHandler) UpdateShipping(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}
	var input shippingInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	order, err := h.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	meta := order.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	if input.ShippingStatus != "" {
		meta["shipping_status"] = input.ShippingStatus
	}
	if input.ShippingAddress != "" {
		meta["shipping_address"] = input.ShippingAddress
	}
	if input.ShippingAmount != nil {
		meta["shipping_amount"] = *input.ShippingAmount
	}
	if input.ShippingDetails != "" {
		meta["shipping_details"] = input.ShippingDetails
	}
	if input.TrackingNumber != "" {
		meta["tracking_number"] = input.TrackingNumber
	}
	if input.DeliveredTo != "" {
		meta["delivered_to"] = input.DeliveredTo
	}
	if input.DeliveryPerson != "" {
		meta["delivery_person"] = input.DeliveryPerson
	}
	if input.DeliveryPhone != "" {
		meta["delivery_phone"] = input.DeliveryPhone
	}

	updated, err := h.client.POSOrder.UpdateOneID(orderID).SetMetadata(meta).Save(r.Context())
	if err != nil {
		h.log.Error("update shipping failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

// notifySaleInput optionally redirects the sale notification to a specific phone/email and
// picks the delivery channel (sms | email | whatsapp; empty = notifications-service default).
type notifySaleInput struct {
	Phone   string `json:"phone,omitempty"`
	Email   string `json:"email,omitempty"`
	Channel string `json:"channel,omitempty"`
}

// NotifySale handles POST /{tenantID}/pos/orders/{orderID}/notify — the All-Sales
// "New Sale Notification" action. Publishes pos.sale.notification_requested for
// notifications-service to deliver the receipt/invoice to the customer.
func (h *POSOrderHandler) NotifySale(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}
	var input notifySaleInput
	_ = json.NewDecoder(r.Body).Decode(&input) // body optional

	if _, err := h.orderSvc.RequestSaleNotification(r.Context(), tid, orderID, input.Phone, input.Email, input.Channel); err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		h.log.Error("notify sale failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"status": "queued"})
}

// GetReceiptShareLink handles GET /{tenantID}/pos/orders/{orderID}/receipt/share-link — resolves
// the order's durable public receipt-download link (+ its on-file customer phone) so the
// "Share via WhatsApp" wa.me quick action can build its deep link client-side, without going
// through notifications-service (no message is queued/sent server-side for this path).
func (h *POSOrderHandler) GetReceiptShareLink(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}
	link, phone, err := h.orderSvc.GetReceiptShareLink(r.Context(), tid, orderID)
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		h.log.Error("get receipt share link failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if link == "" {
		h.log.Warn("receipt share link unavailable; missing public api base", zap.String("order_id", orderID.String()))
	}
	jsonOK(w, map[string]any{"download_link": link, "customer_phone": phone})
}

// UpdateStatus handles PATCH /{tenantID}/pos/orders/{orderID}/status
func (h *POSOrderHandler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	tenantID := httpware.GetTenantID(r.Context())
	if tenantID == "" {
		jsonError(w, "tenant_id required", http.StatusBadRequest)
		return
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}

	var input updateStatusInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.Status == "" {
		jsonError(w, "status is required", http.StatusBadRequest)
		return
	}

	updated, err := h.orderSvc.UpdateStatus(r.Context(), tid, orderID, input.Status)
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		h.log.Error("update order status failed", zap.Error(err))
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	jsonOK(w, updated)
}

// VoidOrder handles PATCH /{tenantID}/pos/orders/{orderID}/void
// Requires pos.orders.void permission (admin/manager only).
func (h *POSOrderHandler) VoidOrder(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order id", http.StatusBadRequest)
		return
	}

	var input struct {
		Reason        string `json:"reason"`
		ApprovalToken string `json:"approval_token"`
		// VoidCode is the one-time, order-scoped code a manager generated and shared with the
		// cashier (the "manager not around" flow) — an alternative to the live PIN/card step-up.
		VoidCode string `json:"void_code"`
		// ApprovalCode is the generic outlet-scoped manager approval code (the same primitive
		// the discount/price-override gates use), tried as a third fallback alongside VoidCode.
		ApprovalCode string `json:"approval_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Reason == "" {
		jsonError(w, "reason is required", http.StatusBadRequest)
		return
	}

	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	callerID, _ := uuid.Parse(claims.Subject)

	order, err := h.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}

	// Manager/admin/platform-owner bypass entirely — a cashier's void must always be approved
	// (see below), matching the seeded pos.orders.void_self permission's intent ("cashier may
	// INITIATE a void; they are not a manager override role, so it still requires manager
	// approval before it lands" — cmd/seed/main.go). pos.orders.void_self is checked first
	// (via the standard superuser/JWT/DB-fallback resolution) so a tenant that grants it to a
	// custom role via the permission matrix gets the bypass too, not just the fixed role-name
	// set the discount/price gates still use; that role-name set is kept as a fallback so an
	// existing manager/admin role never regresses.
	callerIsManager := outletmw.HasServicePermission(r, h.rbac, "pos.orders.void_self")
	if !callerIsManager {
		callerIsManager = overrideRoles[requesterRole(r)]
	}
	if !callerIsManager {
		callerIsManager = claims.IsPlatformOwner || hasOverrideRole(claims.Roles)
	}

	// Capture the manager approver. Three ways, in order:
	//  1) a live step-up approval token (manager scanned a card / typed a PIN at the terminal),
	//  2) a one-time void code the manager generated remotely and shared with the cashier, or
	//  3) a generic outlet-scoped approval code (same primitive the discount/price gates use).
	// A cashier (not callerIsManager) MUST produce one of these — void was previously allowed to
	// proceed with no approval at all when none was supplied.
	var approverID *uuid.UUID
	if input.ApprovalToken != "" && len(h.terminalSecret) > 0 {
		if aid, valid := verifyApprovalToken(input.ApprovalToken, "order.void", h.terminalSecret); valid {
			approverID = &aid
		} else {
			jsonError(w, "invalid or expired approval", http.StatusForbidden)
			return
		}
	} else if input.VoidCode != "" {
		if aid, valid := h.redeemVoidCode(r.Context(), tid, orderID, input.VoidCode); valid {
			approverID = &aid
		} else {
			jsonError(w, "invalid or expired void code", http.StatusForbidden)
			return
		}
	} else if input.ApprovalCode != "" {
		if aid, valid := redeemActionApprovalCode(r.Context(), h.client, h.log, tid, order.OutletID, "order.void", input.ApprovalCode); valid {
			approverID = &aid
		} else {
			jsonError(w, "invalid or expired approval code", http.StatusForbidden)
			return
		}
	}

	if voidNeedsApproval(callerIsManager, approverID) {
		respondJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":             "manager approval required to void this sale",
			"approval_required": true, "action": "order.void",
		})
		return
	}

	// Eligibility rule shared with the bulk endpoint (voidSkipReason): already-voided is a
	// no-op, and a finalized sale has already posted to the ledger (and, only if this tenant
	// is actually eTIMS-integrated, transmitted to KRA). Voiding it here would only flip the
	// status — leaving the GL entry (and any eTIMS receipt) un-reversed. Such sales must go
	// through Edit Sale (in-place adjustment) or Delete Sale (full removal, which reverses the
	// ledger and, only when fiscalized, issues an eTIMS credit note).
	switch voidSkipReason(order.Status) {
	case "already_voided":
		jsonError(w, "order is already voided", http.StatusBadRequest)
		return
	case "finalized":
		tenantSlug := ""
		if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
			tenantSlug = claims.GetTenantSlug()
		}
		jsonError(w, h.finalizedVoidRefusalMessage(r.Context(), tenantSlug, order.ID), http.StatusConflict)
		return
	}

	updated, err := h.applyVoid(r.Context(), tid, order, callerID, approverID, input.Reason)
	if err != nil {
		h.log.Error("void order failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

// DeleteDraft handles DELETE /{tenantID}/pos/orders/{orderID} — permanently removes a DRAFT
// (saved-but-unpaid) sale. RBAC enforced server-side (never trust the client): a manager/admin
// holding pos.orders.manage may delete ANY draft; any other caller needs the dedicated
// pos.orders.delete_own permission AND may only delete their OWN draft (order.user_id ==
// caller) — a tenant admin can revoke pos.orders.delete_own from a role independently of its
// other order permissions, or hide it outlet-wide for non-manager-tier callers via the
// OutletSetting "hide Delete for cashiers" quick config (see outletHidesDraftButtonForCashier).
//
// Only draft-status orders are deletable here. A finalized/settled sale carries ledger + KRA
// eTIMS state and must be reversed via void/return instead, so those are rejected with 409.
// A draft was never posted to the ledger or fiscalised, so it is hard-deleted along with its
// child rows (line modifiers, lines, any stray payments, events) in one transaction — nothing
// to reverse, nothing worth keeping for audit beyond the audit-log entry recorded below.
func (h *POSOrderHandler) DeleteDraft(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order id", http.StatusBadRequest)
		return
	}

	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims == nil || claims.Subject == "" {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	callerID, _ := uuid.Parse(claims.Subject)

	order, err := h.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		h.log.Error("delete draft: query order failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Eligibility rule shared with the bulk endpoint (draftDeleteSkipReason):
	// pos.orders.manage → delete any; otherwise the caller needs pos.orders.delete_own (unless
	// the outlet's "hide Delete for cashiers" quick config revokes it for non-manager callers)
	// AND must own the draft. HasServicePermission bypasses for superusers/platform owners,
	// exactly like the read-scoping full-view check.
	canDeleteAny := outletmw.HasServicePermission(r, h.rbac, "pos.orders.manage")
	canDeleteOwn := canDeleteAny || outletmw.HasServicePermission(r, h.rbac, "pos.orders.delete_own")
	if canDeleteOwn && !canDeleteAny && outletHidesDraftButtonForCashier(r.Context(), h.client, order.OutletID, metaKeyHideDraftDeleteForCashier) {
		canDeleteOwn = false
	}
	switch draftDeleteSkipReason(order.Status, order.UserID, callerID, canDeleteAny, canDeleteOwn) {
	case "not_draft":
		jsonError(w, "only draft orders can be deleted — void or return a finalized sale instead", http.StatusConflict)
		return
	case "forbidden":
		jsonError(w, "you do not have permission to delete drafts", http.StatusForbidden)
		return
	case "not_owner":
		jsonError(w, "you can only delete your own drafts", http.StatusForbidden)
		return
	}

	if err := h.hardDeleteDraft(r.Context(), orderID); err != nil {
		h.log.Error("delete draft failed", zap.Stringer("order_id", orderID), zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.recordDraftDeleted(r.Context(), tid, order, callerID)

	jsonOK(w, map[string]any{"deleted": true, "order_number": order.OrderNumber})
}

// AddOrderLines handles POST /{tenantID}/pos/orders/{orderID}/lines
// Appends new items to an existing open order and notifies KDS stations.
func (h *POSOrderHandler) AddOrderLines(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}

	var input struct {
		Lines []createOrderLineInput `json:"lines"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Lines) == 0 {
		jsonError(w, "lines are required", http.StatusBadRequest)
		return
	}

	lines := make([]orders.OrderLineInput, len(input.Lines))
	for i, l := range input.Lines {
		lines[i] = orders.OrderLineInput{
			CatalogItemID:    l.CatalogItemID,
			SKU:              l.SKU,
			Name:             l.Name,
			Category:         l.Category,
			Quantity:         l.Quantity,
			UnitPrice:        l.UnitPrice,
			TotalPrice:       l.TotalPrice,
			CourseNumber:     l.CourseNumber,
			Metadata:         l.Metadata,
			Modifiers:        parseLineModifiers(l.Metadata),
			TaxStatus:        l.TaxStatus,
			TaxCodeID:        l.TaxCodeID,
			PriceIncludesTax: l.PriceIncludesTax,
			TaxRate:          l.TaxRate,
		}
	}

	tenantSlug := ""
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		tenantSlug = claims.GetTenantSlug()
	}
	result, err := h.orderSvc.AddOrderLines(r.Context(), tid, tenantSlug, orderID, lines)
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		h.log.Error("add order lines failed", zap.Error(err))
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	jsonOK(w, result)
}

// CaptureSerial handles POST /{tenantID}/pos/orders/{orderID}/lines/{lineID}/serials
// Captures a serial number for a tracked item on an order line.
func (h *POSOrderHandler) CaptureSerial(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}
	lineID, err := uuid.Parse(chi.URLParam(r, "lineID"))
	if err != nil {
		jsonError(w, "invalid line_id", http.StatusBadRequest)
		return
	}

	var input struct {
		SerialNumber string `json:"serial_number"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.SerialNumber == "" {
		jsonError(w, "serial_number is required", http.StatusBadRequest)
		return
	}

	// Verify the line belongs to this order + tenant
	line, err := h.client.POSOrderLine.Query().
		Where(posorderline.ID(lineID), posorderline.OrderID(orderID)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order line not found", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Create a SerialNumberLog entry for audit trail
	_, err = h.client.SerialNumberLog.Create().
		SetTenantID(tid).
		SetOrderLineID(lineID).
		SetSerialNumber(input.SerialNumber).
		SetItemSku(line.Sku).
		Save(r.Context())
	if err != nil {
		h.log.Error("capture serial failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, map[string]any{"order_line_id": lineID, "serial_number": input.SerialNumber})
}

// FireCourse handles POST /{tenantID}/pos/orders/{orderID}/fire-course
// Marks a course as fired: sets order.fired_courses = course, then creates KDS tickets
// for all lines whose course_number == course (items with lower courses already fired,
// course_number=0 items fire at order creation).
func (h *POSOrderHandler) FireCourse(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}

	var input struct {
		Course int `json:"course"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Course < 1 || input.Course > 9 {
		jsonError(w, "course must be 1–9", http.StatusBadRequest)
		return
	}

	order, err := h.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tid)).
		WithLines().
		Only(r.Context())
	if err != nil {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}

	if input.Course <= order.FiredCourses {
		jsonError(w, "course already fired", http.StatusConflict)
		return
	}

	// Update the order's fired_courses watermark
	updated, err := order.Update().SetFiredCourses(input.Course).Save(r.Context())
	if err != nil {
		h.log.Error("fire-course: update failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Trigger KDS ticket creation for all lines belonging to the fired course
	if err := h.orderSvc.FireCourseKDS(r.Context(), tid, order, input.Course); err != nil {
		h.log.Warn("fire-course: KDS ticket creation partially failed", zap.Error(err))
	}

	jsonOK(w, map[string]any{
		"order_id":      orderID,
		"fired_courses": updated.FiredCourses,
		"course":        input.Course,
	})
}
