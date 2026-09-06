package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	sharedcache "github.com/Bengo-Hub/cache"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/config"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/migrate"
	handlers "github.com/bengobox/pos-service/internal/http/handlers"
	router "github.com/bengobox/pos-service/internal/http/router"
	backupmod "github.com/bengobox/pos-service/internal/modules/backup"
	catalogmodule "github.com/bengobox/pos-service/internal/modules/catalog"
	"github.com/bengobox/pos-service/internal/modules/documents"
	"github.com/bengobox/pos-service/internal/modules/identity"
	inventorymodule "github.com/bengobox/pos-service/internal/modules/inventory"
	kdsmodule "github.com/bengobox/pos-service/internal/modules/kds"
	notifmodule "github.com/bengobox/pos-service/internal/modules/notifications"
	ordermodule "github.com/bengobox/pos-service/internal/modules/orders"
	paymentmodule "github.com/bengobox/pos-service/internal/modules/payments"
	"github.com/bengobox/pos-service/internal/modules/printing"
	promommodule "github.com/bengobox/pos-service/internal/modules/promotions"
	rbacmodule "github.com/bengobox/pos-service/internal/modules/rbac"
	"github.com/bengobox/pos-service/internal/modules/returns"
	"github.com/bengobox/pos-service/internal/modules/reversals"
	"github.com/bengobox/pos-service/internal/modules/saledelete"
	"github.com/bengobox/pos-service/internal/modules/saleedit"
	shiftsmodule "github.com/bengobox/pos-service/internal/modules/shifts"
	"github.com/bengobox/pos-service/internal/modules/staffcredit"
	"github.com/bengobox/pos-service/internal/modules/tenant"
	treasurymodule "github.com/bengobox/pos-service/internal/modules/treasury"
	webhookmodule "github.com/bengobox/pos-service/internal/modules/webhooks"
	"github.com/bengobox/pos-service/internal/platform/cache"
	"github.com/bengobox/pos-service/internal/platform/database"
	"github.com/bengobox/pos-service/internal/platform/erp"
	"github.com/bengobox/pos-service/internal/platform/events"
	logisticsclient "github.com/bengobox/pos-service/internal/platform/logistics"
	"github.com/bengobox/pos-service/internal/platform/marketflow"
	orderingclient "github.com/bengobox/pos-service/internal/platform/ordering"
	"github.com/bengobox/pos-service/internal/platform/scheduler"
	"github.com/bengobox/pos-service/internal/platform/subscriptions"
	webhookspkg "github.com/bengobox/pos-service/internal/platform/webhooks"
	"github.com/bengobox/pos-service/internal/shared/logger"
)

type App struct {
	cfg                      *config.Config
	log                      *zap.Logger
	httpServer               *http.Server
	db                       *pgxpool.Pool
	entClient                *ent.Client
	cache                    *redis.Client
	events                   *nats.Conn
	outboxPublisher          *eventslib.Publisher
	webhookWorker            *webhookmodule.DeliveryWorker
	shiftAutoEndWorker       *shiftsmodule.AutoEndWorker
	saleFinalizedReconciler  *paymentmodule.SaleFinalizedReconciler
	treasuryIntentReconciler *paymentmodule.TreasuryIntentReconciler
	kdsHub                   *kdsmodule.Hub
	printHub                 *printing.Hub
	notifHub                 *notifmodule.Hub
	layawayReminderScheduler *scheduler.LayawayReminderScheduler
}

