# Contributing Guide

Thanks for helping build the POS Service! Follow this guide to get started.

## Prerequisites

- Go 1.22+, make, Docker.
- PostgreSQL & Redis (use forthcoming `docker-compose.yml` for local dev).
- Familiarity with BengoBox platform services (inventory, treasury, auth, logistics).

## Setup

1. Clone the repository or create a feature branch from `main`.
2. Copy environment template once available (`config/example.env`).
3. Run `go generate ./internal/ent` after editing schema files.
4. Start dependencies via Docker and run the service with `go run ./cmd/server`.

## Development Workflow

1. Create a descriptive branch (e.g., `feature/cash-drawer-events`).
2. Implement changes with tests and documentation updates.
3. Run:
   ```shell
   go fmt ./...
   golangci-lint run
   go test ./...
   ```
4. Update `plan.md`, `docs/erd.md`, and relevant docs when altering data structures or behaviour.
5. Submit a PR with a clear summary, testing evidence, and integration implications.

## Coding Standards

- Follow clean architecture boundaries: handlers → services → repositories.
- Prefer dependency injection and context-aware operations.
- Write table-driven tests; use Testcontainers for integration tests with Postgres/Redis.
- Ensure migrations are backwards compatible and paired with Ent schema updates.

## Commit Style

- Use context prefixes when useful (`orders:`, `tendering:`, `licensing:`).
- Reference tracking IDs or product requirements when applicable.
- Avoid bundling unrelated changes in one commit.

## Reporting Issues

- Include reproduction steps, expected vs actual behaviour, environment, and logs (sanitised).
- Tag severity (`bug`, `enhancement`, `ops`, `security`).
- For security-sensitive reports, follow `SECURITY.md`.

## Communication

- Slack channels: `#bengobox-pos` (engineering), `#pos-ops` (operations).
- Weekly sync: Thursdays 15:00 EAT.
- Architecture decisions recorded as ADRs under `docs/`.

We appreciate your contributions—together we can deliver a world-class POS platform.

