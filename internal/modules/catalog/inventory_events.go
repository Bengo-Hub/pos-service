package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	sharedevents "github.com/Bengo-Hub/shared-events"

	"github.com/bengobox/pos-service/internal/ent"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
	"github.com/bengobox/pos-service/internal/modules/notifications"
)

// CatalogStockVersionKey is the Redis key holding a per-tenant "stock version" (a millisecond
// timestamp) that GetCatalogVersion folds into the /pos/catalog/version fingerprint. Bumped when
// inventory stock changes so the terminal's version poll force-refreshes its catalog — without it a
// pure restock never changed the version (which hashes only POSCatalogOverride), so a cached
// out-of-stock row survived in the terminal's IndexedDB until the 5-min sweep.
func CatalogStockVersionKey(tenant string) string { return "pos:catver:" + tenant }

// uuidFromPayload parses a UUID from an event payload value (string after JSON round-trip).
func uuidFromPayload(v any) *uuid.UUID {
	switch t := v.(type) {
	case string:
		if id, err := uuid.Parse(t); err == nil {
			return &id
		}
	case uuid.UUID:
		return &t
	}
	return nil
}

// intPtrFromPayload extracts an *int from a JSON payload value (numbers decode as float64).
func intPtrFromPayload(v any) *int {
	switch t := v.(type) {
	case float64:
		i := int(t)
		return &i
	case int:
		return &t
	case int64:
		i := int(t)
		return &i
	}
	return nil
}

// floatPtrFromPayload extracts a *float64 from a JSON payload value (numbers decode as float64).
func floatPtrFromPayload(v any) *float64 {
	switch t := v.(type) {
	case float64:
		return &t
	case float32:
		f := float64(t)
		return &f
	case int:
		f := float64(t)
		return &f
	}
	return nil
}

// InventoryEventHandler syncs POS-specific compliance flags from inventory events.
// Item data (name, description, image) is always fetched fresh from inventory-api at request time.
// Only flags that POS needs to enforce locally (pharmacy, age-gate, etc.) are stored.
type InventoryEventHandler struct {
	client   *ent.Client
	db       *sql.DB
	redis    *redis.Client
	logger   *zap.Logger
	notifHub catalogNotifHub
}

// catalogNotifHub is the slice of the notifications hub this consumer needs: a tenant-wide push
// so all connected terminals refresh their catalog the instant inventory changes, instead of
// waiting up to ~45s for their version poll. Kept as an interface to avoid a module import cycle.
type catalogNotifHub interface {
	BroadcastToTenant(tenantID uuid.UUID, msg notifications.Message)
}

// SetNotifHub wires the notification hub for real-time catalog-change push (optional).
func (h *InventoryEventHandler) SetNotifHub(hub catalogNotifHub) { h.notifHub = hub }

// NewInventoryEventHandler creates a new inventory event handler. db is the raw *sql.DB backing
// entClient's driver -- syncCatalogItem needs it directly for an atomic ON CONFLICT upsert that
// ent's typed query API can't express (a jsonb `||` merge in the DO UPDATE clause). May be nil in
// tests that don't exercise syncCatalogItem.
func NewInventoryEventHandler(client *ent.Client, db *sql.DB, redisClient *redis.Client, logger *zap.Logger) *InventoryEventHandler {
	return &InventoryEventHandler{
		client: client,
		db:     db,
		redis:  redisClient,
		logger: logger.Named("pos.catalog.inventory_events"),
	}
}

