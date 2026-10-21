# Zuscar backend (Go, Gin, PostgreSQL)

See ../CLAUDE.md for cross-repo rules. Read @docs/PROGRESS.md and @docs/ASSUMPTIONS.md first.

## Commands
- Build: `go build ./...`   Vet: `go vet ./...`   Format check: `gofmt -l .`
- SQL codegen: `sqlc generate` (generated code is committed; diff must be empty after generate)
- Tests need Postgres: `export TEST_DATABASE_URL="postgres://zuscar:change-me-dev-only@localhost:5432/postgres?sslmode=disable"` then `go test ./...` (slow, about 3 min)
- Local migrate: `docker compose run --rm --entrypoint /app/migrate backend up`

## Architecture rules
- Modules talk only through service packages. `internal/archtest` enforces this; do not bypass.
- One sqlc.yaml block per module. New migration => update sqlc.yaml schema list.
- Money is integer cents, tax-inclusive. No floats for money.
- Every mutating endpoint is idempotent: `Idempotency-Key`, Begin first, Complete last, replays send `Idempotent-Replayed`.
- Business date is Africa/Nairobi. Gapless counters and immutability triggers exist; do not edit rows that are meant to be append-only (void with a reason instead).
- Orders use optimistic concurrency (`expected_version`, error code `order_changed`).
- Migrations: new numbered file in database/migrations, never edit an applied one.

## Definition of done for a stage
build, vet, gofmt, sqlc diff clean, openapi.yaml parses, docs updated (routes.csv, openapi.yaml, rbac.md, ASSUMPTIONS.md, PROGRESS.md), full tests pass.
