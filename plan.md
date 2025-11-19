## POS Service Delivery Plan

### 1. Vision & Guiding Principles
- Build a multi-tenant, cloud-ready Point-of-Sale backend that powers third-party outlets (cafés/bars, quick-service restaurants, supermarkets, ecommerce fulfilment/ecommerce shops, kitchen displays, kiosks) using the shared `tenant_slug` and outlet registry consumed by the cafe-backend, inventory, logistics, and auth services.
- Provide a composable API layer that can be embedded into client surfaces (web, mobile, kiosk, delivery app) while remaining extensible for partner ecosystems and white-label deployments.
- Embrace an event-driven architecture so POS actions (sales, refunds, drawer activity, menu changes, stock events) propagate in real time to treasury, notifications, delivery, inventory, licensing, and analytics services.
- Optimise for resilience and near-real-time operation even with intermittent connectivity through smart caching, queueing, offline queues, and automated sync jobs.
- Treat subscription/licensing state as a first-class domain so feature availability, device limits, and renewal reminders are centrally managed and surfaced to administrators.
- **Entity Ownership**: This service owns POS-specific entities: POS orders, devices, sessions, cash drawers, POS payments (references), promotions, gift cards, price books (outlet-specific overrides), table management, and bar tabs. **POS does NOT own**: catalog items (references inventory-service), users (references auth-service via `user_id`), payment processing (uses treasury-app APIs), inventory balances (queries inventory-service APIs). See `docs/cross-service-entity-ownership.md` for complete ownership matrix.

### 2. Technical Foundations
- **Language & Runtime:** Go 1.22+, strict gofmt, golangci-lint.
- **Frameworks & Libraries:**
  - HTTP transport with `chi` router, middleware stack mirroring the cafe-backend for consistency.
  - Validation via `go-playground/validator`, configuration via `kelseyhightower/envconfig`.
  - `ent` ORM for schema-as-code modelling and migrations.
  - `pq`/`pgx` driver wrapped by `ent/dialect/sql` for PostgreSQL connectivity.
  - Redis client (`redis/go-redis/v9`) for low-latency session/cache/event buffers.
  - OpenAPI/Swagger (`swaggo/swag`) for contract-first REST endpoints; ConnectRPC optional for gRPC wrappers.
- **Data Stores:**
  - PostgreSQL as the system of record (multi-schema, tenant awareness).
  - Redis for ephemeral state: POS sessions, offline queues, rate limiting, publish/subscribe.
- **Deployment:** Docker multi-stage builds, Helm charts, ArgoCD GitOps pipeline aligned with other services.
- **Observability:** zap logging, Prometheus metrics, OpenTelemetry tracing, Tempo/Jaeger compatibility.
- **Testing:** Go test (table-driven), Testcontainers for Postgres/Redis integration, k6 for performance validation.
- **Auth-Service SSO Integration:** ✅ **COMPLETED** - Integrated `shared/auth-client` v0.1.0 library for production-ready JWT validation using JWKS from auth-service. All protected `/v1/{tenantID}` routes require valid Bearer tokens. Swagger documentation updated with BearerAuth security definition. **Deployment:** Uses monorepo `replace` directives with versioned dependency (`v0.1.0`). Go workspace (`go.work`) handles local development automatically. Each service has independent DevOps workflows and can be deployed separately while sharing the auth library. See `shared/auth-client/DEPLOYMENT.md` and `shared/auth-client/TAGGING.md` for details.

### 3. Core Capabilities & Domain Modules
1. **Tenant & Outlet Management**
   - Register tenants (franchises, merchant partners) and map POS outlets/locations using the shared tenant/outlet identifiers agreed across cafe-backend, inventory, and logistics services. If an outlet does not exist locally, the service triggers a tenant discovery webhook to fetch authoritative data before persisting POS records.
   - Store configuration profiles (timezone, tax config, operating hours, supported tenders).
   - Manage device provisioning (register POS terminals/tablets/kiosks) while syncing device metadata with the common registry (no duplicate outlet tables downstream).

2. **User, Roles & Sessions**
   - POS-specific RBAC roles (cashier, supervisor, manager, inventory clerk).
   - Device sign-in via PIN, RFID, or OAuth tokens (delegated to identity service).
   - Session heartbeat tracking, forced logout, device-level permissions.

3. **Catalog & Pricebook**
   - Sync menu/product data from cafe-backend or external ERP.
   - Support multiple pricebooks per outlet (happy hour, wholesale, ecommerce, regional).
   - Local overrides, modifiers, bundles/combos, allergen/nutritional metadata, barcode/PLU mapping.