// SubscribeToInventoryEvents subscribes to inventory.item.* JetStream subjects.
func (h *InventoryEventHandler) SubscribeToInventoryEvents(nc *nats.Conn) error {
	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("pos catalog: jetstream init: %w", err)
	}

	if _, err := js.StreamInfo("inventory"); err != nil {
		_, addErr := js.AddStream(&nats.StreamConfig{
			Name:      "inventory",
			Subjects:  []string{"inventory.>"},
			Retention: nats.LimitsPolicy,
			MaxAge:    72 * time.Hour,
			Storage:   nats.FileStorage,
		})
		if addErr != nil && addErr != nats.ErrStreamNameAlreadyInUse {
			return fmt.Errorf("pos catalog: ensure inventory stream: %w", addErr)
		}
	}

	handler := func(msg *nats.Msg) {
		// A panic inside a NATS subscriber goroutine crashes the whole pod. Recover here so a
		// single poison event (or an unexpected nil) is logged + terminated instead of taking
		// the service down. Term() (not Nak) drops the message so it is NOT redelivered into a
		// crash loop.
		defer func() {
			if rec := recover(); rec != nil {
				h.logger.Error("catalog sync: handler panicked — dropping event",
					zap.Any("panic", rec), zap.ByteString("data", msg.Data))
				_ = msg.Term()
			}
		}()
		evt, err := sharedevents.FromJSON(msg.Data)
		if err != nil {
			h.logger.Error("catalog sync: unmarshal event failed", zap.Error(err))
			_ = msg.Nak()
			return
		}
		var handleErr error
		switch evt.EventType {
		case "bundle.created", "bundle.updated":
			handleErr = h.syncBundle(context.Background(), evt)
		case "stock.updated":
			handleErr = h.onStockUpdated(context.Background(), evt)
		case "item.cost_changed":
			handleErr = h.syncItemCostChanged(context.Background(), evt)
		case "item.pricing_updated":
			handleErr = h.onItemPricingUpdated(context.Background(), evt)
		case "item.outlet_pricing_removed":
			handleErr = h.onOutletPricingRemoved(context.Background(), evt)
		default:
			handleErr = h.syncCatalogItem(context.Background(), evt)
		}
		if handleErr != nil {
			h.logger.Error("catalog sync: handler failed",
				zap.String("event_type", evt.EventType), zap.Error(handleErr))
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
	}

	type sub struct {
		subject string
		durable string
	}
	subs := []sub{
		{"inventory.item.created", "pos-inv-item-created"},
		{"inventory.item.updated", "pos-inv-item-updated"},
		// Goods-receipt-driven standard-cost recompute (RecomputeStandardCost) fires this instead of
		// item.updated — a consumer only watching item.updated would miss it and let the cached cost
		// go stale after every purchase-cost change that isn't a full item edit.
		{"inventory.item.cost_changed", "pos-inv-item-cost-changed"},
		// A tiered price edit (inventory-ui's Catalog item detail page, PUT .../items/{id}/pricing,
		// AND the bulk "Generate tier pricing" action) publishes THIS event, never item.updated — it
		// was never subscribed to here at all, so a price change made through that specific screen
		// bumped no version, busted no cache, and pushed no WS broadcast. The only thing that ever
		// surfaced it was pos-ui's unrelated 5-min full-catalog background pull, landing anywhere from
		// ~0-5 minutes later (averaging the reported "2-3 minutes, not instant") depending on where in
		// its cycle the edit happened. Confirmed live 2026-08-12 against boi-enterprises SKU 17606 via
		// this exact endpoint, AFTER today's earlier notifHub cross-pod-relay and cache-bust/broadcast-
		// decouple fixes — those two fixes were real and necessary but didn't cover this event type at
		// all, which is why the symptom kept recurring even once both were live.
		{"inventory.item.pricing_updated", "pos-inv-item-pricing-updated"},
		// A tenant deleted an outlet-scoped inventory price (Catalog item detail page's per-outlet
		// pricing "Delete" action) — cascade by clearing THIS outlet's own selling_price override
		// too, so the branch actually reverts to charging the standard price rather than being
		// silently shadowed by a now-stale pos-api-layer override. See onOutletPricingRemoved.
		{"inventory.item.outlet_pricing_removed", "pos-inv-outlet-pricing-removed"},
		{"inventory.bundle.created", "pos-inv-bundle-created"},
		{"inventory.bundle.updated", "pos-inv-bundle-updated"},
		// Stock-level changes (restock / adjustment / stock-take / sale deduction) bump the catalog
		// version so the terminal auto-refreshes its stock within ~45s instead of showing a stale
		// out-of-stock row until the 5-min sweep.
		{"inventory.stock.updated", "pos-inv-stock-updated"},
	}
	for _, s := range subs {
		// Multi-layer rebind: settle buffer + retry-on-"already bound" so a
		// restart never silently drops the inventory->POS catalog sync.
		sharedevents.SubscribeQueueWithRebind(h.logger, js, "inventory", s.subject, s.durable, handler,
			nats.Durable(s.durable),
			nats.AckExplicit(),
			nats.AckWait(30*time.Second),
			nats.MaxDeliver(5),
			nats.DeliverAll(),
		)
	}

	h.logger.Info("inventory catalog sync subscriptions active",
		zap.String("subjects", "inventory.item.created/updated/cost_changed/pricing_updated, inventory.bundle.created/updated, inventory.stock.updated"))
	return nil
}