// newReadOnlyEntClient opens a separate Ent client against cfg.ReadOnlyURL (a read replica, via
// pgbouncer's pos_ro alias in prod) for the read-routing described on readEntClient in New(). A
// smaller pool than the primary's — this backs a handful of specific endpoints, not general
// traffic. No migration/schema management here; the replica streams the primary's schema.
func newReadOnlyEntClient(cfg config.PostgresConfig) (*ent.Client, error) {
	sqlDB, err := sql.Open("pgx", cfg.ReadOnlyURL)
	if err != nil {
		return nil, fmt.Errorf("sql open for read-replica ent client: %w", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("read-replica ping: %w", err)
	}
	maxOpen := cfg.MaxOpenConns / 2
	if maxOpen < 2 {
		maxOpen = 2
	}
	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(1 * time.Minute)
	drv := entsql.OpenDB(dialect.Postgres, sqlDB)
	return ent.NewClient(ent.Driver(drv)), nil
}

func New(ctx context.Context) (*App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}

	log, err := logger.New(cfg.App.Env)
	if err != nil {
		return nil, fmt.Errorf("logger init: %w", err)
	}

	dbPool, err := database.NewPool(ctx, cfg.Postgres)
	if err != nil {
		return nil, fmt.Errorf("postgres init: %w", err)
	}

	redisClient := cache.NewClient(cfg.Redis)

	natsConn, err := events.Connect(cfg.Events)
	if err != nil {
		log.Warn("event bus connection failed", zap.Error(err))
	}

	healthHandler := handlers.NewHealthHandler(log, dbPool, redisClient, natsConn)

	// Initialize auth-service JWT validator
	var authMiddleware *authclient.AuthMiddleware
	authConfig := authclient.DefaultConfig(
		cfg.Auth.JWKSUrl,
		cfg.Auth.Issuer,
		cfg.Auth.Audience,
	)
	authConfig.CacheTTL = cfg.Auth.JWKSCacheTTL
	authConfig.RefreshInterval = cfg.Auth.JWKSRefreshInterval

	validator, err := authclient.NewValidator(authConfig)
	if err != nil {
		return nil, fmt.Errorf("auth validator init: %w", err)
	}
	if cfg.Auth.EnableAPIKeyAuth {
		apiKeyValidator := authclient.NewAPIKeyValidator(cfg.Auth.ServiceURL, nil)
		authMiddleware = authclient.NewAuthMiddlewareWithAPIKey(validator, apiKeyValidator)
	} else {
		authMiddleware = authclient.NewAuthMiddleware(validator)
	}

	sqlDB, err := sql.Open("pgx", cfg.Postgres.URL)
	if err != nil {
		return nil, fmt.Errorf("sql open for ent: %w", err)
	}
	sqlDB.SetMaxOpenConns(cfg.Postgres.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.Postgres.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.Postgres.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(1 * time.Minute)
	drv := entsql.OpenDB(dialect.Postgres, sqlDB)
	entClient := ent.NewClient(ent.Driver(drv))

	// readEntClient points a handful of heavy, staleness-tolerant read endpoints (All-Sales
	// list/export today) at a read replica instead of the primary, via cfg.Postgres.ReadOnlyURL
	// (pgbouncer's pos_ro alias in prod — see devops-k8s). Unset (every environment that hasn't
	// been explicitly given the env var, incl. local dev) falls back to entClient itself — zero
	// behavior change. A replica connection failure at startup is logged and swallowed, not
	// fatal: those endpoints just keep using the primary, same as before this existed.
	readEntClient := entClient
	if cfg.Postgres.ReadOnlyURL != "" {
		if roClient, err := newReadOnlyEntClient(cfg.Postgres); err != nil {
			log.Warn("read-replica postgres connection failed — read-heavy endpoints will use the primary", zap.Error(err))
		} else {
			readEntClient = roClient
		}
	}

	// Run versioned migrations only when explicitly enabled.
	// In production, migrations are run by the entrypoint (cmd/migrate) before the server starts —
	// this inline path is a dev/local convenience only. Same WithDropColumn/WithDropIndex flags as
	// cmd/migrate/main.go for dev-parity: without them ent's live diff silently skips any
	// index/column removal a struct change implies (the 2026-07-26 outage this service's
	// cmd/migrate binary was fixed for), and a dev testing locally with RunMigrations=true should
	// see the same drop behavior production's real migrate job does.
	if cfg.Postgres.RunMigrations {
		if err := entClient.Schema.Create(ctx,
			schema.WithDir(migrate.Dir),
			schema.WithDropColumn(true),
			schema.WithDropIndex(true),
		); err != nil {
			return nil, fmt.Errorf("ent schema create: %w", err)
		}
		log.Info("versioned migrations completed")
	}

	subsClient := subscriptions.NewClient(subscriptions.Config{
		ServiceURL:     cfg.Subscriptions.ServiceURL,
		RequestTimeout: cfg.Subscriptions.RequestTimeout,
		APIKey:         cfg.Subscriptions.APIKey,
	}, log.Named("subscriptions.client"))

	tenantCache := sharedcache.New(redisClient, log)
	tenantSyncer := tenant.NewSyncer(entClient, cfg.Auth.ServiceURL, tenantCache).WithDB(sqlDB)
	identitySvc := identity.NewService(entClient, tenantSyncer)

	// Initialize business services
	orderSvc := ordermodule.NewService(entClient, ordermodule.Config{
		DefaultCurrency: cfg.App.DefaultCurrency,
		TaxRatePercent:  cfg.App.TaxRatePercent,
		OrderPrefix:     cfg.App.OrderPrefix,
	}, log)

	// Per-tenant, numeric-by-default document numbering (order/receipt/return/reversal/repair-job).
	// One SequenceService fans out to every generator + the Settings → Documents config API,
	// mirroring inventory-api's DocumentSequence wiring.
	docSeqSvc := documents.NewSequenceService(entClient)
	orderSvc.WithSequence(docSeqSvc) // online order numbers via the order sequence (offline path unchanged)
	// Same public base URL already used to build the payments-initiate callback URL — reused here
	// to build the durable, unauthenticated receipt-share download link (public_token capability
	// URL) embedded in pos.sale.notification_requested for notifications-service.
	orderSvc.WithPublicAPIBase(cfg.Treasury.PublicBaseURL)
	orderSvc.WithShortLinkBase(cfg.Treasury.ShortLinkBaseURL)

	// Wire event publisher for POS order lifecycle events (shared-events outbox pattern)
	var outboxPub *eventslib.Publisher
	var eventPub *events.Publisher
	if natsConn != nil {
		eventPub = events.NewPublisher(sqlDB, log)
		orderSvc.SetPublisher(eventPub)

		// Start background outbox publisher
		js, jsErr := natsConn.JetStream()
		if jsErr != nil {
			log.Warn("jetstream init for outbox publisher", zap.Error(jsErr))
		} else {
			pubCfg := eventslib.DefaultPublisherConfig(js, eventPub.OutboxRepo(), log)
			outboxPub = eventslib.NewPublisher(pubCfg)
		}
	}

	// MarketFlow CRM S2S client (async contact upsert on loyalty account creation)
	mfClient := marketflow.NewClient(cfg.MarketFlow.ServiceURL, cfg.MarketFlow.APIKey, log)

	// Treasury S2S client (thin proxy; pos-api delegates all payment processing to treasury-api)
	treasuryClient := treasurymodule.NewClient(cfg.Treasury.ServiceURL, cfg.Treasury.InternalServiceKey, cfg.Treasury.RequestTimeout)

	// Tax resolver: fetches TaxCode definitions from treasury S2S with Redis caching (10-min TTL).
	// Used by orders.Service to compute tax per order line at creation time.
	taxResolver := ordermodule.NewTaxResolver(treasuryClient, redisClient, log)
	orderSvc.SetTaxResolver(taxResolver)

	// Inventory S2S client for stock backflush after order completion
	inventoryAPIURL := os.Getenv("INVENTORY_API_URL")
	if inventoryAPIURL == "" {
		inventoryAPIURL = "http://inventory-api.inventory.svc.cluster.local:4000"
	}
	inventoryClient := inventorymodule.NewClient(inventoryAPIURL, cfg.Treasury.InternalServiceKey, 15*time.Second)

	paymentSvc := paymentmodule.NewService(entClient, orderSvc, cfg.App.DefaultCurrency, log)
	paymentSvc.SetTreasuryClient(treasuryClient)
	paymentSvc.SetInventoryClient(inventoryClient)
	paymentSvc.SetMarketFlowClient(mfClient)
	if pub := orderSvc.GetPublisher(); pub != nil {
		paymentSvc.SetPublisher(pub)
	}
	promoSvc := promommodule.NewService(entClient, log)
	// Auto-apply scope-enforced happy-hour / negotiated-meal discounts at checkout, and audit
	// the applied promo (decoupled hooks into the orders service; app.go adapts the line types).
	orderSvc.SetHappyHourEvaluator(
		func(ctx context.Context, tenantID, outletID uuid.UUID, lines []ordermodule.TimedOrderLine) ordermodule.HappyHourOutcome {
			dls := make([]promommodule.TimedDiscountLine, 0, len(lines))
			for _, l := range lines {
				dls = append(dls, promommodule.TimedDiscountLine{
					DiscountLine: promommodule.DiscountLine{
						SKU: l.SKU, Category: l.Category, Total: decimal.NewFromFloat(l.TotalPrice),
						Quantity: l.Quantity, UnitPrice: decimal.NewFromFloat(l.UnitPrice),
					},
					AddedAt: l.AddedAt,
				})
			}
			r := promoSvc.EvaluateTimedOrderDiscounts(ctx, tenantID, outletID, dls)
			bySKU := make(map[string]ordermodule.HappyHourLine, len(r.PerSKU))
			for sku, ld := range r.PerSKU {
				bySKU[sku] = ordermodule.HappyHourLine{
					Amount:  ld.Amount.InexactFloat64(),
					FreeQty: ld.FreeQty,
					Type:    ld.Type,
					Label:   ld.Label,
				}
			}
			return ordermodule.HappyHourOutcome{
				PromoID:              r.PromoID,
				PromoName:            r.PromoName,
				Discount:             r.Discount,
				BySKU:                bySKU,
				ContributingPromoIDs: r.ContributingPromoIDs,
				PerPromoAmount:       r.PerPromoAmount,
			}
		},
		promoSvc.RecordApplication,
	)

	// Centralized audit trail for sensitive/fraud-relevant actions.
	auditSvc := audit.NewService(entClient, log)

	// Create HTTP handlers
	orderHandler := handlers.NewPOSOrderHandler(log, entClient, orderSvc, subsClient)
	orderHandler.SetAuditService(auditSvc)
	// Line-edit price corrections can optionally propagate to the inventory catalog.
	orderHandler.SetInventoryClient(inventoryClient)
	// Only used to confirm actual fiscal status before a void-refusal message mentions KRA eTIMS.
	orderHandler.SetTreasuryClient(treasuryClient)
	// Lets a total-reducing line void/edit recheck payment completion (see orders.go's
	// paymentSvc field doc for the incident this fixes).
	orderHandler.SetPaymentService(paymentSvc)
	// All-Sales list routed to a read replica when configured — see readEntClient above.
	orderHandler.SetReadClient(readEntClient)
	// Enforces Promotion.usage_limit/max_units_per_customer when an order carries a promotion_id.
	orderHandler.SetPromotionsService(promoSvc)
	catalogHandler := handlers.NewCatalogHandler(log, entClient)
	catalogHandler.SetRedisClient(redisClient)
	tableHandler := handlers.NewTableHandler(log, entClient)
	if pub := orderSvc.GetPublisher(); pub != nil {
		tableHandler.SetPublisher(pub)
	}
	tenderHandler := handlers.NewTenderHandler(log, entClient)
	paymentHandler := handlers.NewPaymentHandler(log, paymentSvc, treasuryClient, cfg.Treasury.PublicBaseURL, entClient)
	var drawerPublisher *events.Publisher
	if pub := orderSvc.GetPublisher(); pub != nil {
		drawerPublisher = pub
	}
	drawerHandler := handlers.NewDrawerHandler(log, entClient, drawerPublisher)
	drawerHandler.SetAuditService(auditSvc)
	barTabHandler := handlers.NewBarTabHandler(log, entClient)
	promotionHandler := handlers.NewPromotionHandler(log, entClient, promoSvc)
	// Storefront-banner feature gate (subscriptions.FeatureStorefrontBanner) — enforced inline
	// at the point of write/read in the promotions handlers, not a route-group middleware.
	promotionHandler.SetSubscriptionsClient(subsClient)

	// Hotel, KDS and device session handlers
	var hotelEventPub *events.Publisher
	if pub := orderSvc.GetPublisher(); pub != nil {
		hotelEventPub = pub
	}
	hotelHandler := handlers.NewHotelHandler(log, entClient, hotelEventPub)
	hotelHandler.SetTreasuryClient(treasuryClient)
	hotelHandler.SetInventoryClient(inventoryClient)
	hotelHandler.SetSubscriptionsClient(subsClient)
	kdsHandler := handlers.NewKDSHandler(log, entClient)
	kdsHandler.Hub().SetRedis(redisClient)
	kdsHub := kdsHandler.Hub()
	deviceHandler := handlers.NewDeviceHandler(log, entClient)
	if pub := orderSvc.GetPublisher(); pub != nil {
		deviceHandler.SetPublisher(pub)
	}
	notificationsHandler := handlers.NewNotificationsHandler(log, entClient)
	// Cross-pod relay: with multiple pos-api replicas, the pod that consumes an inventory/treasury
	// event (or handles the triggering HTTP request) is rarely the same pod a given terminal's
	// WebSocket is connected to. Without this, catalog_changed/eTIMS-block/treasury-balance pushes
	// only reached terminals lucky enough to share a pod with the event — see notifications.Hub's
	// doc comment. Mirrors kdsHandler/printHub's SetRedis+Start below.
	notifHub := notificationsHandler.Hub()
	notifHub.SetRedis(redisClient)
	// Push the eTIMS fiscal block to the selling cashier's terminal the instant a sale is signed
	// (sync checkout sign OR async treasury.etims.invoice_transmitted), so the receipt's KRA TIMS
	// details appear via WebSocket push instead of the terminal polling the receipt endpoint.
	paymentSvc.SetNotifHub(notificationsHandler.Hub())
	// Wire the shared notification hub so KDS can push real-time alerts to floor staff.
	kdsHandler.SetNotifHub(notificationsHandler.Hub())
	queueHandler := handlers.NewQueueHandler(log, entClient)

	// Terminal PIN auth — TERMINAL_JWT_SECRET must be set in production.
	// Falls back to INTERNAL_SERVICE_KEY only to prevent a hard startup failure in dev/local environments.
	terminalJWTSecret := []byte(cfg.Auth.TerminalJWTSecret)
	if len(terminalJWTSecret) == 0 {
		log.Warn("TERMINAL_JWT_SECRET is not set; falling back to INTERNAL_SERVICE_KEY for terminal JWT signing — set TERMINAL_JWT_SECRET in production")
		terminalJWTSecret = []byte(cfg.Treasury.InternalServiceKey)
	}
	pinAuthHandler := handlers.NewPINAuthHandler(log, entClient, terminalJWTSecret, subsClient)
	pinAuthHandler.SetAuditService(auditSvc)
	// Order handler verifies manager step-up approval tokens with the same secret.
	orderHandler.SetTerminalSecret(terminalJWTSecret)
	// Payment handler verifies manager approval (step-up token or one-time code) for the
	// Complimentary/no-charge tender, and audits who approved it.
	paymentHandler.SetTerminalSecret(terminalJWTSecret)
	paymentHandler.SetAuditService(auditSvc)
	publicOutletHandler := handlers.NewPublicOutletHandler(log, entClient)

	// Retail module: layaway plans, weighing scale, purchase orders proxy
	// Staff fund-from-salary (premium): pos→erp S2S client + provisioner/settler.
	erpClient := erp.NewClient(cfg.ERP.ServiceURL, cfg.ERP.APIKey, cfg.ERP.RequestTimeout, log)
	staffCreditSvc := staffcredit.NewService(entClient, erpClient, eventPub, subsClient, log)
	paymentSvc.SetStaffCredit(staffCreditSvc) // staff credit sales route the debt to ERP payroll
	layawayHandler := handlers.NewLayawayHandler(log, entClient).WithStaffCredit(staffCreditSvc).WithFinalizer(paymentSvc).WithTreasuryClient(treasuryClient).
		// Branding + platform-owner footer for the printable layaway deposit/instalment slips.
		WithBranding(tenantCache, cfg.Auth.ServiceURL)
	scaleHandler := handlers.NewScaleHandler(log, entClient)

	appointmentHandler := handlers.NewAppointmentHandler(log, entClient)
	commissionHandler := handlers.NewCommissionHandler(log, entClient)
	staffScheduleHandler := handlers.NewStaffScheduleHandler(log, entClient)
	shiftOverrideHandler := handlers.NewStaffShiftOverrideHandler(log, entClient)
	leaveRequestHandler := handlers.NewLeaveRequestHandler(log, entClient)
	shiftRotationHandler := handlers.NewShiftRotationHandler(log, entClient)
	payrollHandler := handlers.NewPayrollHandler(log, entClient, treasuryClient)

	// Loyalty programs (Sprint 10)
	loyaltyHandler := handlers.NewLoyaltyHandler(log, entClient, mfClient)
	if pub := orderSvc.GetPublisher(); pub != nil {
		loyaltyHandler.SetPublisher(pub)
	}

	// Reports & Analytics (Sprint 11)
	reportsHandler := handlers.NewReportsHandler(log, entClient)
	// Wire the inventory S2S client so register-details can group products sold by brand.
	reportsHandler.SetInventoryClient(inventoryClient)

	// Branded, printable report documents (PDF/CSV) — reuses the report ent queries + tenant
	// branding cache, mirroring ReceiptHandler wiring.
	reportPDFHandler := handlers.NewReportPDFHandler(log, entClient, tenantCache, cfg.Auth.ServiceURL)
	// All-Sales export routed to a read replica when configured — see readEntClient above.
	reportPDFHandler.SetReadClient(readEntClient)

	// Webhook subscriptions (Sprint 12)
	webhookHandler := handlers.NewWebhookHandler(log, entClient)

	// Ordering-backend S2S client — used to DELEGATE delivery rider assignment to the
	// canonical admin endpoint (ordering-backend owns the order + rider-assignment flow).
	orderingS2SClient := orderingclient.NewClient(cfg.Ordering.ServiceURL, cfg.Ordering.APIKey, cfg.Ordering.RequestTimeout, log)

	// Online ordering pickup status + WS-D rider assignment (Sprint 13)
	onlineOrderHandler := handlers.NewOnlineOrderHandler(log, entClient)
	if pub := orderSvc.GetPublisher(); pub != nil {
		onlineOrderHandler.SetPublisher(pub)
	}
	// Wire WS-D deps: ordering S2S client (assign-rider delegation for ONLINE orders) + logistics
	// base URL/key (available-riders proxy) + logistics dispatch client (direct create-task/assign
	// for POS-NATIVE delivery orders). All use the shared INTERNAL_SERVICE_KEY.
	logisticsDispatch := logisticsclient.NewClient(cfg.Logistics.ServiceURL, cfg.Logistics.APIKey, cfg.Logistics.RequestTimeout)
	onlineOrderHandler.SetRiderDeps(orderingS2SClient, logisticsDispatch, cfg.Logistics.ServiceURL, cfg.Logistics.APIKey)

	// Platform admin: service configuration CRUD
	serviceConfigHandler := handlers.NewServiceConfigHandler(entClient, log)

	// Tenant/outlet POS settings (receipt, printer, module toggles, outlet switch)
	serviceSettingsHandler := handlers.NewServiceSettingsHandler(log, entClient)

	// Managed screensaver media (Settings → Display): uploads to the media volume,
	// urls stored in outlet-settings metadata, files served publicly at /media/*.
	mediaRoot := os.Getenv("MEDIA_ROOT")
	if mediaRoot == "" {
		mediaRoot = "./media"
	}
	screensaverMediaHandler := handlers.NewScreensaverMediaHandler(log, serviceSettingsHandler, mediaRoot)

	// ERP: daily closings + returns
	closingHandler := handlers.NewDailyClosingHandler(log, entClient)
	var returnEventPub *events.Publisher
	if pub := orderSvc.GetPublisher(); pub != nil {
		returnEventPub = pub
	}
	returnsSvc := returns.NewService(log, entClient, treasuryClient, returnEventPub)
	returnsSvc.SetAuditService(auditSvc)
	returnsSvc.WithSequence(docSeqSvc) // pos_return numbers via the document sequence
	// Exchange fulfilment creates the replacement order through the normal sale pipeline.
	returnsSvc.SetOrderService(orderSvc)
	// Branding + platform-owner footer for the printable refund receipt.
	returnHandler := handlers.NewReturnHandler(log, entClient, returnsSvc).WithBranding(tenantCache, cfg.Auth.ServiceURL)
	// Transaction reversals (platform-owner data-repair tool): orchestrates pos totals,
	// inventory consumption reversal, treasury GL and eTIMS credit note per reversal.
	reversalSvc := reversals.NewService(log, entClient, orderSvc, treasuryClient, inventoryClient)
	reversalSvc.SetAuditService(auditSvc)
	reversalSvc.WithSequence(docSeqSvc) // pos_reversal numbers via the document sequence
	reversalHandler := handlers.NewReversalHandler(log, reversalSvc)
	// Tenant-admin Delete-Sale ("shred") tool — built on the SAME reversal engine (reversalSvc)
	// for the fiscalised branch; the non-fiscalised branch additionally hard-deletes the treasury
	// ledger and pos-api's own rows. See internal/modules/saledelete.
	saleDeleteSvc := saledelete.NewService(log, entClient, reversalSvc, treasuryClient, inventoryClient)
	saleDeleteSvc.SetAuditService(auditSvc)
	saleDeleteHandler := handlers.NewSaleDeleteHandler(log, saleDeleteSvc)
	// Tenant-admin Edit-Sale tool. See internal/modules/saleedit: the centralized Edit
	// orchestrator branches non-fiscalized (true in-place, both reduce and increase) vs
	// fiscalized (return+credit-note for reductions, the existing linked-addendum-order
	// pattern for increases) — reuses reversalSvc/returnsSvc/orderSvc, no duplicate logic.
	saleEditSvc := saleedit.NewService(log, entClient, reversalSvc)
	saleEditSvc.SetAuditService(auditSvc)
	saleEditSvc.SetTreasuryClient(treasuryClient)
	saleEditSvc.SetInventoryClient(inventoryClient)
	saleEditSvc.SetOrderService(orderSvc)
	saleEditSvc.SetReturnsService(returnsSvc)
	saleEditHandler := handlers.NewSaleEditHandler(log, saleEditSvc)
	receiptHandler := handlers.NewReceiptHandler(log, entClient, tenantCache, cfg.Auth.ServiceURL)
	// KRA PIN header line on receipts — resolved from the treasury tax profile, printed
	// ONLY for eTIMS-activated tenants (FiscalPin returns "" otherwise). Fallback for sales
	// whose transmitted fiscal identity hasn't landed on the order yet.
	receiptHandler.SetFiscalPinResolver(taxResolver.FiscalPin)
	// Fiscal-identity backfill: pull the KRA TIMS details from treasury when the
	// etims.invoice_transmitted event was missed, before a receipt renders.
	receiptHandler.SetTreasuryClient(treasuryClient)
	receiptHandler.SetAuditService(auditSvc)
	// Customer account-balance line: key the lookup on the SAME CRM contact a credit sale/return
	// would resolve to, so the receipt reads the identical treasury CustomerBalance row.
	receiptHandler.SetCrmContactResolver(paymentSvc.ResolveCrmContactID)
	// Branded, printable customer menu document (public/tokenless — QR target). Reuses the
	// catalog assembly + tenant branding cache, mirroring ReceiptHandler wiring.
	menuHandler := handlers.NewMenuHandler(log, entClient, tenantCache, cfg.Auth.ServiceURL, catalogHandler)

	// Initialize RBAC
	rbacRepo := rbacmodule.NewEntRepository(entClient)
	rbacSvc := rbacmodule.NewService(rbacRepo, log)
	rbacHandler := handlers.NewRBACHandler(log, rbacSvc, rbacRepo, cfg.Auth.ServiceURL, cfg.Auth.APIKey)
	// Credit sale (sell on account) is gated in-handler on pos.orders.manage (same as approving sale
	// returns) because the tender is in the request body, not the route.
	paymentHandler.SetRBAC(rbacSvc)
	// Cost price is only serialized to callers with pos.catalog.view_cost (manager/admin).
	catalogHandler.SetRBAC(rbacSvc)
	// Per-cashier sales visibility (pos.orders.view_own) needs the RBAC fallback on order reads.
	orderHandler.SetRBAC(rbacSvc)
	// The All-Sales export applies the same per-cashier scoping as the order list.
	reportPDFHandler.SetRBAC(rbacSvc)
	// Dashboard KPI summary applies the same per-cashier scoping as the order list/export.
	reportsHandler.SetRBAC(rbacSvc)

	// Wire RBAC service into identity for JIT role assignment from JWT claims
	identitySvc.SetRBACService(rbacSvc)

	// Subscribe to auth-service user events for proactive user sync
	authEventHandler := identity.NewAuthEventHandler(entClient, identitySvc, log)
	if natsConn != nil {
		if err := authEventHandler.SubscribeToAuthEvents(natsConn); err != nil {
			log.Warn("app: failed to subscribe to auth user events", zap.Error(err))
		}
	}

	// Subscribe to auth.outlet.* NATS events — keeps local outlet mirror in sync
	authOutletHandler := identity.NewAuthOutletEventHandler(entClient, tenantSyncer, log)
	if natsConn != nil {
		if err := authOutletHandler.SubscribeToOutletEvents(natsConn); err != nil {
			log.Warn("app: failed to subscribe to auth outlet events", zap.Error(err))
		}
	}

	// Subscribe to ordering.order.confirmed — the SINGLE online-order ingestion path for
	// BOTH pickup (click-and-collect) and delivery. Idempotent via OrderLink; creates the
	// POSOrder + lines + OrderLink and routes KDS tickets to the correct stations.
	confirmedConsumer := ordermodule.NewConfirmedOrderConsumer(entClient, orderSvc, log)
	if natsConn != nil {
		if eventPub := orderSvc.GetPublisher(); eventPub != nil {
			confirmedConsumer.SetPublisher(eventPub)
		}
		if err := confirmedConsumer.SubscribeToConfirmedOrders(natsConn); err != nil {
			log.Warn("app: failed to subscribe to ordering.order.confirmed events", zap.Error(err))
		}
	}

	// Legacy ordering.order.for_pickup consumer — retained but now a no-op whenever an
	// OrderLink already exists (the confirmed consumer is authoritative). Kept subscribed
	// for backward compatibility with any still-publishing ordering-backend version.
	pickupConsumer := ordermodule.NewPickupConsumer(entClient, orderSvc, log)
	if natsConn != nil {
		if eventPub := orderSvc.GetPublisher(); eventPub != nil {
			pickupConsumer.SetPublisher(eventPub)
		}
		if err := pickupConsumer.SubscribeToPickupOrders(natsConn); err != nil {
			log.Warn("app: failed to subscribe to ordering click-and-collect events", zap.Error(err))
		}
	}

	// Wire KDS hub into order service and ordering subscriber so new tickets
	// broadcast immediately to connected KDS WebSocket clients.
	orderSvc.SetKDSHub(kdsHandler.Hub())

	// Subscribe to ordering.order.status.changed to create/update KDS tickets (Sprint 13)
	// consumerFeatureGate restricts cross-service data sync (KDS, stock availability,
	// treasury/eTIMS) to tenants entitled to the corresponding feature, mirroring the
	// HTTP-layer gating contract. Cached per tenant; fails open on subscriptions-api outage.
	consumerFeatureGate := func(ctx context.Context, tenantID, feature string) bool {
		return subsClient.ConsumerHasFeature(ctx, tenantID, feature)
	}

	kdsOrderingSubscriber := ordermodule.NewKDSOrderingSubscriber(entClient, log)
	kdsOrderingSubscriber.SetKDSHub(kdsHandler.Hub())
	kdsOrderingSubscriber.SetFeatureGate(consumerFeatureGate)
	if natsConn != nil {
		if eventPub := orderSvc.GetPublisher(); eventPub != nil {
			kdsOrderingSubscriber.SetPublisher(eventPub)
		}
		if err := kdsOrderingSubscriber.SubscribeToOrderingEvents(natsConn); err != nil {
			log.Warn("app: failed to subscribe to ordering status events for KDS", zap.Error(err))
		}
	}

	// Subscribe to treasury events: payment.success/failed → complete/fail local payment; etims → store invoice data
	treasurySubscriber := paymentmodule.NewTreasurySubscriber(entClient, paymentSvc, log)
	treasurySubscriber.SetFeatureGate(consumerFeatureGate)
	if natsConn != nil {
		if err := treasurySubscriber.SubscribeToTreasuryEvents(natsConn); err != nil {
			log.Warn("app: failed to subscribe to treasury events", zap.Error(err))
		}
	}

	// Invalidate cached treasury tax rates immediately when treasury changes a tax code,
	// instead of waiting for the 10-minute Redis TTL owned by the TaxResolver.
	taxSubscriber := ordermodule.NewTaxSubscriber(entClient, taxResolver, log)
	if natsConn != nil {
		if err := taxSubscriber.SubscribeToTaxEvents(natsConn); err != nil {
			log.Warn("app: failed to subscribe to treasury tax events", zap.Error(err))
		}
	}

	// Subscribe to inventory events for catalog projection sync + initial sync
	inventoryEventHandler := catalogmodule.NewInventoryEventHandler(entClient, sqlDB, redisClient, log)
	// Push a catalog-changed signal to every terminal on the tenant the instant inventory changes,
	// so they refresh via WS instead of waiting on the ~45s catalog-version poll.
	inventoryEventHandler.SetNotifHub(notificationsHandler.Hub())
	if natsConn != nil {
		if err := inventoryEventHandler.SubscribeToInventoryEvents(natsConn); err != nil {
			log.Warn("app: failed to subscribe to inventory events for catalog sync", zap.Error(err))
		}
	}

	// Invalidate tenant branding cache when subscription changes so new plan
	// is reflected in subsequent JWT-enriched responses without a restart.
	if natsConn != nil {
		subCacheSub := subscriptions.NewCacheSubscriber(redisClient, log)
		if err := subCacheSub.Start(natsConn); err != nil {
			log.Warn("app: failed to start subscription cache subscriber", zap.Error(err))
		}
	}

	// Subscribe to pos.sale.finalized: auto-earn loyalty points + create commission records + ERP pass-through
	if natsConn != nil {
		saleFinalizedSub := subscriptions.NewSaleFinalizedSubscriber(entClient, log, orderSvc.GetPublisher())
		if err := saleFinalizedSub.Start(natsConn); err != nil {
			log.Warn("app: failed to start sale.finalized subscriber", zap.Error(err))
		}
	}

	// Subscribe to inventory.stock.low → re-publish as pos.alert.stock_low for notifications-service
	if natsConn != nil {
		if eventPub := orderSvc.GetPublisher(); eventPub != nil {
			stockSub := events.NewStockSubscriber(eventPub, entClient, log)
			stockSub.SetFeatureGate(consumerFeatureGate)
			if err := stockSub.Subscribe(natsConn); err != nil {
				log.Warn("app: failed to subscribe to inventory.stock.low", zap.Error(err))
			}
		}
	}

	// Subscribe to erp.staff_purchase.recovered/reversed → pay down the staff layaway/credit as ERP
	// payroll recovers the debt (staff fund-from-salary settlement loop).
	if natsConn != nil {
		erpStaffSub := events.NewERPStaffPurchaseSubscriber(staffCreditSvc, log)
		if err := erpStaffSub.Subscribe(natsConn); err != nil {
			log.Warn("app: failed to subscribe to erp staff-purchase settlement events", zap.Error(err))
		}
	}

	// Subscribe to tenant.purge → IRREVERSIBLY delete all of a tenant's pos-api data when the
	// platform owner confirms a dormancy purge (subscriptions-api emits aggregate_type="tenant",
	// event_type="purge"). Guarded: only a confirmed dormancy purge with a valid tenant_id runs.
	if natsConn != nil {
		tenantPurgeSub := events.NewTenantPurgeSubscriber(sqlDB, log)
		if err := tenantPurgeSub.Subscribe(natsConn); err != nil {
			log.Warn("app: failed to subscribe to tenant.purge events", zap.Error(err))
		}
	}

	// Subscribe to crm.contact.merged → repoint pos-api's own crm_contact_id-bearing rows
	// (client_records, loyalty_accounts, appointments, bookings, room guests) when marketflow
	// folds a duplicate CRM contact into a survivor.
	if natsConn != nil {
		crmMergeSub := events.NewCRMContactMergedSubscriber(sqlDB, log)
		if err := crmMergeSub.Subscribe(natsConn); err != nil {
			log.Warn("app: failed to subscribe to crm.contact.merged events", zap.Error(err))
		}
	}

	// Webhook dispatcher: fan-out pos.> NATS events to matching webhook subscriptions with HTTP delivery + backoff
	if natsConn != nil {
		webhookDispatcher := webhookspkg.NewDispatcher(entClient, log)
		if err := webhookDispatcher.Start(natsConn); err != nil {
			log.Warn("app: failed to start webhook dispatcher", zap.Error(err))
		}
	}

	webhookWorker := webhookmodule.NewDeliveryWorker(entClient, log)
	shiftAutoEndWorker := shiftsmodule.NewAutoEndWorker(entClient, log)
	saleFinalizedReconciler := paymentmodule.NewSaleFinalizedReconciler(paymentSvc, log)
	treasuryIntentReconciler := paymentmodule.NewTreasuryIntentReconciler(paymentSvc, log)
	var layawayReminder *scheduler.LayawayReminderScheduler
	if eventPub := orderSvc.GetPublisher(); eventPub != nil {
		layawayReminder = scheduler.NewLayawayReminderScheduler(log, entClient, eventPub)
	}

	// Wire publisher into KDS handler for waiter notification event publishing
	if pub := orderSvc.GetPublisher(); pub != nil {
		kdsHandler.SetPublisher(pub)
	}

	billSplitHandler := handlers.NewBillSplitHandler(log.Named("bill-splits"), entClient)
	resourceHandler := handlers.NewResourceHandler(log, entClient)
	commissionRuleHandler := handlers.NewCommissionRuleHandler(log, entClient)
	packageHandler := handlers.NewPackageHandler(log, entClient)
	clientHandler := handlers.NewClientHandler(log, entClient)
	clientHandler.SetMarketFlowClient(mfClient)
	clientHandler.SetTreasuryClient(treasuryClient)
	channelHandler := handlers.NewChannelHandler(log, entClient)
	printHandler := handlers.NewPrintHandler(log, entClient)

	// Background print queue (AccuPOS model): orders/payments enqueue kitchen/bar/bill/receipt
	// jobs; the on-site Local Print Agent polls, claims and prints them. Postgres row locks
	// (FOR UPDATE SKIP LOCKED) keep claims multi-replica-safe.
	printQueue := printing.NewQueue(entClient, log.Named("print-queue"), true)
	// Real-time agent wake-up hub (push-with-poll-fallback): Enqueue nudges connected agents so a
	// job is claimed in ms instead of on the next poll; Redis relays the nudge across replicas (the
	// enqueue and the agent socket can live on different pods). Agents without a socket keep polling.
	printHub := printing.NewHub(log)
	printHub.SetRedis(redisClient)
	printQueue.SetHub(printHub)
	orderSvc.SetPrintQueue(printQueue)
	paymentSvc.SetPrintQueue(printQueue)
	printJobsHandler := handlers.NewPrintJobsHandler(log.Named("print-jobs"), entClient, printQueue)
	printAgentAPIHandler := handlers.NewPrintAgentAPIHandler(log.Named("print-agent-api"), printQueue)
	printAgentAPIHandler.SetHub(printHub)
	staffAdminHandler := handlers.NewStaffHandler(log.Named("staff-admin"), entClient, cfg.Auth.ServiceURL, cfg.Treasury.InternalServiceKey)
	// Repair / job-card module (device repair lifecycle: intake -> ... -> settled via POS)
	repairHandler := handlers.NewRepairHandler(log, entClient)
	repairHandler.WithSequence(docSeqSvc) // repair_job numbers via the document sequence

	// Settings → Documents: per-tenant document-numbering config API (numeric by default).
	docSequenceHandler := handlers.NewDocumentSequenceHandler(log, docSeqSvc)

	// Pluggable backup destination (OneDrive/GDrive/S3/WebDAV/SFTP/SMB) — encrypted
	// at rest with a SECRET_KEY-derived key. The handler owns the destination Store;
	// its Uploader is attached to the backup service so every PVC backup is
	// additionally mirrored best-effort (the PVC copy remains the durable primary).
	backupDestHandler := handlers.NewBackupDestinationHandler(entClient, log)

	// Tenant-scoped backups + daily 02:00 auto-backup scheduler + retention churn.
	backupSvc := backupmod.NewService(sqlDB, entClient, cfg.Backup.Dir, log).
		WithMirrorer(backupDestHandler.Uploader())
	backupHandler := handlers.NewBackupHandler(log, backupSvc, cfg.Backup.RetentionDays)
	backupmod.NewScheduler(backupSvc, backupmod.SchedulerConfig{
		Enabled:       cfg.Backup.ScheduleEnabled,
		Hour:          cfg.Backup.ScheduleHour,
		RetentionDays: cfg.Backup.RetentionDays,
	}, log).Start(ctx)

	// One-time recovery tool: fleet-wide recipe-COGS backfill (platform-owner only).
	recipeCOGSBackfillHandler := handlers.NewRecipeCOGSBackfillHandler(entClient, inventoryClient, treasuryClient, log)
	// One-time recovery tool: catalog-cost cache reconciliation (platform-owner only) — closes the
	// gross-profit cost-cache gap the same incident (ef9e882, 2026-08-10) left behind; see
	// pos_catalog_cost_backfill.go's doc comment.
	catalogCostBackfillHandler := handlers.NewCatalogCostBackfillHandler(entClient, inventoryClient, sqlDB, log)

	chiRouter := router.New(log, healthHandler, authMiddleware, entClient, identitySvc, orderHandler, catalogHandler, tableHandler, tenderHandler, paymentHandler, drawerHandler, barTabHandler, promotionHandler, rbacHandler, rbacSvc, hotelHandler, kdsHandler, deviceHandler, pinAuthHandler, publicOutletHandler, closingHandler, returnHandler, reversalHandler, saleDeleteHandler, saleEditHandler, receiptHandler, menuHandler, layawayHandler, scaleHandler, appointmentHandler, commissionHandler, staffScheduleHandler, shiftOverrideHandler, leaveRequestHandler, shiftRotationHandler, loyaltyHandler, reportsHandler, reportPDFHandler, webhookHandler, onlineOrderHandler, serviceConfigHandler, serviceSettingsHandler, docSequenceHandler, notificationsHandler, queueHandler, billSplitHandler, resourceHandler, commissionRuleHandler, packageHandler, clientHandler, channelHandler, printHandler, printJobsHandler, printAgentAPIHandler, payrollHandler, staffAdminHandler, repairHandler, cfg.HTTP.AllowedOrigins, redisClient, cfg.Treasury.InternalServiceKey, backupHandler, backupDestHandler, screensaverMediaHandler, mediaRoot, recipeCOGSBackfillHandler, catalogCostBackfillHandler)

	httpServer := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.HTTP.Host, cfg.HTTP.Port),
		Handler:           chiRouter,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
	}

	return &App{
		cfg:                      cfg,
		log:                      log,
		httpServer:               httpServer,
		db:                       dbPool,
		entClient:                entClient,
		cache:                    redisClient,
		events:                   natsConn,
		outboxPublisher:          outboxPub,
		webhookWorker:            webhookWorker,
		shiftAutoEndWorker:       shiftAutoEndWorker,
		saleFinalizedReconciler:  saleFinalizedReconciler,
		treasuryIntentReconciler: treasuryIntentReconciler,
		kdsHub:                   kdsHub,
		printHub:                 printHub,
		notifHub:                 notifHub,
		layawayReminderScheduler: layawayReminder,
	}, nil
}