4. **Order Capture & Ticketing**
   - Create and update POS orders (dine-in, bar tab, take-out, delivery handoff, ecommerce pickup, drive-thru).
   - Handle split bills, table assignments, coursing, service charges, tip prompts, special instructions.
   - Real-time order events broadcast to kitchen display systems, delivery service, and customer notification channels.

5. **Tendering & Cash Management**
   - Accept diverse tenders: cash, card, mobile money (M-Pesa), gift cards, loyalty points and vouchers (via treasury).
   - Cash drawer lifecycle: float setup, open/close shifts, blind counts, mid-shift skims, discrepancies, cash audits.
   - Integrate with treasury microservice for settlement reconciliation, refunds, cash-in/payments, payouts, chargebacks.

6. **Inventory & Stock Adjustments**
   - Deduct inventory at sale, track wastage, returns, transfers between outlets.
   - Alarm notifications on low stock thresholds (calls notifications service).
   - Bulk stocktake imports/exports, integration hooks for WMS/ERP connectors.

7. **Kitchen & Fulfilment Coordination**
   - Route POS orders to kitchen production queues with status updates.
   - Manage order ready notifications, expedite signals, and re-fire requests.
   - Sync with delivery app for order readiness and driver pickup.

8. **Ecommerce & Omnichannel Sync**
   - Provide connectors to ecommerce storefronts (order ingest, inventory updates).
   - Manage click-and-collect workflows, pickup scheduling, customer communication.

9. **Promotions, Loyalty & Giftcards**
   - Apply promo codes, auto-discounts, membership pricing based on plan entitlements.
   - Integrate with loyalty ledger from cafe-backend for earn/burn operations.
   - Gift card issuance, redemption, and balance management.

10. **Reporting & Analytics**
    - Sales reports (daily/weekly/monthly), tender breakdown, tax reports, cashier performance.
    - Cash variance, void/discount audit logs, product mix, top sellers.
    - Export jobs for finance teams; restful API for BI tools.

11. **Audit, Compliance & Security**
   - Audit trails for voids, discounts, drawer adjustments, user logins.
   - Configurable retention policies per tenant, GDPR/Kenyan DPA compliance, fiscal printer roadmap.
   - Fine-grained permissions for high-risk actions (refunds, price overrides, cash drawer openings).
12. **Licensing & Subscription Enforcement**
   - Track plan entitlements (devices, outlets, advanced features) sourced from subscription service.
   - Record usage metrics (orders processed, integrations used) for overage billing.
   - Trigger renewal reminders, grace-period enforcement, and feature gating via notifications & treasury.

### 4. Scenario Coverage & Workflows
### Café / Bar Service
- Table, tab, and counter-order workflows with seat mapping, coursing, bartender-specific permissions, happy-hour scheduling, and split/merge bills.
- Tip pooling, service charge management, age-restricted item prompts, and spill/waste tracking.
- Cash drawer reconciliation per shift, petty cash tracking, and tip-out events.

### Kitchen Display & Back-of-House
- Kitchen routing rules by prep station, expo dashboards, bump/pause/remake workflows, load balancing.
- Expo-driven notifications back to wait staff/riders via notifications service and delivery integrations.
- Print/fiscal integration hooks for kitchen, bar, and service printers.

### Ecommerce / Retail (Supermarket, Convenience, Pharmacy)
- Barcode scanning, weigh-scale integration, rainchecks, layaways, and bulk discount rules.
- Cash tray management with forced drawer counts, deposit tracking, and configurable receipt templates.
- Customer loyalty enrolment at POS, click-and-collect staging (zone & bin management), parcel scanning for delivery & courier handoff.

### Over-the-Counter / Kiosk / QSR
- Self-service kiosk flows with upsell prompts, queue management, and low-touch payment capture.
- Quick toggle menus, combo builders, multi-lane order throttling, and ticket recall.
- Offline queue with automatic conflict resolution and audit when connectivity is restored.

### Delivery & Fulfilment Coordination
- Flag orders for delivery pickup, emit readiness events to delivery app, capture driver handoff confirmation.
- Manage third-party delivery connectors (Bolt, Uber, Glovo) via adaptor pattern.
- Trigger notifications for delayed pickups, substitution approvals, and driver reassignment.

### 5. Integrations
#### 5.1 Integration Points (endpoints/events)
- Treasury:
  - REST: `/v1/{tenant}/payments`, `/refunds`, `/payouts`
  - Events: `payment_initiated`, `payment_captured`, `refund_processed`