// onStockUpdated reacts to an inventory stock-level change by bumping the tenant's catalog stock
// version (folded into GetCatalogVersion) so terminals force-refresh their catalog, and busting the
// per-(tenant,outlet,use-case) catalog source cache so that refresh reads fresh on_hand instead of
// the up-to-60s-stale Redis snapshot.
//
// The version bump and cache-bust ALWAYS run, on every event — both are cheap (a Redis SET and a
// SCAN+DEL) and neither causes any client-side work by itself. Only the real-time WS broadcast is
// rate-limited (at most once per tenant per 30s, see the lock below): a busy till emits a
// stock.updated per sale, and an un-debounced broadcast would make every terminal refetch the whole
// (potentially multi-thousand item) catalog on every sale. Terminals whose socket is momentarily
// down (or between broadcasts) still get the fresh version+cache within their next ~45s poll.
//
// NOTE (2026-08-12): the cache-bust used to be INSIDE the same debounce gate as the broadcast — on
// a busy multi-thousand-item store, a run of stock.updated events every few seconds meant the
// debounce lock was almost always already held, so a human's specific edit (price, availability)
// could have its cache-bust silently skipped too, not just its broadcast. The Redis catalogsrc
// cache then stayed stale for up to its own 60s TTL on top of the poll interval, compounding into
// the "took a couple of minutes, not instant" symptom reported live against boi-enterprises SKU
// 17606. Splitting these apart so the cache-bust is unconditional closes that gap.
func (h *InventoryEventHandler) onStockUpdated(ctx context.Context, evt *sharedevents.Event) error {
	if h.redis == nil {
		return nil
	}
	tenant := evt.TenantID.String()
	if tenant == "" || tenant == "00000000-0000-0000-0000-000000000000" {
		return nil
	}
	// Bump the stock version → /pos/catalog/version changes → terminals force-refresh.
	if err := h.redis.Set(ctx, CatalogStockVersionKey(tenant), fmt.Sprintf("%d", time.Now().UnixMilli()), 0).Err(); err != nil {
		h.logger.Debug("catalog stock version bump failed", zap.String("tenant", tenant), zap.Error(err))
	}
	h.bustCatalogSourceCache(ctx, tenant)
	h.broadcastCatalogChangedDebounced(ctx, tenant, evt.TenantID, "pos:catver-lock:")
	return nil
}

// onItemPricingUpdated reacts to inventory.item.pricing_updated — published only by the dedicated
// per-tier pricing endpoints (single-item PUT .../items/{id}/pricing and the bulk tier-generate
// action), never by item.updated. Its payload is deliberately thin ({item_id, pricing_tier_id,
// price, previous_price, currency, effective_from, is_active, updated_at} — no sku/use_case/etc.),
// so unlike syncCatalogItem this can't upsert a POSCatalogOverride row; there's nothing here to
// upsert anyway, since the numeric price itself is never cached on that row (pos-api reads it live
// from inventory-api's item_pricings through the pos:catalogsrc cache, per catalog three-layer
// pricing — see [[catalog-three-layer-pricing]]). All that's actually needed is the same
// version-bump + cache-bust + debounced-broadcast triplet onStockUpdated already does, so a price
// edit through this specific screen gets the identical instant path instead of relying on the
// unrelated 5-min full-catalog background pull as its only way to ever surface. Own lock prefix
// (pos:catprice-lock:) so a bulk tier-regenerate (one event per item, easily thousands in a burst)
// can't starve a concurrent stock-driven broadcast, or vice versa.
func (h *InventoryEventHandler) onItemPricingUpdated(ctx context.Context, evt *sharedevents.Event) error {
	if h.redis == nil {
		return nil
	}
	tenant := evt.TenantID.String()
	if tenant == "" || tenant == "00000000-0000-0000-0000-000000000000" {
		return nil
	}
	if err := h.redis.Set(ctx, CatalogStockVersionKey(tenant), fmt.Sprintf("%d", time.Now().UnixMilli()), 0).Err(); err != nil {
		h.logger.Debug("catalog price version bump failed", zap.String("tenant", tenant), zap.Error(err))
	}
	h.bustCatalogSourceCache(ctx, tenant)
	h.broadcastCatalogChangedDebounced(ctx, tenant, evt.TenantID, "pos:catprice-lock:")
	return nil
}

