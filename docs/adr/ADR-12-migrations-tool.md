# ADR-12: Migration tool

Status: Accepted (Stage 5)

## Context
The brief names golang-migrate or an equivalent. Migrations must be explicit commands, never run on app start (AC-13).

## Decision
A small repo-owned runner (`internal/platform/migrate`, command `cmd/migrate`, about 150 lines). Numbered plain SQL files `NNNN_description.sql` in `database/migrations`, each applied in its own transaction; `schema_migrations` records version, name and SHA-256; a PostgreSQL advisory lock serialises runners; applied files must never change (checksum check); no down migrations (forward-fix). Commands: `migrate up`, `migrate status`.

## Consequences
- golang-migrate was not adopted because the repo-owned runner is small enough to test fully and adds no dependency; this is a judgment, not a measured comparison. Swapping later is possible because files are plain SQL.
- Verified at Stage 5 against PostgreSQL 16.15: apply on an empty database, no-op re-run, edited-migration refusal, rollback of a failed migration, four concurrent runners applying each migration once.
- Not verified: behaviour through a transaction-mode pooler (Supabase). Run migrations over a direct or session connection.