- Notifications:
  - REST: `/v1/{tenant}/notify` (channels: sms,email,push)
  - Events: `pos.order.ready`, `pos.cash_exception`, `pos.license.renewal_due`
- Inventory:
  - Webhooks: `inventory.low_stock`, `inventory.po.approved`
  - REST: `/v1/{tenant}/inventory/consumption`, `/v1/{tenant}/transfers`
- Logistics:
  - Webhooks: `logistics.task.assigned`, `logistics.task.status.changed`
  - REST: `/v1/{tenant}/handoff/{orderId}` for driver pickup confirmation
  - Note: Align with inventory zone/branch policies to prefer nearest stock for click-and-collect and deliveries
- **Treasury Service (`treasury-app`):**
  - Payment initiation (card tokens, MPesa STK push) and capture flows.
  - Settlement summaries, refund processing, ledger entry synchronization.
  - Subscription billing for POS licenses and overage tracking, invoice generation, grace-period management.
- **Notifications Service (`notifications-app`):**
  - Push/SMS/email for order readiness, shift reminders, cash exceptions, stock alerts.
  - Multi-channel alerts for license renewals, integration failures.
- **Cafe Backend:**
  - Shared catalog, menu items, loyalty accounts, customer profiles using canonical item IDs from inventory.
  - Delivery order status exchange (POS ready -> delivery dispatch) aligned with `logistics-service` task IDs and shared tenant/outlet registry.
  - Subscription entitlements to gate advanced POS features without duplicating entitlement tables.
- **Inventory Service (`inventory-service`):**
   - Receives stock consumption events and provides low-stock alerts and BOM depletion using the shared tenant/outlet and item identifiers with webhook callbacks.
- **Logistics Service (`logistics-service`):**
   - Handles curbside pickup readiness, delivery handoff, and driver assignment using consistent task IDs delivered via callback events.
- **Inventory/ERP Connectors:**
  - Optional connectors for third-party inventory management.
  - Webhooks for stock adjustments, purchase order receptions.
- **Analytics & Data Warehouse:**
  - Outbox pattern to feed event hubs (Kafka/NATS) consumed by analytics pipelines.
- **Device Management / MDM:**
  - Exposure of APIs for Mobile Device Management to provision kiosks and tablets.
- **Fiscal / Regulatory Gateways:**
  - Hooks for electronic tax registers, digital receipt portals, and region-specific compliance APIs.
  - Scheduling of periodic fiscal reports, receipt archiving, and enforcement of regulatory daily closures.
- **Auth Service (`auth-service`):**
  - Supplies SSO tokens, role claims, and emits tenant/outlet discovery callbacks so POS can auto-provision metadata before processing sessions.
- **Payment Gateways & Terminals:**
  - Gateway providers (e.g., Stripe, MPesa, Flutterwave) and terminal providers (PAX, Verifone) via connector adapters.
- **Ecommerce Platforms & Marketplaces:**
  - Storefront order ingest, catalogue sync, and returns handling via provider registry.

### 6. System Architecture
- **API Layer:** RESTful endpoints versioned by tenant (`/v1/{tenant}/pos/...`) with OpenAPI docs. Optional ConnectRPC/gRPC for high-throughput operations (bulk sync, streaming events).
- **Service Layer:** Clean/hexagonal architecture; domain services orchestrate business logic, interface adapters encapsulate I/O (treasury, notifications, delivery).
- **Persistence Layer:** Ent-generated schemas, migrations via `ent/migrate`. Soft deletes where required (orders, catalog).
- **Caching & Queues:** Redis for:
  - POS terminal session cache and ephemeral order buffers.
  - Pub/Sub for live drawer/order updates between devices.
  - Rate limiting, distributed locks (e.g. single cash drawer open per terminal).
- **Eventing:** Outbox table + background dispatcher to NATS/Kafka for cross-service events.
- **Configuration Management:** Tenant-level configuration stored in Postgres (`pos_configurations`, `integration_settings` tables aligned with backend ERD updates).
- **Webhook Fabric:** All integrations (treasury settlements, inventory consumption, logistics handoffs, tenant/outlet discovery) rely on signed HTTP callbacks with retries rather than polling endpoints.