// onOutletPricingRemoved reacts to inventory.item.outlet_pricing_removed — published only when a
// tenant deletes an outlet-scoped inventory price (never by a plain price edit, which stays
// item.pricing_updated). Clears THIS OUTLET's own POSCatalogOverride.selling_price, if one is
// set, so the branch actually reverts to the tenant-wide price the tenant expects, instead of
// being silently shadowed by a pos-api-layer override nobody remembers is still there — the
// whole reason a plain inventory-side delete needs to cascade at all: pos-api's own
// selling_price is an INDEPENDENT override, not a cache of inventory's price (see
// onItemPricingUpdated's doc comment), so deleting only the inventory row would otherwise leave
// checkout charging whatever pos-api still has on file.
func (h *InventoryEventHandler) onOutletPricingRemoved(ctx context.Context, evt *sharedevents.Event) error {
	sku, _ := evt.Payload["sku"].(string)
	outletID := uuidFromPayload(evt.Payload["outlet_id"])
	if sku == "" || outletID == nil {
		return nil
	}
	tenant := evt.TenantID.String()
	if tenant == "" || tenant == "00000000-0000-0000-0000-000000000000" {
		return nil
	}

	existing, err := h.client.POSCatalogOverride.Query().
		Where(entoverride.TenantID(evt.TenantID), entoverride.InventorySku(sku), entoverride.OutletID(*outletID)).
		Only(ctx)
	if err != nil {
		// No POS-layer override for this outlet — nothing to cascade, not an error.
		return nil
	}
	if existing.SellingPrice == nil {
		return nil
	}
	if _, err := existing.Update().ClearSellingPrice().Save(ctx); err != nil {
		return fmt.Errorf("clear outlet selling_price override: %w", err)
	}
	h.logger.Info("POS catalog outlet selling_price override cleared (cascaded from inventory delete)",
		zap.String("sku", sku), zap.String("outlet_id", outletID.String()))

	if h.redis != nil {
		h.bustCatalogSourceCache(ctx, tenant)
		h.broadcastCatalogChangedDebounced(ctx, tenant, evt.TenantID, "pos:catprice-lock:")
	}
	return nil
}

// broadcastCatalogChangedDebounced pushes a real-time catalog-changed signal to every terminal on
// the tenant, at most once per tenant per 30s per lockPrefix (onStockUpdated and syncCatalogItem
// each use their own prefix so a burst of one kind of event can't starve the other's broadcast
// entirely). Never gates the cache-bust — call this AFTER busting the cache, not instead of it.
func (h *InventoryEventHandler) broadcastCatalogChangedDebounced(ctx context.Context, tenant string, tenantID uuid.UUID, lockPrefix string) {
	if h.redis == nil || h.notifHub == nil {
		return
	}
	ok, err := h.redis.SetNX(ctx, lockPrefix+tenant, "1", 30*time.Second).Result()
	if err != nil || !ok {
		return
	}
	h.notifHub.BroadcastToTenant(tenantID, notifications.Message{
		Type:    "catalog_changed",
		Payload: map[string]any{"tenant_id": tenant},
	})
}

// bustCatalogSourceCache deletes all pos:catalogsrc:{tenant}:* cache entries (one per outlet /
// use-case set) so the next catalog fetch reads fresh on_hand from inventory-api.
func (h *InventoryEventHandler) bustCatalogSourceCache(ctx context.Context, tenant string) {
	pattern := "pos:catalogsrc:" + tenant + ":*"
	var cursor uint64
	for {
		keys, cur, err := h.redis.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return
		}
		if len(keys) > 0 {
			_ = h.redis.Del(ctx, keys...).Err()
		}
		cursor = cur
		if cursor == 0 {
			return
		}
	}
}

