# Changelog

Changes to the POS Service are documented here following [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed
- Standardized API base path to `/api/v1` (previously `/v1`)
- Standardized Swagger documentation path to `/v1/docs` (previously `/swagger/*`)
- Updated OpenAPI specification servers to use HTTPS URLs for local development
- Updated Swagger specifications to support both HTTP and HTTPS schemes

### Added
- Authored service delivery plan (`plan.md`) describing multi-scenario POS capabilities.
- Documented ERD in `docs/erd.md`.
- Added repository scaffolding (README, contributing, security, support, policies).
- **Service Bootstrap:** Complete Go service scaffolding with HTTP server, configuration, logging, health endpoints, and Swagger documentation.
- **Auth-Service SSO Integration:** Integrated `shared/auth-client` v0.1.0 library for production-ready JWT validation using JWKS from auth-service. All protected `/v1/{tenantID}` routes require valid Bearer tokens. Swagger documentation updated with BearerAuth security definition. Uses monorepo `replace` directives with versioned dependency. See `shared/auth-client/DEPLOYMENT.md` and `shared/auth-client/TAGGING.md` for details.
- **Infrastructure:** PostgreSQL connection pool, Redis caching, NATS event bus integration, Prometheus metrics, structured logging with zap.

### Changed
- Service now uses Go workspace (`go.work`) for local development; production deployments consume `shared/auth-client` as a private Go module.

### Pending
- Ent schema implementation
- Domain-specific handlers and business logic
- Automated tests

