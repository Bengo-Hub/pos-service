# POS Service

The POS Service delivers a configurable, multi-tenant point-of-sale backend for cafés/bars, retail outlets, kitchens, kiosks, and ecommerce counters within the BengoBox ecosystem, built on the shared `tenant_slug` and outlet registry used by food-delivery, inventory, logistics, treasury, and auth services.

## Core Capabilities

- Outlet/device provisioning and session control.
- POS-specific RBAC, cashier workflows, and licensing enforcement.
- Catalog synchronisation with inventory/food-delivery services, price book management, modifiers, and promotion rules.
- Order/ticket lifecycle with table management, bar tabs, and kiosk flows.
- Tendering and cash drawer management integrated with treasury services.
- Real-time stock consumption events and alerts via inventory service.
- Omnichannel connectors (ecommerce storefronts, kiosks) and settlement reporting.

## Technology Stack

- Go 1.22+, Ent ORM, PostgreSQL, Redis.
- REST APIs using `chi`, optional ConnectRPC/gRPC for streaming updates.
- Swagger/OpenAPI documentation, WebSocket support for real-time dashboards.
- Observability with zap logging, Prometheus metrics, OpenTelemetry tracing.

## Local Setup

```shell
cp config/example.env .env
make deps
docker compose up -d postgres redis
go generate ./internal/ent
go run ./cmd/server
```

Default HTTP port: `4104` (`POS_HTTP_PORT` override).

## Repository Layout

- `cmd/` – service entrypoints (`server`, `migrate`, `seed`, `worker`).
- `internal/app` – configuration and dependency wiring.
- `internal/ent` – Ent schemas and generated code.
- `internal/modules` – domains (catalog, orders, tendering, licensing, integrations).
- `docs/` – ERD, ADRs, integration guides.

## Integrations

- **Inventory Service:** stock consumption, low-stock alerts, BOM depletion delivered via signed webhook callbacks (no polling).
- **Food Delivery Backend:** order linkage, loyalty, subscription entitlements leveraging the shared tenant/outlet registry and webhook events.
- **Logistics Service:** curbside pickup readiness, delivery handoff via logistics task update callbacks.
- **Treasury App:** payment intents, settlements, refunds, revenue accounting.
- **Notifications App:** cash variance alerts, integration failures, license reminders.
- **Auth Service:** SSO and role claims for POS operators; tenant/outlet discovery callbacks ensure this service has current metadata immediately after login.

For more detail see `plan.md` and `docs/erd.md`.

## Current Status

- Architectural planning and documentation in progress.
- Track milestones via `CHANGELOG.md`.