// syncCatalogItem upserts the POSCatalogOverride projection for an inventory item.
// Item name/description/image are always fetched fresh from inventory-api at request time;
// this row stores POS-relevant flags + the synced reference data (use_case, tax, compliance)
// so hospitality items (rooms/facilities/amenities/services) and others project into POS.
func (h *InventoryEventHandler) syncCatalogItem(ctx context.Context, evt *sharedevents.Event) error {
	sku, _ := evt.Payload["sku"].(string)
	itemType, _ := evt.Payload["type"].(string)
	if sku == "" {
		return nil
	}

	// Skip non-sellable inventory types — only GOODS, RECIPE, SERVICE, VOUCHER belong in POS
	if itemType == "INGREDIENT" || itemType == "EQUIPMENT" {
		return nil
	}

	if evt.TenantID.String() == "00000000-0000-0000-0000-000000000000" {
		return nil
	}
	tenantID := evt.TenantID

	requiresAgeVerification, _ := evt.Payload["requires_age_verification"].(bool)
	isControlledSubstance, _ := evt.Payload["is_controlled_substance"].(bool)
	isActive, _ := evt.Payload["is_active"].(bool)
	useCase, _ := evt.Payload["use_case"].(string)
	taxCodeID, _ := evt.Payload["tax_code_id"].(string)

	itemID := uuidFromPayload(evt.Payload["id"])
	durationMinutes := intPtrFromPayload(evt.Payload["duration_minutes"])
	// Cache the inventory cost so POS-side profitability reports (Most-Profitable, GetSummary,
	// SalesByHour) can compute real margins without an S2S call. Stored in metadata.cost_price;
	// selling_price stays the price override.
	costPrice := floatPtrFromPayload(evt.Payload["cost_price"])
	// Cache the item's stock unit so pos.sale.finalized can carry a real uom_code
	// (inventory converts non-stock-unit sale quantities before deducting).
	unitName, _ := evt.Payload["unit_name"].(string)
	// Cache manufacturer/category/brand alongside cost so the ?group_by=manufacturer|category|brand
	// rollup on MostProfitableItems can also read the local cache instead of a live catalog walk
	// (orders.CatalogCacheBySKU resolves cost + these fields together, one query).
	manufacturer, _ := evt.Payload["manufacturer"].(string)
	categoryName, _ := evt.Payload["category_name"].(string)
	brandName, _ := evt.Payload["brand_name"].(string)

	md := map[string]any{}
	if costPrice != nil {
		md["cost_price"] = *costPrice
	}
	if unitName != "" {
		md["uom"] = unitName
	}
	if manufacturer != "" {
		md["manufacturer"] = manufacturer
	}
	if categoryName != "" {
		md["category_name"] = categoryName
	}
	if brandName != "" {
		md["brand_name"] = brandName
	}

	// Atomic create-or-update against the tenant-wide (outlet_id IS NULL) row — see upsert.go for
	// why this must be a single DB-level upsert rather than a query-then-create/update: the latter
	// raced under concurrent event delivery and produced duplicate rows, which is what silently
	// broke sale-time COGS cost lookups for a large share of items (see migration
	// 20260803191626_pos_catalog_override_dedupe_guard.sql).
	if err := upsertSyncedCatalogOverride(ctx, h.db, tenantID, sku, upsertSyncedItemFields{
		ItemUseCase:             useCase,
		TaxCodeID:               taxCodeID,
		DurationMinutes:         durationMinutes,
		RequiresAgeVerification: requiresAgeVerification,
		IsControlledSubstance:   isControlledSubstance,
		IsAvailable:             isActive,
		InventoryItemID:         itemID,
		Metadata:                md,
	}); err != nil {
		return err
	}
	h.logger.Debug("POS catalog override synced", zap.String("sku", sku), zap.String("use_case", useCase))

	// item.updated (incl. a price-only change — see inventory-api pricing_enrich.go
	// setSellingPrice) already bumps GetCatalogVersion's fingerprint via the upsert above
	// (ON CONFLICT ... updated_at=now()), but until now that was the ONLY signal: no Redis
	// cache-bust, no real-time WS push, so a price edit relied purely on the ~45s version poll
	// outliving the 60s cachedCatalogSource TTL. Give item.updated the SAME low-latency path
	// stock.updated already gets, so a price correction shows up on the terminal immediately
	// instead of "eventually, maybe" — this was the root cause of "I changed the price and POS
	// never reflects it, even after refresh."
	//
	// The cache-bust always runs (cheap, no client-visible side effect by itself); only the WS
	// broadcast is debounced (own lock prefix, separate from onStockUpdated's, so a burst of
	// unrelated stock events can't starve a price/availability edit's broadcast — see
	// broadcastCatalogChangedDebounced's doc comment for why these used to be bundled together).
	if h.redis != nil {
		tenant := tenantID.String()
		h.bustCatalogSourceCache(ctx, tenant)
		h.broadcastCatalogChangedDebounced(ctx, tenant, tenantID, "pos:catsync-lock:")
	}
	return nil
}

