package router

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"go.uber.org/zap"

	"github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	handlers "github.com/bengobox/pos-service/internal/http/handlers"
	outletmw "github.com/bengobox/pos-service/internal/http/middleware"
	"github.com/bengobox/pos-service/internal/modules/identity"
	rbacmodule "github.com/bengobox/pos-service/internal/modules/rbac"
	"github.com/bengobox/pos-service/internal/platform/subscriptions"
)

// bypassForWebsocket wraps a middleware so it never runs on a WebSocket upgrade request — used
// for TWO independent hijack-breaking middlewares found live during E2E verification of this
// session's new WS routes:
//  1. chi's middleware.Compress: compressResponseWriter.Hijack() type-asserts its wrapped writer
//     directly instead of walking an http.ResponseController Unwrap() chain.
//  2. httpware.Logging (github.com/Bengo-Hub/httpware, shared fleet-wide): its status-capturing
//     responseWriter embeds the http.ResponseWriter INTERFACE (not a concrete type), so Go only
//     promotes that interface's own three methods (Header/Write/WriteHeader) — Hijack is never
//     promoted regardless of what the underlying writer supports. This is a PRE-EXISTING bug in
//     the shared httpware module, unrelated to this session's changes: every WS route in this API
//     (notifications, KDS, print-agent) has silently never been able to hijack through Logging,
//     which is why print-agent's real-time wake-up socket ALWAYS fell back to its 10s poll loop
//     without functional impact (poll-fallback = fully correct, just slower) — the same class of
//     bug this session's new notification stream hit, minus a working fallback for a fresh push.
//     Proper fix belongs in httpware itself (a shared module, out of scope here); this local
//     bypass is the safe, scoped workaround. RFC 6455 upgrade requests always carry
//     Connection: Upgrade and Upgrade: websocket, so detecting them here is exact, not a heuristic.
func bypassForWebsocket(mw func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		wrapped := mw(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				next.ServeHTTP(w, r)
				return
			}
			wrapped.ServeHTTP(w, r)
		})
	}
}