func (a *App) Run(ctx context.Context) error {
	// Start outbox background publisher for POS events
	if a.outboxPublisher != nil {
		go func() {
			if err := a.outboxPublisher.Start(ctx); err != nil {
				a.log.Error("outbox publisher stopped", zap.Error(err))
			}
		}()
		a.log.Info("outbox background publisher started")
	}

	// Start webhook delivery worker — polls pending deliveries every 10s
	if a.webhookWorker != nil {
		go a.webhookWorker.Start(ctx)
		a.log.Info("webhook delivery worker started")
	}

	// Start shift auto-end worker — closes overdue shift sessions every 15 min
	if a.shiftAutoEndWorker != nil {
		go a.shiftAutoEndWorker.Start(ctx)
	}

	// Start sale.finalized reconciler — republishes completed orders left without a live
	// outbox row (crash-window gap or an exhausted-retry FAILED row) every 5 min
	if a.saleFinalizedReconciler != nil {
		go a.saleFinalizedReconciler.Start(ctx)
	}

	// Start treasury-intent reconciler — retries the deferred cash/manual payment-intent create
	// (see pos-sale-close-async-fanout.md follow-up) for any payment left without one every 2 min
	if a.treasuryIntentReconciler != nil {
		go a.treasuryIntentReconciler.Start(ctx)
	}

	// Start KDS/print/notification hub Redis pub/sub relays — no-op if Redis is not configured
	go a.kdsHub.Start(ctx)
	go a.printHub.Start(ctx)
	go a.notifHub.Start(ctx)

	// Start layaway payment-due reminder scheduler — fires once at startup then every 24h
	if a.layawayReminderScheduler != nil {
		go a.layawayReminderScheduler.Start(ctx)
	}

	a.log.Info("pos service starting", zap.String("addr", a.httpServer.Addr))

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := a.httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}

		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("http server error: %w", err)
	}
}

func (a *App) Close() {
	if a.events != nil {
		if err := a.events.Drain(); err != nil {
			a.log.Warn("nats drain failed", zap.Error(err))
		}
		a.events.Close()
	}

	if a.cache != nil {
		if err := a.cache.Close(); err != nil {
			a.log.Warn("redis close failed", zap.Error(err))
		}
	}

	if a.entClient != nil {
		if err := a.entClient.Close(); err != nil {
			a.log.Warn("ent close failed", zap.Error(err))
		}
	}

	if a.db != nil {
		a.db.Close()
	}

	_ = a.log.Sync()
}