// syncItemCostChanged reacts to inventory.item.cost_changed — a goods-receipt-driven standard-cost
// recompute (stock.Service.RecomputeStandardCost) — which carries a much thinner payload than
// item.updated/item.created: only {item_id, sku, previous_cost, new_cost, source}, no
// use_case/is_active/tax_code_id/etc. Updates ONLY the cached cost_price metadata key via
// upsertCatalogCostOnly; it must NOT go through syncCatalogItem/upsertSyncedCatalogOverride, whose
// DO UPDATE SET item_use_case = EXCLUDED.item_use_case / is_available = EXCLUDED.is_available (both
// unconditional, not COALESCE) would silently blank those real values back to empty/false from this
// event's missing fields.
func (h *InventoryEventHandler) syncItemCostChanged(ctx context.Context, evt *sharedevents.Event) error {
	sku, _ := evt.Payload["sku"].(string)
	if sku == "" {
		return nil
	}
	if evt.TenantID.String() == "00000000-0000-0000-0000-000000000000" {
		return nil
	}
	newCost := floatPtrFromPayload(evt.Payload["new_cost"])
	if newCost == nil {
		return nil
	}
	if err := upsertCatalogCostOnly(ctx, h.db, evt.TenantID, sku, *newCost); err != nil {
		return err
	}
	h.logger.Debug("POS catalog cost-only sync (goods receipt cost recompute)",
		zap.String("sku", sku), zap.Float64("new_cost", *newCost))
	return nil
}

// syncBundle marks the catalog projection of a bundle's parent item as a package
// (conference DDR/RDR, room rate plan, service-session bundle).
func (h *InventoryEventHandler) syncBundle(ctx context.Context, evt *sharedevents.Event) error {
	if evt.TenantID.String() == "00000000-0000-0000-0000-000000000000" {
		return nil
	}
	tenantID := evt.TenantID
	sku, _ := evt.Payload["sku"].(string)
	itemID := uuidFromPayload(evt.Payload["item_id"])
	packageType, _ := evt.Payload["package_type"].(string)
	isActive, _ := evt.Payload["is_active"].(bool)

	q := h.client.POSCatalogOverride.Query().Where(entoverride.TenantID(tenantID))
	switch {
	case sku != "":
		q = q.Where(entoverride.InventorySku(sku))
	case itemID != nil:
		q = q.Where(entoverride.InventoryItemID(*itemID))
	default:
		return nil
	}
	existing, _ := q.First(ctx)
	if existing == nil {
		// Parent item event not yet projected; nothing to flag. Item event will create the row.
		h.logger.Debug("bundle sync: no catalog row yet", zap.String("sku", sku))
		return nil
	}
	upd := existing.Update().SetIsBundle(true).SetIsAvailable(isActive)
	md := existing.Metadata
	if md == nil {
		md = map[string]any{}
	}
	md["package_type"] = packageType
	upd = upd.SetMetadata(md)
	if _, err := upd.Save(ctx); err != nil {
		return fmt.Errorf("flag bundle override: %w", err)
	}
	h.logger.Debug("POS catalog bundle flagged", zap.String("sku", sku), zap.String("package_type", packageType))
	return nil
}