### 7. Data Model Highlights (in addition to existing ERD)
- `pos_stations`, `pos_devices`, `pos_device_sessions` for hardware tracking.
- `pos_orders`, `pos_order_lines`, `pos_order_events` dedicated to POS context (with cross-links to `orders` in delivery backend via `pos_order_links`).
- `cash_drawers`, `cash_drawer_events` for cash management.
- `pos_shift_logs` capturing cashier shifts and reconciliation.
- `promotions`, `promotion_rules`, `promotion_applications`.
- `pos_sync_jobs`, `integration_failures` to audit ecommerce/POS gateway interactions.
- `pos_table_layouts`, `pos_table_assignments` for dine-in management and floor plans.
- `pos_bar_tabs`, `pos_tab_activity` for bar-specific open tabs and authorisations.
- `pos_license_usages`, `pos_subscription_states` mirroring subscription entitlements for fast lookup.
- `barcode_catalog_entries`, `weigh_scale_readings` for retail integrations.
- `offline_event_queue`, `offline_event_replay` supporting offline-first reconciliation.
- `provider_credentials` (encrypted at rest), `provider_configs` for gateways, terminals, ecommerce, fiscal/regulatory connectors.

### 8. Cross-Cutting Concerns
- **Security:** JWT/OAuth tokens validated via identity service, optional mTLS for internal calls, audit logging for privileged actions.
- **Multi-tenancy:** Row-level scoping by `tenant_id`; connection pooling per tenant to Postgres for future sharding; outlets/devices reference the shared registry used by cafe-backend, inventory, and logistics so tenants manage locations once.
- **Configuration & Feature Flags:** Subscription entitlements drive dynamic configuration delivered to client applications; tenant-level feature switches exposed via admin UI.
- **Resilience:** Circuit breakers for external services (treasury, notifications); retry policies with exponential backoff; offline-first queue with reconciliation playbooks.
- **Compliance:** Sales tax handling per locale, ETR integration roadmap, digital receipt storage, GDPR-compliant data export/delete endpoints.
- **Operational Telemetry:** Standard event schema (`pos.order.created`, `pos.cashdrawer.closed`, `pos.subscription.renewal_due`) for downstream analytics and observability dashboards.
- **Configuration & Secrets Management:** Tenant-level provider registry with encrypted secrets (envelope/KMS). Secrets redacted in reads, rotation jobs scheduled, audit trails enabled. Config precedence: env defaults → tenant-level override → flags.

### 9. API Strategy
- **Contracts:** OpenAPI specs (`docs/openapi/pos.yaml`) generated from annotations; published to Stoplight/Postman shared workspace.
- **Versioning:** Semantic versioned endpoints, with backward compatibility window.
- **SDKs:** Auto-generated Go/TypeScript client libraries for internal consumers.
- **Webhooks:** Tenant-configurable webhooks for POS events (order complete, drawer closed) with HMAC signatures.
 - **Provider configuration & secrets APIs:**
   - `/v1/{tenant}/integrations/providers` (list/configure: gateways, terminals, ecommerce, fiscal/regulatory)
   - `/v1/{tenant}/integrations/providers/{provider}/config` (GET/PUT) with encryption-at-rest and redaction on reads
   - `/v1/{tenant}/integrations/events` (ingest/monitor connector failures), `/v1/{tenant}/exports/schedules` (report feeds)

### 10. Deployment & Environments
- **Local Development:** Run Postgres/Redis via docker-compose; service on port 4100 (HTTP). Makefile targets (`make run`, `make migrate`, `make seed`).
- **Staging/Production:** Kubernetes deployment with horizontal pod autoscaling; environment variables injected via Vault/Secrets Manager.
- **CI/CD:** GitHub Actions pipeline running lint/test/build, container publish, helm chart packaging. Automated integration tests hitting staging endpoints.

### 11. Testing & Quality Strategy
- Unit tests for domain services (table-driven).
- Integration tests spinning up Postgres/Redis (Testcontainers).
- Contract tests with consumer-driven approach (Pact) for treasury and notifications.
- Performance tests (k6) simulating high-volume POS transactions during peak hours.
- Chaos tests to validate offline queue behaviour and circuit breaker thresholds.

### 12. Delivery Roadmap (Suggested Sprints)
1. **Sprint 0 – Foundations (Week 1)**
   - [ ] Repo scaffolding, configuration loader, logging/metrics bootstrap.
   - [ ] Health/liveness endpoints, CI/CD pipeline, OpenAPI boilerplate.
   - [ ] Ent schema initialisation: tenants, outlets, devices, users; auth-service JWT middleware and RBAC seed.
   - [ ] Helm chart with HPA/VPA defaults; secrets & env management.
2. **Sprint 1 – Identity & Device Sessions (Weeks 2-3)**
   - [ ] RBAC roles, device registration, session APIs, Redis session cache.
   - [ ] Subscription entitlement hooks for POS feature gating.
   - [ ] Device management endpoints; session timeout/force logout.