func New(
	log *zap.Logger,
	health *handlers.HealthHandler,
	authMiddleware *authclient.AuthMiddleware,
	entClient *ent.Client,
	idSvc *identity.Service,
	orders *handlers.POSOrderHandler,
	catalog *handlers.CatalogHandler,
	tables *handlers.TableHandler,
	tenders *handlers.TenderHandler,
	payments *handlers.PaymentHandler,
	drawers *handlers.DrawerHandler,
	barTabs *handlers.BarTabHandler,
	promotions *handlers.PromotionHandler,
	rbacHandler *handlers.RBACHandler,
	rbacSvc *rbacmodule.Service,
	hotel *handlers.HotelHandler,
	kds *handlers.KDSHandler,
	devices *handlers.DeviceHandler,
	pinAuth *handlers.PINAuthHandler,
	publicOutlet *handlers.PublicOutletHandler,
	closings *handlers.DailyClosingHandler,
	returns *handlers.ReturnHandler,
	reversalsH *handlers.ReversalHandler,
	saleDeleteH *handlers.SaleDeleteHandler,
	saleEditH *handlers.SaleEditHandler,
	receipt *handlers.ReceiptHandler,
	menu *handlers.MenuHandler,
	layaway *handlers.LayawayHandler,
	scale *handlers.ScaleHandler,
	appointments *handlers.AppointmentHandler,
	commissions *handlers.CommissionHandler,
	staffSchedule *handlers.StaffScheduleHandler,
	shiftOverrides *handlers.StaffShiftOverrideHandler,
	leaveRequests *handlers.LeaveRequestHandler,
	shiftRotations *handlers.ShiftRotationHandler,
	loyalty *handlers.LoyaltyHandler,
	reports *handlers.ReportsHandler,
	reportPDF *handlers.ReportPDFHandler,
	webhooks *handlers.WebhookHandler,
	onlineOrders *handlers.OnlineOrderHandler,
	serviceConfig *handlers.ServiceConfigHandler,
	serviceSettings *handlers.ServiceSettingsHandler,
	docSequences *handlers.DocumentSequenceHandler,
	notifications *handlers.NotificationsHandler,
	queue *handlers.QueueHandler,
	billSplits *handlers.BillSplitHandler,
	resources *handlers.ResourceHandler,
	commissionRules *handlers.CommissionRuleHandler,
	packages *handlers.PackageHandler,
	clients *handlers.ClientHandler,
	channels *handlers.ChannelHandler,
	print *handlers.PrintHandler,
	printJobs *handlers.PrintJobsHandler,
	printAgentAPI *handlers.PrintAgentAPIHandler,
	payroll *handlers.PayrollHandler,
	staffAdmin *handlers.StaffHandler,
	repairs *handlers.RepairHandler,
	allowedOrigins []string,
	redisClient *redis.Client,
	internalServiceKey string,
	backups *handlers.BackupHandler,
	backupDest *handlers.BackupDestinationHandler,
	screensaverMedia *handlers.ScreensaverMediaHandler,
	mediaRoot string,
	recipeCOGSBackfill *handlers.RecipeCOGSBackfillHandler,
	catalogCostBackfill *handlers.CatalogCostBackfillHandler,
) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	// CORS must run BEFORE the rate limiter (and other early-exit middleware) so that even a 429 /
	// 401 / timeout response still carries Access-Control-Allow-* headers — otherwise the browser
	// masks the real status as an opaque CORS error. RealIP stays above so the limiter keys on the
	// true client IP. go-chi/cors also short-circuits OPTIONS preflight here, before rate limiting.
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   allowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "Origin", "X-Request-ID", "X-Tenant-ID", "X-Tenant-Slug", "X-Outlet-ID", "X-API-Key", "Idempotency-Key"},
		ExposedHeaders:   []string{"Link", "X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "Retry-After", "Idempotent-Replayed"},
		AllowCredentials: true,
		MaxAge:           300,
	}))
	r.Use(httpware.RequestID)
	// bypassForWebsocket: see its doc comment — httpware.Logging's wrapper structurally cannot
	// support Hijack (a pre-existing fleet-wide bug), which breaks every WS upgrade in this API.
	r.Use(bypassForWebsocket(httpware.Logging(log)))
	r.Use(httpware.Recover(log))
	// gzip the JSON responses (catalog lists, order detail w/ lines+payments) — the largest
	// payloads on this API had no compression at any layer (confirmed: the devops-k8s ingress-nginx
	// gzip ConfigMap exists but isn't wired into any ArgoCD Application). Skips already-compressed
	// types (images/pdf/zip) automatically. bypassForWebsocket is REQUIRED: chi's
	// compressResponseWriter.Hijack() does a raw type-assertion on its wrapped writer rather than
	// walking an http.ResponseController Unwrap() chain, so wrapping a WebSocket upgrade request in
	// it breaks nhooyr.io/websocket's Accept() hijack fleet-wide (notifications/KDS/print-agent
	// streams) with "http.Hijacker is unavailable on the writer" — confirmed live via kubectl logs
	// during E2E verification (2026-08-07).
	r.Use(bypassForWebsocket(middleware.Compress(5)))
	// bypassForWebsocket here too: chi's Timeout fires its own abort/WriteHeader on ITS timer
	// regardless of whether the connection was since hijacked for a long-lived WS stream, forcibly
	// disconnecting every open WS connection roughly every 30s ("http: response.WriteHeader on
	// hijacked connection" — confirmed live via kubectl logs during E2E verification). A 30s
	// request timeout is meaningless for a stream that's SUPPOSED to stay open indefinitely.
	r.Use(bypassForWebsocket(middleware.Timeout(30 * time.Second)))
	r.Use(middleware.RequestSize(10 << 20)) // 10 MB max body size
	r.Use(outletmw.IPRateLimit(redisClient, log, outletmw.DefaultRateLimitConfig()))

	r.Get("/healthz", health.Liveness)
	r.Get("/readyz", health.Readiness)
	r.Get("/metrics", health.Metrics)
	r.Get("/v1/docs/*", handlers.SwaggerUI)

	// Public read-only media (managed screensavers). Files are admin-uploaded display
	// assets rendered on the pre-auth PIN screen, so no auth; traversal-guarded.
	if mediaRoot != "" {
		r.Get("/media/*", handlers.ServeMedia(mediaRoot))
	}

	r.Route("/api/v1", func(api chi.Router) {
		// Ã¢â€â‚¬Ã¢â€â‚¬ Platform admin endpoints (platform owner JWT required) Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬
		if serviceConfig != nil && authMiddleware != nil {
			api.Group(func(admin chi.Router) {
				admin.Use(authMiddleware.RequireAuth)
				// Platform-wide + per-tenant-override service config - platform-owner only.
				// ServiceConfigHandler's own doc comment says the caller must apply this gate;
				// it wasn't wired here (found in a 2026-09-06 pricing/tiering audit) even
				// though every sibling platform-admin route below already does.
				admin.Use(requirePlatformOwner)
				serviceConfig.RegisterAdminRoutes(admin)

				// Platform-default backup destination (OneDrive/GDrive/S3/WebDAV/
				// SFTP/SMB) — platform-owner only. Secret params encrypted at rest.
				if backupDest != nil || recipeCOGSBackfill != nil || catalogCostBackfill != nil {
					admin.Group(func(platform chi.Router) {
						platform.Use(requirePlatformOwner)
						if backupDest != nil {
							backupDest.RegisterPlatformRoutes(platform)
						}
						// One-time recipe-COGS backfill (see pos_cogs_backfill.go) — fleet-wide,
						// platform-owner only, same gate as the backup-destination routes above.
						if recipeCOGSBackfill != nil {
							recipeCOGSBackfill.RegisterRoutes(platform)
						}
						// One-time catalog-cost cache reconciliation (see
						// pos_catalog_cost_backfill.go) — same gate, tenant-scoped or fleet-wide.
						if catalogCostBackfill != nil {
							catalogCostBackfill.RegisterRoutes(platform)
						}
					})
				}
			})
		}

		// Ã¢â€â‚¬Ã¢â€â‚¬ Public endpoints (no auth required) Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬
		// These routes are accessible before the staff member has authenticated.
		// TenantV2 extracts tenant UUID directly from the URL path parameter.
		api.Get("/openapi.json", handlers.OpenAPIJSON)

		// Public download for the Local Print Agent installer (no auth, no tenant — a generic,
		// credential-free binary). 302-redirects to the GitHub release asset.
		api.Get("/pos/print-agent/download", handlers.PrintAgentDownload)

		// Public receipt-share routes — no auth, no tenant_id in the URL. The order's own
		// public_token capability column IS the auth (mirrors treasury-api's /api/v1/public
		// document links). Registered here, ahead of the tenantID-scoped `pub`/`prot` blocks
		// below, so a WhatsApp/Email/SMS "download your receipt" link never needs a JWT.
		if receipt != nil {
			api.Route("/public", func(pubDoc chi.Router) {
				receipt.RegisterPublicRoutes(pubDoc)
			})

			// Short receipt-download link (/api/v1/r/{code}) — same public_token capability model
			// as above, just base58-encoded to a ~22-char code instead of the full
			// /public/receipts/{uuid}/pdf?download=true path, for a link that looks less alarming
			// in a WhatsApp/SMS message. No new secret/entropy: it's the SAME public_token, so the
			// long-form route above keeps serving any already-sent historical links unchanged.
			api.Route("/r", func(short chi.Router) {
				receipt.RegisterShortReceiptRoute(short)
			})
		}

		// Local Print Agent job polling (AccuPOS-style spooler). The agent lives on the shop LAN
		// and polls OUT; auth is its pairing key (X-Agent-Key), not a user JWT — hence outside the
		// tenant JWT group. Long-poll claim + ack.
		if printAgentAPI != nil {
			api.Get("/pos/printing/agent/jobs", printAgentAPI.NextJob)
			api.Post("/pos/printing/agent/jobs/{jobID}/ack", printAgentAPI.AckJob)
			// Real-time wake-up socket (push-with-poll-fallback): agent claims the instant a job is
			// enqueued instead of on its next poll. Same X-Agent-Key auth as the poll/ack routes.
			api.Get("/pos/printing/agent/ws", printAgentAPI.StreamAgent)
		}

		api.Group(func(pub chi.Router) {
			pub.Use(httpware.TenantV2(httpware.TenantConfig{
				URLParamFunc: chi.URLParam,
				URLParamName: "tenantID",
				Required:     true,
			}))
			if pinAuth != nil {
				pub.Get("/{tenantID}/pos/staff", pinAuth.ListStaff)
				// Dedicated, much stricter per-IP limit on the actual PIN-guess surface (bcrypt
				// compare against a candidate set), stacked on top of the general limiter above —
				// see PINRateLimit's doc comment for why (PIN uniqueness is per-tenant only, so a
				// common/guessed PIN can collide across unrelated tenants' admin accounts).
				pub.Group(func(pinGroup chi.Router) {
					pinGroup.Use(outletmw.PINRateLimit(redisClient, log, outletmw.DefaultPINRateLimitConfig()))
					pinGroup.Post("/{tenantID}/pos/auth/pin", pinAuth.Login)
					pinGroup.Post("/{tenantID}/pos/auth/pin/identify", pinAuth.IdentifyByPIN)
					pinGroup.Post("/{tenantID}/pos/auth/pin/step-up", pinAuth.StepUp)
					pinGroup.Post("/{tenantID}/pos/auth/pin/step-up-card", pinAuth.StepUpByCard)
				})
				pub.Get("/{tenantID}/pos/auth/pin/profile", pinAuth.StaffProfiles)
			}
			if publicOutlet != nil {
				pub.Get("/{tenantID}/pos/outlets", publicOutlet.ListPublicOutlets)
				pub.Get("/{tenantID}/pos/outlets/current", publicOutlet.GetCurrentOutlet)
			}
			// Branded printable customer menu document (tokenless so the QR code target opens
			// in any browser). Regenerated on every request → always reflects the live catalog.
			if menu != nil {
				pub.Get("/{tenantID}/pos/outlets/{outletID}/menu.html", menu.GetMenuHTML)
				// True-PDF variant (same tokenless data path) for DocPreview + sharing.
				pub.Get("/{tenantID}/pos/outlets/{outletID}/menu.pdf", menu.GetMenuPDF)
			}
			// Public reservation endpoints Ã¢â‚¬â€ used by the embeddable booking widget
			if tables != nil {
				pub.Get("/{tenantID}/pos/reservations/available", tables.GetAvailableSlots)
				pub.Post("/{tenantID}/pos/reservations", tables.CreateReservation)
			}
			// Payment-gateway init proxy. Called by the embedded treasury/Paystack payment UI (a
			// cross-origin "Books" iframe) which does NOT carry the POS user's JWT — so it must be
			// public. It only forwards the server-issued intent_id to treasury (treasury validates the
			// intent), so the intent_id is the capability; requiring POS auth here 401s the handoff.
			if payments != nil {
				pub.Post("/{tenantID}/pos/payments/initiate", payments.ProxyInitiate)
			}
		})

		// Ã¢â€â‚¬Ã¢â€â‚¬ Protected endpoints (auth required) Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬
		// RequireAnyAuth accepts both SSO JWTs and HMAC terminal JWTs from PIN login.
		api.Group(func(prot chi.Router) {
			if pinAuth != nil {
				prot.Use(pinAuth.RequireAnyAuth(authMiddleware))
			} else if authMiddleware != nil {
				prot.Use(authMiddleware.RequireAuth)
			}
			if authMiddleware != nil {
				prot.Use(subscriptions.SubscriptionGate())
			}

			if idSvc != nil {
				prot.Use(func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						claims, ok := authclient.ClaimsFromContext(r.Context())
						if ok && claims.Subject != "" {
							subject, _ := uuid.Parse(claims.Subject)
							slug := claims.GetTenantSlug()
							if slug != "" {
								_, err := idSvc.EnsureUserFromToken(r.Context(), subject, slug, map[string]any{
									"email":             claims.Email,
									"roles":             claims.Roles,
									"permissions":       claims.Permissions,
									"is_platform_owner": claims.IsPlatformOwner,
									"outlet_id":         claims.GetOutletID(),
								})
								if err != nil {
									log.Warn("jit provisioning failed", zap.Error(err))
								}
							}
						}
						next.ServeHTTP(w, r)
					})
				})
			}

			prot.Route("/{tenantID}", func(tenant chi.Router) {
				tenant.Use(httpware.TenantV2(httpware.TenantConfig{
					ClaimsExtractor: func(ctx context.Context) (tenantID, tenantSlug string, isPlatformOwner bool, ok bool) {
						claims, found := authclient.ClaimsFromContext(ctx)
						if !found {
							return "", "", false, false
						}
						return claims.TenantID, claims.GetTenantSlug(), claims.IsPlatformOwner, true
					},
					URLParamFunc: chi.URLParam,
					URLParamName: "tenantID",
					Required:     true,
				}))
				tenant.Use(outletmw.OutletContextMiddleware(entClient, log))

				// RBAC routes — role/permission management. Gated so only admins/managers
				// (pos.users.view for reads, pos.users.manage for mutations via the in-handler
				// canManageRBAC checks) can enumerate or edit the role model. Previously these
				// were authenticated-only, letting any tenant user grant themselves any role.
				if rbacHandler != nil {
					tenant.Group(func(rg chi.Router) {
						rg.Use(outletmw.RequireServicePermission(rbacSvc, "pos.users.view", "pos.users.manage"))
						rbacHandler.RegisterRoutes(rg)
					})
				}

				// Outlet settings + TruLoad-inspired outlet switch
				if serviceSettings != nil {
					serviceSettings.RegisterRoutes(tenant)
				}

				// Per-tenant document numbering (Settings → Documents): numeric-by-default POS
				// order/receipt/return/reversal/repair-job numbers, per-tenant configurable. Reads
				// are tenant-scoped; the mutating PUT is config-gated like other POS settings.
				if docSequences != nil {
					docSequences.RegisterRoutes(tenant,
						outletmw.RequireServicePermission(rbacSvc, "pos.config.change", "pos.config.manage"))
				}

				// Tenant-scoped backups (this tenant's data only) — config/admin-gated.
				if backups != nil {
					tenant.Group(func(bg chi.Router) {
						bg.Use(outletmw.RequireServicePermission(rbacSvc, "pos.config.change", "pos.config.manage"))
						backups.RegisterRoutes(bg)
					})
				}

				// Per-tenant backup-destination override (mirrors backups off the
				// PVC) — same config permission gate as the tenant backups routes.
				if backupDest != nil {
					tenant.Group(func(dg chi.Router) {
						dg.Use(outletmw.RequireServicePermission(rbacSvc, "pos.config.change", "pos.config.manage"))
						backupDest.RegisterRoutes(dg)
					})
				}

				tenant.Route("/pos", func(pos chi.Router) {
					// Replay-safety for the offline-sync worker: a request carrying an
					// Idempotency-Key (the offline local_id) is executed once and its response
					// stored, so reconnect retries never duplicate sales/payments/voids/returns.
					// No-op for normal online traffic, which sends no key.
					pos.Use(outletmw.Idempotency(entClient))

					// Managed screensaver media (Settings → Display) — list/upload/delete.
					// Permission enforced inside the handlers (pos.config.change/manage).
					if screensaverMedia != nil {
						screensaverMedia.RegisterRoutes(pos)
					}

					// Orders
					if orders != nil {
						// Reads require an orders permission; the handlers additionally narrow
						// view_own-only principals (cashiers) to their OWN sales (REQ-007).
						orderRead := outletmw.RequireServicePermission(rbacSvc,
							"pos.orders.view", "pos.orders.view_own", "pos.orders.change", "pos.orders.manage")
						// Creating/mutating a bill requires an order-write permission (cashier/waiter
						// hold pos.orders.add). Previously ungated — any authenticated outlet user
						// could POST an order.
						orderWrite := outletmw.RequireServicePermission(rbacSvc,
							"pos.orders.add", "pos.orders.change", "pos.orders.change_own", "pos.orders.manage")
						pos.With(orderRead).Get("/orders", orders.ListOrders)
						// Totals footer for the All-Sales / POS-Sales list: aggregates the full
						// filtered set (all pages), not just the visible page. Same read gate + filters.
						pos.With(orderRead).Get("/orders/summary", orders.OrdersSummary)
						pos.With(orderWrite).Post("/orders", orders.CreateOrder)
						pos.With(orderRead).Get("/orders/by-number/{orderNumber}", orders.GetOrderByNumber)
						pos.With(orderRead).Get("/orders/{orderID}", orders.GetOrder)
						// Closing / completing / cancelling an order (the bill-clear action). Gated so a
						// principal must hold an order-write permission; a super-waiter granted
						// pos.orders.view can see + close others' bills, a plain waiter only their own.
						pos.With(orderWrite).Patch("/orders/{orderID}/status", orders.UpdateStatus)
						// All-Sales "Edit Shipping": update shipping status/address/charges (metadata).
						pos.Patch("/orders/{orderID}/shipping", orders.UpdateShipping)
						// All-Sales "New Sale Notification": (re)send the customer their receipt/invoice.
						pos.Post("/orders/{orderID}/notify", orders.NotifySale)
						// "Share via WhatsApp" wa.me quick action: resolves the durable public receipt
						// link client-side (no notifications-service round-trip for this path).
						pos.Get("/orders/{orderID}/receipt/share-link", orders.GetReceiptShareLink)
						// pos.orders.void lets a cashier INITIATE a void; the handler itself then
						// requires manager approval (pos.orders.void_self / an override role /
						// a live approval token or code) unless the caller already holds
						// manager-level authority — see VoidOrder's mandatory-approval gate.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.void", "pos.orders.manage")).
							Patch("/orders/{orderID}/void", orders.VoidOrder)
						// Delete a DRAFT (saved-but-unpaid) sale. Route-gated to an order-write
						// permission OR the dedicated pos.orders.delete_own (a custom role can hold
						// delete_own without add/change_own); DeleteDraft then enforces the RBAC
						// boundary server-side — pos.orders.manage deletes ANY draft, any other
						// caller needs delete_own AND must own the draft (or a tenant admin may have
						// hidden delete_own outlet-wide for non-manager callers via the OutletSetting
						// quick config). Only draft-status orders are deletable (finalized sales must
						// be voided/returned so the ledger + eTIMS stay consistent).
						draftDeleteWrite := outletmw.RequireServicePermission(rbacSvc,
							"pos.orders.add", "pos.orders.change", "pos.orders.change_own",
							"pos.orders.manage", "pos.orders.delete_own")
						pos.With(draftDeleteWrite).Delete("/orders/{orderID}", orders.DeleteDraft)
						// Bulk draft delete — same middleware + per-order rules as the single
						// DeleteDraft above (manage deletes any draft, others only their own with
						// delete_own); missing/ineligible ids are reported as skipped, never errors.
						pos.With(draftDeleteWrite).Post("/orders/bulk-delete", orders.BulkDeleteDrafts)
						// Bulk void — a back-office manager action, so it is route-gated to
						// pos.orders.manage (no terminal step-up token: the caller IS the
						// manager). Per-order eligibility mirrors the single void exactly
						// (already-voided → skipped, finalized sales must go via return/refund).
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")).
							Post("/orders/bulk-void", orders.BulkVoidOrders)
						// Manager generates a one-time code (shareable) to authorize voiding this
						// order when they're not at the terminal. Manager-only (handler re-checks role).
						pos.Post("/orders/{orderID}/void-code", orders.GenerateVoidCode)
						// Manager generates a one-time code (shareable) to authorize closing this
						// bill via the Complimentary/no-charge tender when they're not at the
						// terminal. Manager-only (handler re-checks role).
						pos.Post("/orders/{orderID}/complimentary-code", orders.GenerateComplimentaryCode)
						// Generic (non-order-scoped) manager approval codes for pre-order actions
						// (over-limit discount / price override / order adjustment / out-of-stock
						// override). Generate is manager-only (handler re-checks the override role);
						// verify consumes a code for the client-side out-of-stock gate.
						pos.Post("/approval-codes", orders.GenerateActionApprovalCode)
						pos.Post("/approval-codes/verify", orders.VerifyActionApprovalCode)
						pos.Post("/orders/{orderID}/fire-course", orders.FireCourse)
						pos.With(orderWrite).Post("/orders/{orderID}/lines", orders.AddOrderLines)
						// Same gate as the whole-order void above — cashier may initiate, the
						// handler requires manager approval unless the caller already has it.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.void", "pos.orders.manage")).
							Post("/orders/{orderID}/lines/{lineID}/void", orders.VoidOrderLine)
						// Manager/admin corrective tool: directly edit a persisted line's price/qty
						// instead of requiring a raw database fix for stale-priced sales.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")).
							Patch("/orders/{orderID}/lines/{lineID}", orders.EditOrderLine)
						// Manager/admin corrective tool: set an unsettled order's order-level discount
						// in place (recomputes totals) so a resumed sale never settles at a stale
						// pre-discount total (root cause of the 2026-07-14 duplicate-settle incident).
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")).
							Patch("/orders/{orderID}/discount", orders.SetOrderDiscount)
						// Admin/platform-owner-only corrective tool: move a settled sale's reporting
						// date (e.g. a sale rung up and synced a day late) without touching amounts,
						// payments, or the immutable created_at audit timestamp. pos.orders.manage is
						// the outer gate (defense in depth); MoveOrderDate itself further restricts to
						// the tenant's admin/owner tier — a plain manager holding pos.orders.manage is
						// NOT enough (see dateMoveAdminRoles in orders_date_move.go).
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")).
							Patch("/orders/{orderID}/date", orders.MoveOrderDate)
						// Admin/manager correction tool: who served a sale + the customer on file
						// (draft, open, OR completed — never voided/cancelled/refunded). Narrower
						// scope than MoveOrderDate's route+service double-gate — pos.orders.manage
						// alone is sufficient here (see orders.Service.UpdateSaleInfo).
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")).
							Patch("/orders/{orderID}/sale-info", orders.UpdateSaleInfo)
						// Tenant-admin Delete-Sale ("shred") tool for a FINALIZED sale — admin-only by
						// default (carved out of manager's pos.orders.* wildcard in the seed),
						// tenant-configurable via the Roles & Permissions matrix. POST, not DELETE:
						// DELETE /orders/{orderID} is already taken by DeleteDraft (draft-status sales
						// only, 409s on anything finalized) — this is a distinct capability for a
						// SETTLED sale, not a route override. Distinct from the platform-owner-only
						// /reversals routes below: this is a routine business action a tenant admin
						// can take on their own sale, not a platform data-repair tool. See
						// saledelete.Service for the fiscalised-vs-hard-delete branching.
						if saleDeleteH != nil {
							pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.delete")).
								Post("/orders/{orderID}/delete", saleDeleteH.DeleteSale)
						}
						// Tenant-admin Edit-Sale tool — admin-only by default (carved out of manager's
						// pos.orders.* wildcard), tenant-configurable. Caller sends the full desired
						// line set; the orchestrator diffs server-side and routes in-place vs return
						// vs addendum internally based on the order's actual fiscalization status.
						if saleEditH != nil {
							pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.edit_finalized")).
								Post("/orders/{orderID}/edit", saleEditH.Edit)
						}
						// Upsell / set-aside: hold a wrongly-ordered (already-made) item for resale
						// instead of voiding it. No manager approval; must be cleared before shift close.
						pos.Post("/orders/{orderID}/lines/{lineID}/set-aside", orders.SetAsideLine)
						pos.Get("/held-items", orders.ListHeldItems)
						pos.Post("/held-items/{id}/claim", orders.ClaimHeldItem)
						pos.Post("/held-items/{id}/void", orders.VoidHeldItem)
						pos.Post("/orders/{orderID}/lines/{lineID}/serials", orders.CaptureSerial)
						// Bulk import of HISTORICAL sales (migration) — idempotent on external_ref,
						// no stock/loyalty/GL side effects (see sales_import.go). Manager/admin only.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")).
							Post("/sales/import", orders.ImportSales)
					}
					if print != nil {
						pos.Post("/orders/{orderID}/print", print.PrintReceipt)
					}

					// In-app notifications (waiter order-ready alerts + real-time WS stream)
					if notifications != nil {
						pos.Get("/notifications", notifications.List)
						pos.Get("/notifications/stream", notifications.StreamNotifications)
						pos.Post("/notifications/mark-all-read", notifications.MarkAllRead)
						pos.Patch("/notifications/{id}/read", notifications.MarkRead)
					}

					// Receipt
					if receipt != nil {
						pos.Get("/orders/{orderID}/receipt", receipt.GetReceipt)
						pos.Get("/orders/{orderID}/receipt/html", receipt.GetReceiptHTML)
						pos.Get("/orders/{orderID}/receipt/pdf", receipt.GetReceiptPDF)
						pos.Post("/orders/{orderID}/receipt/reprint", receipt.ReprintReceipt)
					}

					// QZ Tray printing bridge — serve the platform cert + sign print requests so the
					// pos-ui can print silently to assigned printers. Stateless (env-driven key/cert).
					pos.Get("/printing/qz/cert", handlers.QZCert)
					pos.Post("/printing/qz/sign", handlers.QZSign)
					// Server-side LAN printer discovery (mDNS/scan/SNMP). Env-gated — only useful for
					// on-prem pos-api on the same network as the terminals; the pos-ui tries this first
					// then falls back to the local QZ Tray / WebUSB / Bluetooth bridges.
					pos.Get("/printing/discover", handlers.PrinterDiscover)
					// Build a diagnostic test ticket (ESC/POS hex) for the printer-setup "Test print"
					// button, so a network printer prints silently via the local agent/QZ rather than
					// opening the browser print dialog. Stateless — no order required.
					pos.Post("/printing/test-ticket", handlers.TestTicket)

					// Background print queue (AccuPOS model): explicit job enqueue for Print
					// Bill/Receipt/Test-print buttons + Local Print Agent pairing/status.
					if printJobs != nil {
						pos.Post("/printing/jobs", printJobs.EnqueueJob)
						pos.Get("/printing/jobs/status", printJobs.JobsStatus)
						pos.Get("/printing/agents", printJobs.ListAgents)
						pos.Post("/printing/agents", printJobs.PairAgent)
						pos.Delete("/printing/agents/{agentID}", printJobs.RevokeAgent)
					}

					// Catalog
					if catalog != nil {
						pos.Route("/catalog", func(cat chi.Router) {
							cat.Get("/version", catalog.GetCatalogVersion)
							cat.Get("/categories", catalog.GetCatalogCategories)
							cat.Get("/brands", catalog.GetBrands)
							cat.Get("/items", catalog.ListCatalogItems)
							cat.Get("/pricing/resolve", catalog.ResolvePrice)
							cat.Get("/pricing/tiers", catalog.GetPricingTiers)
							cat.Post("/items", catalog.CreateCatalogItem)
							// Price management endpoints (must come before /{id} routes)
							cat.Patch("/items/prices", catalog.SetCatalogItemPrice)
							cat.Post("/items/prices/bulk", catalog.BulkSetCatalogPrices)
							// KDS station assignment — the priority-1 explicit per-item routing override.
							cat.Patch("/items/kds-station", catalog.SetCatalogItemKDSStation)
							cat.Get("/items/{id}", catalog.GetCatalogItem)
							cat.Put("/items/{id}", catalog.UpdateCatalogItem)
							cat.Delete("/items/{id}", catalog.DeleteCatalogItem)
							cat.Get("/items/{id}/stock", catalog.GetItemStock)
							cat.Get("/barcode/{barcode}", catalog.BarcodeLookup)
						})
					}

					// Sections & Tables Ã¢â‚¬â€ hospitality only
					if tables != nil {
						pos.Group(func(tbl chi.Router) {
							tbl.Use(outletmw.RequireUseCase("hospitality"))
							// NOT gated on subscriptions.RequireFeature(FeatureTableManagement): every hospitality
							// plan tier already includes "table_management" (subscriptions-api cmd/seed/
							// plans_pos_lines.go), so the gate was pure redundant surface for a JWT/plan-sync
							// failure to break core checkout actions on — notably SplitOrder/SetServiceCharge
							// below, which have nothing to do with the "Table & Floor Management" add-on the
							// code represents. RequireUseCase("hospitality") above is the real gate here.
							tbl.Get("/sections", tables.ListSections)
							tbl.Post("/sections", tables.CreateSection)
							tbl.Put("/sections/{id}", tables.UpdateSection)
							tbl.Delete("/sections/{id}", tables.DeleteSection)
							tbl.Get("/tables", tables.ListTables)
							tbl.Post("/tables", tables.CreateTable)
							tbl.Put("/tables/{id}", tables.UpdateTable)
							tbl.Delete("/tables/{id}", tables.DeleteTable)
							// Operational floor actions (clear/assign/transfer/merge/split a table &
							// its bill) require a table-change permission — previously ungated (any
							// hospitality user could clear/transfer another waiter's table).
							tablesChange := outletmw.RequireServicePermission(rbacSvc,
								"pos.tables.change", "pos.tables.change_own", "pos.tables.manage")
							tbl.With(tablesChange).Patch("/tables/{id}/status", tables.UpdateTableStatus)
							tbl.With(tablesChange).Post("/tables/{id}/assign", tables.AssignTable)
							tbl.With(tablesChange).Post("/tables/{id}/release", tables.ReleaseTable)
							tbl.With(tablesChange).Post("/tables/{id}/transfer", tables.TransferTable)
							tbl.With(tablesChange).Post("/tables/merge", tables.MergeTables)
							tbl.With(tablesChange).Post("/tables/unmerge", tables.UnmergeTables)
							// Order split + service charge live here (use TableHandler, need nil guard)
							tbl.With(tablesChange).Post("/orders/{orderID}/split", tables.SplitOrder)
							tbl.With(tablesChange).Patch("/orders/{orderID}/service-charge", tables.SetServiceCharge)
							// Reservations (staff-managed)
							tbl.Get("/reservations", tables.ListReservations)
							tbl.Get("/reservations/available", tables.GetAvailableSlots)
							tbl.Get("/reservations/{id}", tables.GetReservation)
							tbl.Post("/reservations", tables.CreateReservation)
							tbl.Patch("/reservations/{id}", tables.UpdateReservation)
							tbl.Post("/reservations/{id}/confirm", tables.ConfirmReservation)
							tbl.Post("/reservations/{id}/check-in", tables.CheckInReservation)
							tbl.Post("/reservations/{id}/cancel", tables.CancelReservation)
						})
					}

					// Tenders
					if tenders != nil {
						pos.Get("/tenders", tenders.ListTenders)
						pos.Post("/tenders", tenders.CreateTender)
						pos.Put("/tenders/{id}", tenders.UpdateTender)
					}

					// Payments
					if payments != nil {
						pos.Get("/gateways", payments.GetGateways)
						// Currency: proxied from treasury's centralized conversion service —
						// backs the outlet-currency picker + currency-change confirmation modal.
						pos.Get("/currency/currencies", payments.GetSupportedCurrencies)
						pos.Get("/currency/convert", payments.ConvertCurrency)
						pos.Post("/expenses", payments.RecordExpense)
						// Dropdown data for the Add-Expense form (proxied from treasury).
						pos.Get("/expenses/categories", payments.ListExpenseCategories)
						pos.Get("/expenses/accounts", payments.ListExpenseAccounts)
						// "+ Create account" inline action (shared-ui-lib AccountForm) on the same dropdown.
						pos.Post("/expenses/accounts", payments.CreateExpenseAccount)
						// Supplier/vendor search-select for the "Expense for" field (proxied from inventory-api).
						pos.Get("/expenses/suppliers", payments.ListExpenseSuppliers)
						// Live "Reference No" preview from treasury's document-sequence service.
						pos.Get("/expenses/next-number", payments.PreviewExpenseNumber)
						// Treasury-sourced tax codes/rates for the Settings → Tax tab (read-only).
						pos.Get("/tax-codes", payments.ListTaxCodes)
						pos.Get("/c2b/payments", payments.ListC2BCandidates)
						pos.Post("/c2b/payments/{transID}/claim", payments.ClaimC2BPayment)
						pos.Post("/c2b/simulate", payments.SimulateC2BPayment)
						// Recording a payment (cash/M-Pesa ref) or opening a payment intent is a
						// money-movement action Ã¢â‚¬â€ gate on payments.add (cashier, waiter, manager+).
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.payments.add", "pos.payments.manage")).
							Post("/orders/{orderID}/payments/intent", payments.CreatePaymentIntent)
						// Settle an on-account (credit) sale: collected row + treasury AR receipt.
						// Separate from the intent path — the finalized sale's GL/stock never re-post.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.payments.add", "pos.payments.manage")).
							Post("/orders/{orderID}/payments/settle-credit", payments.SettleCreditPayment)
						// Put a partially-paid (or unpaid) sale's outstanding balance on account:
						// books the remainder to the customer's treasury AR + finalizes the sale, so
						// the debtor becomes visible + collectible on the treasury Customers page.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.payments.add", "pos.payments.manage")).
							Post("/orders/{orderID}/close-on-account", payments.CloseOnAccount)
						// Treasury→POS AR data-heal: settles a customer's open POS credit orders down to
						// treasury's live authoritative balance (reduce-only, FIFO). Platform-owner only —
						// this is a fleet/tenant data-repair tool, not a cashier action; the automatic
						// per-event reconcile (treasury_balance_subscriber.go) covers the steady state.
						pos.Group(func(rc chi.Router) {
							rc.Use(requirePlatformOwner)
							rc.Post("/ar/reconcile", payments.ReconcileAR)
						})
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.payments.add", "pos.payments.manage")).
							Post("/orders/{orderID}/payments", payments.RecordPayment)
						pos.With(outletmw.RequireServicePermission(rbacSvc,
							"pos.payments.view", "pos.payments.view_own", "pos.payments.manage")).
							Get("/orders/{orderID}/payments", payments.ListOrderPayments)
						// View-Payments modal actions — manager-only. Edit touches descriptive
						// fields only (never the amount); delete is a soft VOID (paid_total
						// recompute + treasury reversal); notify sends the customer a
						// payment-received confirmation.
						paymentsManage := outletmw.RequireServicePermission(rbacSvc, "pos.payments.manage")
						pos.With(paymentsManage).
							Patch("/orders/{orderID}/payments/{paymentID}", payments.UpdateOrderPayment)
						pos.With(paymentsManage).
							Delete("/orders/{orderID}/payments/{paymentID}", payments.VoidOrderPayment)
						pos.With(paymentsManage).
							Post("/orders/{orderID}/payments/{paymentID}/notify", payments.NotifyOrderPayment)
						pos.Get("/orders/{orderID}/payment-status/stream", payments.StreamPaymentStatus)
						// Bank list + account verification (proxied to treasury S2S Paystack) for the
						// receipt payment-display bank settings.
						pos.Get("/banks/{country}", payments.ListBanks)
						pos.Get("/banks/resolve", payments.ResolveBankAccount)
						// Cheap one-shot status check the pos-ui polls with bounded backoff (replaces the
						// SSE stream's reconnect storm). Rate-limit-exempt; NATS subscriber owns truth.
						pos.Get("/orders/{orderID}/payment-status", payments.GetPaymentStatus)
						// NOTE: POST /payments/initiate is registered in the PUBLIC group — the embedded
						// cross-origin Paystack iframe calls it without the POS user's JWT (intent_id is
						// the capability; treasury validates it). Keeping it here would 401 the handoff.
						// "Save as Quotation" forwards a pos cart to treasury (treasury owns quotations).
						// Quotations are a manager/back-office action (same permission as approving sale
						// returns) — an ordinary cashier must not raise them.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")).
							Post("/quotations", payments.CreateQuotationFromCart)
						// Quotation transactions tab — proxies the treasury quotation list.
						pos.Get("/quotations", payments.ListQuotationsProxy)
						// Full quotation sync (get/put/patch + send/accept/decline/cancel) — the
						// SAME treasury S2S handlers treasury-ui manages the document through.
						// Mutations keep the manager gate the create carries.
						pos.Get("/quotations/{quotationID}", payments.GetQuotationProxy)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")).
							Put("/quotations/{quotationID}", payments.UpdateQuotationProxy)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")).
							Patch("/quotations/{quotationID}", payments.UpdateQuotationProxy)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")).
							Post("/quotations/{quotationID}/{action}", payments.QuotationActionProxy)
					}

					// Cash Drawers
					if drawers != nil {
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.cash_drawers.add", "pos.cash_drawers.manage")).
							Post("/drawers/open", drawers.OpenDrawer)
						pos.Get("/drawers/current", drawers.GetCurrentDrawer)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.cash_drawers.manage")).
							Post("/drawers/{id}/close", drawers.CloseDrawer)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.cash_drawers.add", "pos.cash_drawers.manage")).
							Post("/drawers/{id}/movement", drawers.RecordMovement)
						pos.Get("/drawers/{id}/events", drawers.ListDrawerEvents)
						pos.Get("/drawers", drawers.ListDrawerHistory)
					}

					// Bar Tabs Ã¢â‚¬â€ hospitality only
					if barTabs != nil {
						pos.Group(func(bt chi.Router) {
							bt.Use(outletmw.RequireUseCase("hospitality"))
							bt.Post("/bar-tabs", barTabs.OpenBarTab)
							bt.Get("/bar-tabs", barTabs.ListBarTabs)
							bt.Get("/bar-tabs/{id}", barTabs.GetBarTab)
							bt.Post("/bar-tabs/{id}/close", barTabs.CloseBarTab)
						})
					}

					// Promotions
					if promotions != nil {
						pos.Get("/promotions", promotions.ListPromotions)
						pos.Get("/promotions/{promoID}", promotions.GetPromotion)
						// Creating/editing/deleting a promotion/happy-hour is administrative;
						// applying a code at checkout is part of the cashier order flow and stays
						// ungated.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.promotions.add", "pos.promotions.manage")).
							Post("/promotions", promotions.CreatePromotion)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.promotions.manage")).
							Patch("/promotions/{promoID}", promotions.UpdatePromotion)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.promotions.manage")).
							Delete("/promotions/{promoID}", promotions.DeletePromotion)
						pos.Post("/promotions/apply", promotions.ApplyPromoCode)
						pos.Get("/promotions/happy-hour/active", promotions.GetActiveHappyHours)
					}

					// Device sessions (shift open/close = clock-in / clock-out).
					// Opening/closing a register session is a staff action gated on
					// pos.sessions.add (managers/finance also satisfy via pos.sessions.manage),
					// mirroring the cash-drawer open/close gating above. GET reads stay
					// auth-only so every signed-in staffer can see their own shift status.
					if devices != nil {
						pos.Get("/devices", devices.ListDevices)
						pos.Get("/devices/current/sessions/current", devices.GetCurrentSession)
						pos.Get("/devices/current/sessions/current/summary", devices.GetSessionSummary)
						pos.Get("/devices/current/sessions/history", devices.GetSessionHistory)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.sessions.add", "pos.sessions.manage")).
							Post("/devices/current/sessions/open", devices.OpenSession)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.sessions.add", "pos.sessions.manage")).
							Post("/devices/current/sessions/close", devices.CloseSession)
					}

					// Terminal PIN auth (auth-protected endpoints)
					// SetPIN requires a manager/admin SSO token Ã¢â‚¬â€ no subscription gate so admins
					// can always set staff PINs regardless of plan.
					// AuthMe requires SSO token for Trinity Layer 3.
					// ListStaff / Login / StaffProfiles are registered in the public group above.
					if pinAuth != nil {
						// SetPIN + card-token set/reset ANOTHER staff member's PIN / mint a
						// manager override card — manager/admin only, now enforced server-side.
						// AuthMe stays open: it only returns the CALLER's own role + permissions.
						staffPinManage := outletmw.RequireServicePermission(rbacSvc, "pos.users.manage", "pos.staff.manage")
						pos.With(staffPinManage).Post("/auth/pin/set", pinAuth.SetPIN)
						pos.With(staffPinManage).Post("/staff/{userID}/card-token", pinAuth.IssueStaffCardToken)
						pos.Get("/auth/me", pinAuth.AuthMe)
					}

					// Staff admin CRUD (requires STAFF_MANAGE permission Ã¢â‚¬â€ enforced client-side;
					// server-side role boundary enforced in the handler itself).
					if staffAdmin != nil {
						// Server-side permission gate (was "enforced client-side"): reads need
						// users/staff view, mutations need users/staff manage. The in-handler
						// role boundary additionally stops a manager creating/editing admin staff.
						staffView := outletmw.RequireServicePermission(rbacSvc,
							"pos.users.view", "pos.users.manage", "pos.staff.view", "pos.staff.manage")
						staffManage := outletmw.RequireServicePermission(rbacSvc,
							"pos.users.manage", "pos.staff.manage")
						pos.With(staffView).Get("/staff/admin", staffAdmin.ListStaffForAdmin)
						pos.With(staffManage).Post("/staff", staffAdmin.CreateStaff)
						pos.With(staffManage).Patch("/staff/{staffID}", staffAdmin.UpdateStaff)
						pos.With(staffManage).Post("/staff/{staffID}/deactivate", staffAdmin.DeactivateStaff)
					}

					// KDS Ã¢â‚¬â€ hospitality and quick_service only; outlet must have enable_kds=true
					if kds != nil {
						pos.Group(func(k chi.Router) {
							k.Use(outletmw.RequireUseCase("hospitality", "quick_service"))
							k.Use(outletmw.RequireKDSEnabled(entClient))
							k.Use(subscriptions.RequireFeature(subscriptions.FeatureKDS))
							k.Get("/kds/stations", kds.ListStations)
							k.Post("/kds/stations", kds.CreateStation)
							k.Put("/kds/stations/{id}", kds.UpdateStation)
							k.Delete("/kds/stations/{id}", kds.DeleteStation)
							k.Get("/kds/stream", kds.StreamKDS)
							k.Get("/kds/kitchen", kds.GetKitchenQueue)
							k.Get("/kds/bar", kds.GetBarQueue)
							k.Get("/kds/tickets", kds.ListTickets)
							k.Post("/kds/tickets/{id}/start", kds.StartTicket)
							k.Post("/kds/tickets/{id}/ready", kds.ReadyTicket)
							k.Post("/kds/tickets/{id}/serve", kds.ServeTicket)
							k.Post("/kds/tickets/{id}/void", kds.VoidTicket)
							k.Post("/kds/tickets/{id}/call-waiter", kds.CallWaiter)
							// Bulk-clear the board (serve all active tickets) — manager-only. Lets a
							// single terminal clear a cluttered board (stale never-bumped tickets).
							k.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")).
								Post("/kds/tickets/clear", kds.ClearTickets)
						})
					}

					// Returns
					if returns != nil {
						// Initiate a return — a cashier action.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.change_own", "pos.orders.change", "pos.orders.manage")).
							Post("/orders/{orderID}/returns", returns.CreateReturn)
						// Bill splitting
						if billSplits != nil {
							pos.Get("/orders/{orderID}/splits", billSplits.ListSplits)
							pos.Post("/orders/{orderID}/splits", billSplits.CreateSplits)
							// Settling a split records a payment Ã¢â‚¬â€ gate on payments.add like other tender flows.
							pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.payments.add", "pos.payments.manage")).
								Post("/orders/{orderID}/splits/{splitID}/settle", billSplits.SettleSplit)
						}
						pos.Get("/returns", returns.ListReturns)
						pos.Get("/returns/{returnID}", returns.GetReturn)
						// Printable refund receipt (?format=json|html|pdf) — the same shared
						// receipt template as a sale, flagged as a return.
						pos.Get("/returns/{returnID}/receipt", returns.GetReturnReceipt)
						// Transaction reversals — the platform-owner data-repair tool for
						// FINALIZED sales (whole order or items), orchestrating pos totals,
						// inventory consumption, treasury GL and the eTIMS credit note with a
						// tracked per-step ledger (sync-monitor "Txn Reversals" tab). A tenant
						// superuser is NOT enough: this rewrites money records on tenant request.
						if reversalsH != nil {
							pos.Group(func(rv chi.Router) {
								rv.Use(requirePlatformOwner)
								rv.Post("/reversals", reversalsH.CreateReversal)
								rv.Get("/reversals", reversalsH.ListReversals)
								rv.Get("/reversals/{reversalID}", reversalsH.GetReversal)
								rv.Post("/reversals/{reversalID}/retry", reversalsH.RetryReversal)
							})
						}
						// Approval / rejection is a manager decision.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")).
							Patch("/returns/{returnID}/approve", returns.ApproveReturn)
						// Completion (money-out + inventory restock) is done at the till by a cashier/manager.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.change_own", "pos.orders.change", "pos.orders.manage")).
							Post("/returns/{returnID}/complete", returns.CompleteReturn)
					}

					// Layaway plans & payments. Gated with the SAME order/payment permission codes
					// every seeded role already carries (no new codes → no prod seeder run):
					// create/read for till roles, money + terminal transitions for managers.
					if layaway != nil {
						// Mutations also require the layaway plan feature (retail T2+ per the
						// use-case PowerSuite matrix); reads stay open per the
						// feature-gated-READS-pass convention.
						layawayFeat := subscriptions.RequireFeature(subscriptions.FeatureLayaway)
						layawayRead := outletmw.RequireServicePermission(rbacSvc,
							"pos.orders.view", "pos.orders.view_own", "pos.orders.manage")
						pos.With(layawayFeat, outletmw.RequireServicePermission(rbacSvc, "pos.orders.add", "pos.orders.manage")).
							Post("/layaways", layaway.Create)
						pos.With(layawayRead).Get("/layaways", layaway.List)
						// Staff fund-from-salary links (admin/reconcile view).
						pos.With(layawayRead).Get("/staff-credit", layaway.ListStaffCredit)
						pos.With(layawayRead).Get("/layaways/{id}", layaway.Get)
						// Printable layaway slips (?format=json|html|pdf) — the deposit taken at
						// create time and each recorded instalment both predate the POSOrder a
						// completion creates, so the sale receipt endpoint can't serve them.
						pos.With(layawayRead).Get("/layaways/{id}/receipt", layaway.GetLayawayReceipt)
						pos.With(layawayRead).Get("/layaways/{id}/payments/{paymentID}/receipt", layaway.GetLayawayPaymentReceipt)
						pos.With(layawayFeat, outletmw.RequireServicePermission(rbacSvc, "pos.payments.add", "pos.payments.manage")).
							Post("/layaways/{id}/payments", layaway.RecordPayment)
						layawayManage := outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")
						pos.With(layawayFeat, layawayManage).Post("/layaways/{id}/cancel", layaway.Cancel)
						pos.With(layawayFeat, layawayManage).Post("/layaways/{id}/forfeit", layaway.Forfeit)
						pos.With(layawayFeat, layawayManage).Post("/layaways/{id}/complete", layaway.Complete)
					}

					// Purchase orders live in inventory-api/inventory-ui (the owning service);
					// the old POS proxy duplicate was removed per the use-case PowerSuite specs.

					// Weighing scale readings
					if scale != nil {
						pos.Post("/scale/readings", scale.Create)
						pos.Get("/scale/readings", scale.List)
					}

					// Appointments & staff schedules Ã¢â‚¬â€ services use_case
					if appointments != nil {
						pos.Group(func(svc chi.Router) {
							svc.Use(outletmw.RequireUseCase("services"))
							apptChange := outletmw.RequireServicePermission(rbacSvc, "pos.appointments.change", "pos.appointments.manage")
							svc.Get("/appointments", appointments.List)
							svc.With(outletmw.RequireServicePermission(rbacSvc, "pos.appointments.add", "pos.appointments.manage")).
								Post("/appointments", appointments.Create)
							svc.Get("/appointments/availability", appointments.Availability)
							svc.Get("/appointments/{appointmentID}", appointments.Get)
							svc.With(apptChange).Put("/appointments/{appointmentID}", appointments.Update)
							svc.With(apptChange).Post("/appointments/{appointmentID}/check-in", appointments.CheckIn)
							svc.With(apptChange).Post("/appointments/{appointmentID}/start", appointments.Start)
							svc.With(apptChange).Post("/appointments/{appointmentID}/complete", appointments.Complete)
							svc.With(apptChange).Post("/appointments/{appointmentID}/cancel", appointments.Cancel)
							svc.With(apptChange).Post("/appointments/{appointmentID}/no-show", appointments.NoShow)
						})
					}

					// Walk-in queue Ã¢â‚¬â€ services use_case
					if queue != nil {
						pos.Group(func(svc chi.Router) {
							svc.Use(outletmw.RequireUseCase("services"))
							svc.Get("/queue", queue.List)
							svc.Post("/queue/entries", queue.Create)
							svc.Patch("/queue/entries/{entryID}/status", queue.UpdateStatus)
							svc.Post("/queue/entries/{entryID}/assign", queue.AssignStaff)
						})
					}

					// Resources Ã¢â‚¬â€ services use_case (chairs, rooms, equipment)
					if resources != nil {
						pos.Group(func(svc chi.Router) {
							svc.Use(outletmw.RequireUseCase("services"))
							svc.Get("/resources", resources.List)
							svc.Post("/resources", resources.Create)
							svc.Patch("/resources/{resourceID}", resources.PatchStatus)
						})
					}

					// Staff schedules + overrides + leave
					if staffSchedule != nil {
						pos.Get("/staff/{staffID}/schedule", staffSchedule.ListSchedule)
						pos.Put("/staff/{staffID}/schedule", staffSchedule.UpsertSchedule)
					}
					if shiftOverrides != nil {
						pos.Get("/staff/overrides", shiftOverrides.ListAllOverrides)
						pos.Get("/staff/{staffID}/overrides", shiftOverrides.ListStaffOverrides)
						pos.Post("/staff/{staffID}/overrides", shiftOverrides.CreateOverride)
						pos.Delete("/staff/{staffID}/overrides/{overrideID}", shiftOverrides.DeleteOverride)
					}
					if leaveRequests != nil {
						pos.Get("/leave-requests", leaveRequests.ListLeaveRequests)
						pos.Get("/staff/{staffID}/leave-requests", leaveRequests.ListStaffLeaveRequests)
						pos.Post("/staff/{staffID}/leave-requests", leaveRequests.CreateLeaveRequest)
						pos.Patch("/leave-requests/{leaveID}/status", leaveRequests.UpdateLeaveStatus)
					}
					if shiftRotations != nil {
						pos.Get("/shift-rotations", shiftRotations.ListRotations)
						pos.Post("/shift-rotations", shiftRotations.CreateRotation)
						pos.Get("/shift-rotations/{rotationID}", shiftRotations.GetRotation)
						pos.Patch("/shift-rotations/{rotationID}", shiftRotations.UpdateRotation)
						pos.Put("/shift-rotations/{rotationID}/slots", shiftRotations.UpsertSlots)
					}

					// Payroll & advances
					if payroll != nil {
						pos.Post("/staff/{staffID}/advances", payroll.CreateAdvance)
						pos.Get("/staff/{staffID}/advances", payroll.ListAdvances)
						pos.Post("/payroll/generate", payroll.GeneratePayroll)
						pos.Get("/payroll/{payrollID}", payroll.GetPayroll)
						pos.Post("/payroll/{payrollID}/approve", payroll.ApprovePayroll)
						pos.Post("/payroll/{payrollID}/disburse", payroll.DisbursePayroll)
					}

					// Commissions (records + rules & payout). Commissions are a retail/services
					// concept (salespeople, therapists) — NOT hospitality/QSR/pharmacy. Gate the
					// whole surface by use case so cross-use-case data never mixes (matches the
					// pos-ui module map which only lists commissions for retail/services).
					if commissions != nil || commissionRules != nil {
						pos.Group(func(cm chi.Router) {
							cm.Use(outletmw.RequireUseCase("retail", "services"))
							cm.Use(subscriptions.RequireFeature(subscriptions.FeatureCommissions))
							if commissions != nil {
								cm.Get("/commissions", commissions.List)
								cm.Get("/commissions/{commissionID}", commissions.Get)
							}
							if commissionRules != nil {
								cm.Get("/commissions/rules", commissionRules.List)
								cm.Post("/commissions/rules", commissionRules.Create)
								cm.Patch("/commissions/rules/{ruleID}", commissionRules.Update)
								cm.Post("/commissions/payout", commissionRules.Payout)
							}
						})
					}

					// Service packages
					if packages != nil {
						pos.Group(func(svc chi.Router) {
							svc.Use(outletmw.RequireUseCase("services"))
							svc.Get("/packages", packages.ListPackages)
							svc.Post("/packages", packages.CreatePackage)
							svc.Post("/packages/{packageID}/sell", packages.SellPackage)
							svc.Get("/packages/purchases", packages.ListPurchases)
							svc.Post("/packages/purchases/{purchaseID}/redeem", packages.RedeemSession)
						})
					}

					// Client records. The customer DIRECTORY + credit-terms EDITING are centralized
					// on the treasury Customers page (pos-ui's Clients nav links there) — the old
					// /customers CRM-directory endpoint and PUT credit editor were removed with it.
					if clients != nil {
						pos.Get("/clients", clients.List)
						pos.Post("/clients", clients.CreateOrUpsert)
						pos.Post("/clients/bulk-import", clients.BulkImport)
						pos.Get("/clients/{clientID}", clients.Get)
						pos.Patch("/clients/{clientID}", clients.Update)
						pos.Get("/clients/{phone}/orders", clients.GetOrdersByPhone)
						// Credit terms (treasury AR proxy, READ-ONLY here): balance + limit + period
						// for the checkout credit hint; terms are edited on the treasury Customers page.
						pos.Get("/clients/{accountID}/credit", clients.GetCredit)
						// Identifier-based variant (no POS LoyaltyAccount required) — most customers have
						// no loyalty account yet, but treasury's AR/CustomerBalance is keyed by
						// crm_contact_id/phone regardless of loyalty membership, so a customer attached
						// via a CRM-only search match can still show their real balance.
						pos.Get("/clients/credit", clients.GetCreditByIdentifier)
						// Stand-alone pay-out of a customer's existing stored credit (independent of any
						// return/sale) — proxied to treasury the same way GetCredit is.
						pos.Post("/clients/{accountID}/payout-credit", clients.PayoutCredit)
					}

					// Loyalty programs & accounts — gated on the loyalty_program feature
					// (bundles include it from Starter; POS-device plans do not).
					if loyalty != nil {
						pos.Group(func(ly chi.Router) {
							// Gate by use case (matches the pos-ui module map) in addition to the plan feature.
							ly.Use(outletmw.RequireUseCase("retail", "services"))
							ly.Use(subscriptions.RequireFeature(subscriptions.FeatureLoyalty))
							ly.Get("/loyalty/programs", loyalty.ListPrograms)
							ly.Post("/loyalty/programs", loyalty.CreateProgram)
							ly.Put("/loyalty/programs/{programID}", loyalty.UpdateProgram)
							ly.Get("/loyalty/accounts", loyalty.ListAccounts)
							ly.Post("/loyalty/accounts", loyalty.CreateAccount)
							ly.Get("/loyalty/accounts/{accountID}", loyalty.GetAccount)
							ly.Post("/loyalty/accounts/{accountID}/earn", loyalty.Earn)
							ly.Post("/loyalty/accounts/{accountID}/redeem", loyalty.Redeem)
							ly.Post("/loyalty/accounts/{accountID}/redeem-to-order", loyalty.RedeemToOrder)
							ly.Post("/loyalty/accounts/{accountID}/referrals", loyalty.CreateReferral)
							ly.Get("/loyalty/accounts/{accountID}/referrals", loyalty.ListReferrals)
						})
					}

					// Reports & Analytics — gated on pos.reports.view/manage (cashier holds
					// reports.view; the per-cashier own-sales scoping on the All-Sales export
					// remains the data authority within that gate).
					reportsView := outletmw.RequireServicePermission(rbacSvc, "pos.reports.view", "pos.reports.manage")
					if reports != nil {
						pos.With(reportsView).Group(func(rp chi.Router) {
							rp.Get("/reports/summary", reports.GetSummary)
							rp.Get("/reports/audit-logs", reports.ListAuditLogs)
							rp.Get("/reports/exceptions", reports.Exceptions)
							rp.Get("/reports/sales-summary", reports.SalesSummary)
							rp.Get("/reports/refund-summary", reports.RefundSummary)
							rp.Get("/reports/daily-breakdown", reports.DailyBreakdown)
							rp.Get("/reports/top-items", reports.TopItems)
							rp.Get("/reports/register-details", reports.RegisterDetails)
							rp.Get("/reports/sales-by-staff", reports.SalesByStaff)
							rp.Get("/reports/export", reports.ExportDailyReport)
							// Sprint 11: additional report endpoints
							rp.Get("/reports/shifts", reports.ShiftReportList)
							rp.Get("/reports/shifts/{sessionID}", reports.ShiftReport)
							rp.Get("/reports/commissions", reports.CommissionReport)
							rp.Get("/reports/tax", reports.TaxReport)
							// Hyphenated (matching every other report route + the pos-ui hooks, which
							// have always requested these two exact paths) — NOT the nested
							// "/sales/by-hour" form these two used to register under, which the
							// frontend never called and 404'd unconditionally.
							rp.Get("/reports/sales-by-hour", reports.SalesByHour)
							rp.Get("/reports/sales-by-category", reports.SalesByCategory)
							rp.Get("/reports/stock-consumption", reports.StockConsumptionReport)
							rp.Get("/reports/returns", reports.ReturnsSummary)
							rp.Get("/reports/void-summary", reports.VoidSummary)
							rp.Get("/reports/product-mix", reports.ProductMix)
							rp.Get("/reports/most-profitable", reports.MostProfitableItems)
							// KDS Station is a kitchen concept — hospitality/quick_service only (mirrors
							// pos-ui's isKitchen gate in the analytics/EOD pages). Without this the JSON
							// route had no use-case check at all — any retail/services/pharmacy outlet
							// with reports.view could call it directly and get real-but-meaningless
							// station data back, unlike every other use-case-scoped route group (see the
							// loyalty routes above) which already enforces RequireUseCase.
							rp.With(outletmw.RequireUseCase("hospitality", "quick_service")).
								Get("/reports/sales/by-kds-station", reports.SalesByKDSStation)
							// Occupancy %, ADR, RevPAR, room-vs-ancillary revenue split — hotel-module
							// tenants only (rooms/RoomGuest/RoomFolioItem are meaningless without it),
							// same double gate the /hotel group itself uses (RequireUseCase +
							// FeatureHotelModule) rather than the broader hospitality/quick_service set
							// above, which also covers restaurants/bars with no rooms at all.
							rp.With(outletmw.RequireUseCase("hospitality")).
								With(subscriptions.RequireFeature(subscriptions.FeatureHotelModule)).
								Get("/reports/hotel-occupancy", reports.HotelOccupancyReport)
						})
					}

					// Branded report documents (PDF/CSV via ?format=) — reset summary, item-type,
					// daily sales, shift X, staff, tax and profitability reports. Same reports gate.
					if reportPDF != nil {
						pos.With(reportsView).Group(func(rp chi.Router) {
							rp.Get("/reports/reset-summary", reportPDF.ResetSummary)
							rp.Get("/reports/sales-by-item-type", reportPDF.SalesByItemType)
							// Same use-case gate as the JSON route above — a retail/pharmacy outlet
							// should never be able to export a "Sales by KDS Station" PDF/CSV either.
							rp.With(outletmw.RequireUseCase("hospitality", "quick_service")).
								Get("/reports/sales-by-kds-station-document", reportPDF.SalesByKDSStationDoc)
							rp.Get("/reports/daily-sales", reportPDF.DailySales)
							rp.Get("/reports/shift/{sessionID}", reportPDF.ShiftReportPDF)
							rp.Get("/reports/staff", reportPDF.SalesByStaffPDF)
							rp.Get("/reports/tax-document", reportPDF.TaxReportPDF)
							rp.Get("/reports/most-profitable-document", reportPDF.MostProfitablePDF)
						// Profitability page's non-Products tabs (?group_by=manufacturer|category|
						// brand|outlet|staff|day|customer) — one shared handler, all 7 dimensions
						// share the same {group, units_sold, revenue, profit, margin_pct} row shape.
						rp.Get("/reports/profitability-grouped-document", reportPDF.ProfitabilityGroupedDocument)
							// Analytics-page reports (cards + table + bar chart; ?format=pdf|csv).
							rp.Get("/reports/sales-by-hour-document", reportPDF.SalesByHourDoc)
							rp.Get("/reports/sales-by-category-document", reportPDF.SalesByCategoryDoc)
							rp.Get("/reports/product-mix-document", reportPDF.ProductMixDoc)
							rp.Get("/reports/void-summary-document", reportPDF.VoidSummaryDoc)
							// All-Sales page export — same filters + per-cashier scoping as GET /orders.
							rp.Get("/reports/all-sales-document", reportPDF.AllSalesDocument)
						})
					}

					// Webhook subscriptions & delivery log (Sprint 12)
					if webhooks != nil {
						pos.Get("/webhooks", webhooks.List)
						pos.Post("/webhooks", webhooks.Create)
						pos.Put("/webhooks/{webhookID}", webhooks.Update)
						pos.Delete("/webhooks/{webhookID}", webhooks.Delete)
						pos.Get("/webhooks/{webhookID}/deliveries", webhooks.ListDeliveries)
					}

					// Delivery channel integrations (Uber Eats, Glovo, etc.) Ã¢â‚¬â€ Sprint 12
					if channels != nil {
						pos.Get("/channels", channels.ListChannels)
						pos.Post("/channels", channels.CreateChannel)
						pos.Put("/channels/{channelID}", channels.UpdateChannel)
						pos.Delete("/channels/{channelID}", channels.DeleteChannel)
						pos.Get("/channels/{channelID}/sync-jobs", channels.ListSyncJobs)
						pos.Post("/channels/{channelID}/sync-jobs", channels.TriggerSyncJob)
					}

					// Online ordering pickup status Ã¢â‚¬â€ KDS click-and-collect (Sprint 13)
					if onlineOrders != nil {
						onlineFeat := subscriptions.RequireFeature(subscriptions.FeatureOnlineOrdering)
						pos.Get("/online-orders/pickup", onlineOrders.ListPickup)
						// Collection history (collected + uncollected) for pickup/takeaway/delivery.
						pos.Get("/online-orders/history", onlineOrders.ListPickupHistory)
						// POS-native delivery dispatch queue (order_subtype=delivery) — read-only list.
						pos.Get("/online-orders/dispatch", onlineOrders.ListDeliveryDispatch)
						// Pickup hand-off + delivery rider assignment mutate order state Ã¢â‚¬â€ gate on
						// orders.change (waiter, manager+). Reads (pickup/rider lists) stay open.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.change", "pos.orders.manage"), onlineFeat).
							Post("/online-orders/{orderID}/ready", onlineOrders.MarkReady)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.change", "pos.orders.manage"), onlineFeat).
							Post("/online-orders/{orderID}/collected", onlineOrders.MarkCollected)
						// WS-D delivery rider assignment: list fleet (proxy logistics) +
						// assign rider (delegate to ordering-backend, which owns the order).
						pos.Get("/online-orders/riders", onlineOrders.ListAvailableRiders)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.change", "pos.orders.manage"), onlineFeat).
							Post("/online-orders/{orderID}/assign-rider", onlineOrders.AssignRider)
						// Shipments: hand a sale with shipping details to logistics-api (delivery-
						// execution source of truth) — creates a delivery task, pos keeps the reference.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.change", "pos.orders.manage")).
							Post("/orders/{orderID}/dispatch-delivery", onlineOrders.DispatchShipment)
					}

					// Daily closings (ERP reconciliation)
					if closings != nil {
						pos.Post("/outlets/{outletID}/daily-close", closings.CloseDay)
						pos.Get("/outlets/{outletID}/daily-closings", closings.ListDailyClosings)
					}

					// Repair / job-card module. Reads open to authenticated staff; mutations gated
					// on pos.retail.add / pos.retail.manage (a retail/service-shop workflow).
					if repairs != nil {
						repairWrite := outletmw.RequireServicePermission(rbacSvc, "pos.retail.add", "pos.retail.manage")
						pos.Get("/repairs", repairs.List)
						pos.With(repairWrite).Post("/repairs", repairs.Create)
						pos.Get("/repairs/{id}", repairs.Get)
						pos.With(repairWrite).Patch("/repairs/{id}", repairs.Update)
						pos.With(repairWrite).Post("/repairs/{id}/parts", repairs.AddPart)
						pos.With(repairWrite).Delete("/repairs/{id}/parts/{partID}", repairs.RemovePart)
						pos.With(repairWrite).Post("/repairs/{id}/settle", repairs.Settle)
					}
				})

				// Hotel module — hospitality only
				if hotel != nil {
					tenant.Route("/hotel", func(h chi.Router) {
						h.Use(outletmw.RequireUseCase("hospitality"))
						conferenceFeat := subscriptions.RequireFeature(subscriptions.FeatureConference)
						// Front-desk operational actions (check-in/out, folio, bookings, room status,
						// facility booking, amenities, housekeeping) require hotel CHANGE; admin master
						// data (create/edit/delete rooms & facilities) requires hotel MANAGE.
						hotelChange := outletmw.RequireServicePermission(rbacSvc, "pos.hotel.change", "pos.hotel.manage")
						hotelManage := outletmw.RequireServicePermission(rbacSvc, "pos.hotel.manage")

						// Inventory master pickers (link rooms/facilities/amenities to inventory SERVICE
						// items + packages) — shared by both the hotel PMS and facilities forms below,
						// so they sit outside either feature gate rather than requiring hotel_module.
						h.Get("/inventory-service-items", hotel.ListInventoryServiceItems)
						h.Get("/inventory-bundles", hotel.ListInventoryBundles)

						// ── Full hotel PMS: rooms, group bookings, conference/events, folio,
						// amenities, housekeeping. Requires hotel_module (Enterprise+).
						h.Group(func(g chi.Router) {
							g.Use(subscriptions.RequireFeature(subscriptions.FeatureHotelModule))
							g.Get("/rooms", hotel.ListRooms)
							g.With(hotelManage).Post("/rooms", hotel.CreateRoom)
							g.Get("/rooms/{id}", hotel.GetRoom)
							g.With(hotelChange).Patch("/rooms/{id}/status", hotel.UpdateRoomStatus)
							// Multi-room / group bookings (RoomBooking header -> many RoomGuest)
							g.With(hotelChange).Post("/bookings", hotel.CreateRoomBooking)
							g.Get("/bookings", hotel.ListRoomBookings)
							g.Get("/bookings/{id}", hotel.GetRoomBooking)
							g.With(hotelManage).Patch("/bookings/{id}", hotel.UpdateRoomBooking)
							g.Get("/bookings/{id}/guests", hotel.ListBookingGuests)
							// Conference / events (BEO) + delegate meal cards — require conference_events.
							g.With(outletmw.RequireServicePermission(rbacSvc, "pos.conference.add", "pos.conference.manage"), conferenceFeat).
								Post("/events", hotel.CreateEventBooking)
							g.Get("/events", hotel.ListEventBookings)
							g.Get("/events/{id}", hotel.GetEventBooking)
							g.With(outletmw.RequireServicePermission(rbacSvc, "pos.conference.change", "pos.conference.manage"), conferenceFeat).
								Patch("/events/{id}", hotel.UpdateEventBooking)
							g.Get("/events/{id}/reconciliation", hotel.ReconcileEvent)
							g.With(outletmw.RequireServicePermission(rbacSvc, "pos.conference.manage"), conferenceFeat).
								Post("/events/{id}/generate-mealcards", hotel.GenerateMealCards)
							g.With(outletmw.RequireServicePermission(rbacSvc, "pos.conference.change", "pos.conference.manage"), conferenceFeat).
								Post("/mealcards/{code}/redeem", hotel.RedeemMealCard)
							g.With(hotelChange).Post("/rooms/{id}/check-in", hotel.CheckIn)
							g.With(hotelChange).Post("/rooms/{id}/check-out", hotel.CheckOut)
							g.With(hotelChange).Post("/rooms/{id}/folio", hotel.PostFolioCharge)
							g.Get("/rooms/{id}/folio", hotel.GetRoomFolio)
							// Checkout/settlement: full bill summary + record folio payments (with history).
							g.Get("/rooms/{id}/folio/summary", hotel.GetFolioSummary)
							g.With(hotelChange).Post("/rooms/{id}/settle", hotel.SettleFolio)
							// Amenity management
							g.Get("/amenities", hotel.ListAmenities)
							g.With(hotelManage).Post("/amenities", hotel.CreateAmenity)
							g.Get("/rooms/{id}/amenities", hotel.ListRoomAmenities)
							g.With(hotelChange).Post("/rooms/{id}/amenities", hotel.AssignAmenityToRoom)
							g.With(hotelChange).Post("/rooms/{id}/amenities/{amenityId}/charge", hotel.ChargeAmenityToGuest)
							// Late checkout and batch checkout
							g.With(hotelChange).Post("/rooms/{id}/late-checkout", hotel.LateCheckout)
							g.With(hotelChange).Post("/rooms/batch-checkout", hotel.BatchCheckout)
							// Housekeeping
							g.Get("/housekeeping", hotel.ListHousekeepingTasks)
							g.With(hotelChange).Post("/housekeeping", hotel.CreateHousekeepingTask)
							g.With(hotelChange).Patch("/housekeeping/{taskID}", hotel.UpdateHousekeepingTask)
						})

						// ── Bookable spaces: co-working desks, conference/meeting rooms — sell +
						// capacity-manage a Facility from the till. Requires facility_booking
						// (POS_HOSP_PRO "Growth" and up), independent of the full hotel PMS above —
						// a cafe with spare floor space shouldn't need rooms/check-in/folio just to
						// sell co-working.
						h.Group(func(g chi.Router) {
							g.Use(subscriptions.RequireFeature(subscriptions.FeatureFacilityBooking))
							g.Get("/facilities", hotel.ListFacilities)
							g.With(hotelManage).Post("/facilities", hotel.CreateFacility)
							g.Get("/facilities/{id}", hotel.GetFacility)
							g.With(hotelManage).Patch("/facilities/{id}", hotel.UpdateFacility)
							g.With(hotelManage).Delete("/facilities/{id}", hotel.DeleteFacility)
							g.Get("/facilities/{id}/availability", hotel.GetFacilityAvailability)
							g.With(hotelChange).Post("/facilities/{id}/book", hotel.BookFacility)
							g.With(hotelChange).Patch("/facilities/bookings/{bookingID}", hotel.UpdateBooking)
							g.With(hotelChange).Post("/facilities/bookings/{bookingID}/complete", hotel.CompleteFacilityBooking)
							g.Get("/facilities/bookings", hotel.ListFacilityBookings)
						})
					})
				}
			})
		})

		// ── Service-to-service (S2S) endpoints ──────────────────────────────────────
		// Internal backend-to-backend routes, authenticated with the shared
		// INTERNAL_SERVICE_KEY sent as the X-API-Key header (no user JWT). pos-api is the
		// loyalty source-of-truth (balances keyed on tenant + customer_phone), so other
		// services (e.g. ordering-backend) earn/redeem against these endpoints.
		if internalServiceKey != "" && (loyalty != nil || reports != nil || payments != nil || promotions != nil) {
			api.Group(func(s2s chi.Router) {
				s2s.Use(requireInternalServiceKey(internalServiceKey))
				s2s.Route("/s2s/{tenant}", func(t chi.Router) {
					if loyalty != nil {
						t.Post("/loyalty/earn", loyalty.S2SEarn)
						t.Post("/loyalty/redeem", loyalty.S2SRedeem)
						t.Get("/loyalty/balance", loyalty.S2SBalance)
					}
					if promotions != nil {
						// pos-api is the DISCOUNT source of truth (Promotion+PromotionRule).
						// Other services list/create/apply discounts here — never define a
						// parallel discount entity (see promotions_s2s.go).
						t.Get("/discounts", promotions.S2SListDiscounts)
						t.Post("/discounts", promotions.S2SCreateDiscount)
						t.Post("/discounts/apply", promotions.S2SApplyDiscount)
						// Real usage-cap reservation for the online checkout (2026-09-06/07
						// redemption-cap work) -- ordering-backend calls this at real
						// order-creation time; pos-api's own order creation calls
						// promotions.Service.ReserveRedemption directly (same binary).
						t.Post("/discounts/{promoId}/reserve", promotions.S2SReserveRedemption)
						// Storefront marketing banners — promotions flagged via metadata["banner"]
						// .show_on_storefront, consumed by ordering-frontend's banner widget.
						t.Get("/discounts/banners", promotions.S2SListBanners)
					}
					if reports != nil {
						// POS units sold per SKU — consumed by inventory-api menu-engineering/variance
						// so POS sales are counted, not only ordering-service orders.
						t.Get("/pos/sales/by-sku", reports.S2SSalesBySKU)
					}
					if payments != nil {
						// Manual ops recovery tool — see S2SRecheckOrderCompletion's doc comment.
						t.Post("/orders/{orderNumber}/recheck-completion", payments.S2SRecheckOrderCompletion)
					}
				})
			})
		}
	})

	return r
}

// requireInternalServiceKey guards S2S routes by requiring the shared INTERNAL_SERVICE_KEY in the
// X-API-Key header. The key is compared in constant time to avoid leaking it via timing.
func requireInternalServiceKey(expected string) func(http.Handler) http.Handler {
	expectedBytes := []byte(expected)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			provided := r.Header.Get("X-API-Key")
			if provided == "" || subtle.ConstantTimeCompare([]byte(provided), expectedBytes) != 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"invalid or missing service key"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requirePlatformOwner gates a route to platform-owner principals only. It is used
// for the platform-default (tenant_id NULL) backup-destination management routes,
// which configure the off-PVC mirror for ALL tenants and so must never be reachable
// by an ordinary tenant user. Returns 401 when unauthenticated, 403 otherwise.
func requirePlatformOwner(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := authclient.ClaimsFromContext(r.Context())
		if !ok || claims == nil || claims.Subject == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		// SEC-3 (auth-client v0.10.0): a tenant superuser is NOT a platform owner and must
		// never reach platform-default (tenant_id NULL) management, which configures the
		// off-PVC mirror for ALL tenants. Mirror the shared RequirePlatformOwner contract.
		if !claims.IsPlatformOwner {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"platform owner required"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}