3. **Sprint 2 – Catalog & Pricebook Sync (Weeks 4-5)**
   - [ ] Catalog import/sync, local overrides, pricebook endpoints.
   - [ ] POS station configuration; offline sync routines.
   - [ ] Metrics for sync latency/errors; dashboards.
4. **Sprint 3 – Order Capture & Ticketing (Weeks 6-7)**
   - [ ] POS order APIs, ticket status events, real-time updates via WS/SSE.
   - [ ] Dine-in/table/queue workflows; split bills; modifiers/combos.
   - [ ] HPA tuned with custom metrics (orders/sec, p95 latency).
5. **Sprint 4 – Tendering & Treasury Integration (Weeks 8-9)**
   - [ ] Cash drawer module, tenders, refunds/chargebacks via treasury.
   - [ ] Tip pooling/variance reporting; initial fiscal compliance hooks.
   - [ ] PCI/PII controls; audit logs for privileged actions.
6. **Sprint 5 – Inventory & Promotions (Weeks 10-11)**
   - [ ] Stock deductions, adjustments, low-stock alerts via notifications.
   - [ ] Promotions/discounts, loyalty integration hooks, membership pricing.
   - [ ] KEDA queue-driven scaling for workers; VPA recommendations applied.
7. **Sprint 6 – Ecommerce & POS Gateway Sync (Weeks 12-13)**
   - [ ] POS integration APIs (connections, sync jobs), omnichannel order ingest.
   - [ ] Barcode/scale support, offline reconciliation, error dashboards.
   - [ ] Outbox dispatcher for external systems with DLQ and retries.
8. **Sprint 7 – Provider Registry & External Connectors (Weeks 14-15)**
   - [ ] Tenant provider registry (gateways, terminals, ecommerce, fiscal/regulators) with encrypted secrets.
   - [ ] `/integrations/providers` API; redaction/rotation; connector health endpoints.
   - [ ] Monitoring/alerts for connector errors; backpressure controls.
9. **Sprint 8 – Reporting, Compliance & Hardening (Weeks 16-17)**
   - [ ] Reports API, export jobs, audit logs, security hardening.
   - [ ] Backup scheduling, disaster recovery runbooks, chaos tests.
   - [ ] Performance/load tuning; HA testing.
10. **Sprint 9 – Launch & Handover (Week 18)**
    - [ ] Production readiness, runbooks, dashboards, support plan.
    - [ ] Tenant onboarding playbooks; migration/backfills; SLOs/alerts.

### 13. Backlog & Future Enhancements
- Offline-first mode with local persistence on POS devices and conflict resolution.
- Advanced analytics (AI-driven forecasting, labour optimisation).
- Tip pooling, service charge management, tax integration with local regulators.
- Native connectors for card terminals (PAX, Verifone), weigh-scale integration.
- Multi-brand white-labelling support for SaaS expansion.
- Digital menu boards, self-checkout orchestration, and in-store customer displays.
- ML-driven anomaly detection for void/discount abuse, cash discrepancies, and fraudulent refunds.
- Fiscal printer integrations for targeted regions (Kenya KRA TIMS, EU fiscalization, etc.).
- Partner marketplace APIs for third-party developers to build custom POS extensions.

---

**Next Steps:** Finalise detailed ERD extensions for subscription/licensing and POS tables, align API contracts with treasury/notifications/delivery teams, document POS<>POS gateway event payloads, and schedule Sprint 0 kickoff after stakeholder approval.

### 14. Glossary & Acronyms (Plain‑English Reference)
- POS (Point of Sale): System to record sales transactions in retail, hospitality, etc.
- SKU (Stock Keeping Unit): Unique identifier for a product variant.
- ERP (Enterprise Resource Planning): System managing finance, inventory, sales, etc.
- KDS (Kitchen Display System): Digital display for kitchen orders and status.
- PCI DSS (Payment Card Industry Data Security Standard): Security standard for handling cardholder data.
- 3‑D Secure (3DS) / Strong Customer Authentication (SCA): Extra verification for card payments.
- MPesa (Daraja): Mobile money platform and its API for collections/payouts; STK Push prompts payment on user’s phone.
- API / REST / gRPC / OpenAPI / Webhook: Programmatic interfaces and protocols; see Logistics “Glossary” for definitions.
- Postgres, Redis, Kafka/NATS, Kubernetes/Helm/Argo CD, Prometheus/Grafana, OpenTelemetry: Data, messaging, deployment, and observability stack; see Logistics “Glossary” for concise descriptions.
