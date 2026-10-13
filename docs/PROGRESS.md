# Zuscar build progress

## Stage 0: Orientation (done, docs only, no code)

**Understanding.** Zuscar is a single-venue, online-first hospitality system (restaurant, bar/club, ~13 rooms) built as a modular monolith: Next.js on Vercel proxies `/api/*` (ADR-05) to a Go/Gin API on Render, which is the only client of Supabase PostgreSQL (ADR-02). Integrity rules drive the design: integer-KES money (NN-09), atomic idempotent sale finalisation (NN-04), print separate from persistence (NN-05), append-only stock ledger (ADR-07), DB exclusion constraint for room overlap (ADR-06), room charges as folio postings (NN-10), manual M-Pesa pending until confirmed (NN-11). eTIMS (DG-01) and M-Pesa API (DG-02) are gated: stubs only.

**Top risks to watch.** R-01/DG-03 printer, R-08 sandbox limits (below), R-05 M-Pesa status, R-06 stock data quality, R-09 scope creep.

**Brief vs prompt conflicts (reported, none silently resolved).**
1. Prompt section 2 says every stage report is "exactly" Built/Ran/Not run/Need/Next, while the Stage 0 row asks for understanding/conflicts/tools/questions. Resolved for process (prompt wins): five-part shape, with those items folded in.
2. Brief DQ-06/DQ-07/DQ-09 say "seed sample data"; prompt section 8 limits fictional data to dev/test seed only. Prompt wins (process); requirements unchanged.
3. Brief 5.1 names docs as lowercase (`assumptions`, `progress`); prompt names `PROGRESS.md`, `ASSUMPTIONS.md`, `CONTENT_TODO.md`, `traceability.md`. Prompt names used.
4. Prompt lists a favicon with 16/32/64/180 px PNGs; only the favicon SVG was supplied (see CONTENT_TODO).

## Sandbox probe (Stage 0, actually run)
| Tool | Result |
|---|---|
| node / npm | v22.22.2 / 10.9.7 |
| Next.js | `npm view next version` = 16.4.0 (pin at Stage 4) |
| Go | not preinstalled; installed `golang-1.24-go` via apt: go1.24.13 at `/usr/lib/go-1.24/bin` |
| Go modules | `proxy.golang.org`, `go.dev`, `golang.org` unreachable. `GOPROXY=direct` works for GitHub modules. Latest gin (v1.12.0) and pgx/v5 (v5.11.0) require Go >= 1.25 and toolchain download is blocked. `replace golang.org/x/crypto => github.com/golang/crypto v0.31.0` worked; gin v1.10.1 failed on `golang.org/x/arch` (needs another replace) |
| sqlc | v1.31.1 installed from GitHub release binary |
| PostgreSQL | not preinstalled; installed 16.15 via apt after `apt-get update`; started on `/tmp/pg` port 5433; `btree_gist` works; exclusion constraint rejected an overlapping range and accepted back-to-back `[a,b)` ranges |
| Docker / Compose | not available (cannot run here) |
| psql / pg_ctl | present after install, not on PATH by default (`/usr/lib/postgresql/16/bin`) |
| Network | npm registry OK; github.com OK; api.github.com rate-limited (403) on one call; Google Fonts blocked (fontsource used instead) |
| npm fonts/libs | `@fontsource-variable/outfit` 5.3.0, `@fontsource-variable/inter` 5.3.0, `@fontsource/ibm-plex-mono` 5.3.0, tailwindcss 4.3.3, lucide-react 1.55.0, zod 4.6.7 all exist |

Sandbox state resets between tasks, so Go/PostgreSQL/sqlc installs must be repeated at the start of later stages (a `scripts/sandbox-setup.sh` is planned for Stage 4).

## Decisions
- Go toolchain plan for Stage 4: try Go 1.25 built from GitHub source with 1.24 as bootstrap; fallback: pin gin/pgx to the newest versions that build on 1.24 with x/* replaces. Either way recorded in an ADR and the README.

## Open items
- Go 1.25 availability (above). Printer/hardware (DG-03), plans (DG-04), eTIMS/M-Pesa (DG-01/02) remain with the client.
- DB-dependent invariant tests have not run anywhere yet (none written yet).

## Next stage needs
Stage 1 (architecture pack, docs only): no input needed from you.

## Stage 1: Architecture pack (done, docs only)
- Added docs/architecture.md (context, module boundaries and rules, request pipeline), docs/adr/ADR-01..13 (01-10 from the brief, 11-13 new and Proposed), docs/deployment/topology.md (Vercel, Render, Supabase, Cloudflare, env var list), docs/traceability.md (seed).
- Decisions: module boundary rules; unit-of-work passes a Tx between services; tax rate stored as integer basis points; idempotency retention 24 h proposed; bootstrap credentials supplied at run time only.
- Assumptions used: none new (idempotency TTL 24 h is a proposal, to confirm).
- Open items: ADR-11 Go toolchain, ADR-12 migration tool, ADR-13 CSRF (Proposed). Vendor facts in topology.md are UNVERIFIED.
- Nothing was built or run this stage except file writes; no tests exist yet, so DB-dependent invariants are still unrun.
- Next stage needs: nothing (Stage 2: schema and RBAC plan).

## Stage 2: Schema and RBAC plan (done, docs only)
- Added docs/erd.md (7 Mermaid ERDs), docs/schema-constraints.md (table-by-table notes), docs/rbac.md (role by permission matrix, scope notes, Super Admin rules, planned test names), docs/spikes/stage2-constraints-spike.sql (non-migration spike).
- Verified here: all 9 Mermaid blocks in docs (ERDs, architecture, topology) parse with mermaid 11.17.2 under node/jsdom (syntax only, not rendered visually). Spike on PostgreSQL 16.15 scratch DB behaved as designed for: room overlap exclusion (partial, half-open), append-only ledger trigger (update, delete, truncate), transfer pairs netting to zero (deferred), system.admin only on protected role, last-active-Super-Admin guard including a two-session race.
- Not verified: everything else in the schema (no real migrations exist yet); Supabase RLS and btree_gist on the chosen plan.
- Decisions and assumptions: S-01..S-16 in ASSUMPTIONS.md (notably BIGINT identity keys, integer base-unit stock quantities, payments table serving folios, Business Manager role-management guard, extra permission keys).
- Deviations from the brief entity list (justified in schema-constraints.md): added tax_rates, user_pins, login_attempts, price_history, product_modifiers, order_item_modifiers, ticket_status_history, refund_items, sale_voids, print_attempts; folio_payments merged into payments; tables renamed dining_tables.
- Open items: S-15 permission additions still to be confirmed before Stage 5 seeds them. S-13 resolved at Stage 3 (see below).
- Next stage needs: nothing blocking (Stage 3: behaviour plan with state diagrams, API route plan, full AC traceability).

## Stage 3: Behaviour plan (done, docs only)
- Decision from owner: Business Manager does NOT hold roles.manage or permissions.assign; only Super Admin does (S-13 confirmed). docs/rbac.md matrix, protected rule 2 and planned test names updated.
- Added docs/behaviour.md (10 Mermaid state diagrams with transition permissions and guards), docs/api/routes.csv (154 routes, 24 requiring Idempotency-Key) and docs/api/route-plan.md (conventions, counts by stage), and rewrote docs/traceability.md to map all AC-01..AC-21 and NN-01..NN-14 to named planned tests or manual checks.
- Checked: all 10 new Mermaid blocks parse with mermaid 11.17.2 (syntax only). Every permission key used in routes.csv exists in the RBAC matrix (script check); sales.discount, inventory.override_negative and reports.export are checked inside handlers rather than owning a route.
- New assumptions S-17..S-21 (ticket status skipping, void rule, room reserved on arrival day and reprint state, folio reopen, shift close with pending payments).
- Not verified: no code exists; no test has run. Database-dependent invariants remain unrun in the application (only the Stage 2 spike ran).
- Next stage needs: nothing blocking. Stage 4 scaffold will decide ADR-11 (Go toolchain) and generate openapi.yaml and sandbox-setup script.

## Stage 4: Scaffold (done)
- Built: monorepo scaffold. Backend: Go API skeleton (config with production guards, JSON logging with redaction, request ID, access log without query strings, panic recovery, body limit, standard error body, `/healthz`, `/readyz`, graceful shutdown, `healthcheck` subcommand, pgx pool created without eager dial, no migrations on start) and an import-boundary test. Frontend: Next.js 16.4.0 shell, Tailwind v4 tokens, self-hosted fonts, `components/Logo.tsx`, test-environment banner, not-found page, `/api/*` rewrite, security headers. Plus Dockerfiles, Compose, `.env.example`, Makefile, CI, README, OpenAPI skeleton, design-token contrast check.
- Verified in the sandbox (commands and results are in the stage report): Go 1.27.2 built from source; gofmt, go vet, go build, 16 Go tests pass; API binary run against PostgreSQL (`/healthz` 200, `/readyz` 200 then 503 after PostgreSQL stopped while `/healthz` stayed 200, graceful SIGTERM exit 0, production config refusal exit 2); Next build, ESLint, tsc and brand check pass; a built standalone Next server proxied `/api/v1/...` to the API with the request ID forwarded; a fresh `git clone` repeats Go tests, `npm ci`, lint, typecheck and build successfully.
- Decisions: ADR-11 accepted (Go 1.27, sandbox-only module proxy, no replace lines). Rewrite target baked at build time (ADR-05 note). TypeScript pinned to 5.9.3 (7.x exists, untested with Next). Session defaults S-22, limits S-23.
- NOT verified (see report): Docker builds and Compose (no Docker here), the CI workflow, the Docker base-image tags, go.sum against sum.golang.org (generated with GOSUMDB=off, ADR-11), Vercel rewrite behaviour on a deployed preview.
- Deviation: this stage is about 50 new files, above the 25-file guideline, because of brand assets, lock files and sandbox scripts; the code itself is about 20 files.
- Database-dependent invariants: still no application test has run against PostgreSQL (the API smoke test only exercised connectivity). `make test-db` has no DB tests yet.
- Next stage needs: nothing blocking. Stage 5 (identity schema, bootstrap and seed-dev commands, migration tooling).

## Stage 5: Identity schema (done) and repository split
- **Owner decision (applies from now on, ADR-14):** two independent repositories. `backend/` (Go, Docker, all project docs) deploys to Render; `frontend/` (Next.js, no Docker) deploys to Vercel. Each folder in the zip is its own Git repo; Stage 0 to 4 history stays in the earlier monorepo zips. Env names: backend `FRONTEND_URL`; frontend `BACKEND_URL`, `NEXT_PUBLIC_APP_URL`. Frontend Dockerfile removed (Vercel builds from source). Backend runs fully in Docker (Compose: postgres, backend, optional test runner; the image carries api, migrate, bootstrap; seed-dev only in the dev target).
- **Built:** migrations 0001 to 0004 (extensions and helpers, identity tables, guard triggers, settings/tax/counters); repo-owned migration runner and `migrate` command (ADR-12); Argon2id password package; permission and role catalog checked against docs/rbac.md; sqlc repository layer (identity, settings); `bootstrap` (first Super Admin, templates, settings, tax rate, counters, nothing else) and `seed-dev` (fake `dev.*` users, production guard, refuses databases with real users); test database helper; `/api/v1/health` for the web connection check; `PORT` fallback for Render; backend Dockerfile (targets: test, dev, runtime), Compose, `.env.example`, Makefile, CI; frontend `.env.example`, `BackendStatus` component, `lib/env.ts`, CI, step-by-step READMEs.
- **Verified in the sandbox:** 56 Go tests pass in a fresh clone with a real PostgreSQL 16.15 (0 failed, 0 skipped; without TEST_DATABASE_URL 35 pass and 21 database tests skip loudly), including database tests for AC-11, AC-12 (with a two-connection race; a mutation check that removes the advisory lock makes the race test fail), AC-13, NN-08 (audit_logs), bootstrap and seed behaviour. Without TEST_DATABASE_URL the database tests skip with a loud message. Binaries run against PostgreSQL: migrate up/status/re-run, bootstrap (refuses the dev password, refuses a second run), seed-dev (exit 3 under APP_ENV=production, refuses a database with a real user). A running API plus `npm run dev` with `.env.local`: `/api/v1/health` through the web origin returned 200, the home page showed "Backend connected", and with the API stopped it showed "Backend not reachable".
- **Not verified:** all Docker artefacts (Dockerfile targets, Compose, image tags), both CI workflows, Supabase behaviour (direct vs pooler, sslmode, btree_gist), Vercel and Render deployment, `go.sum` against sum.golang.org (ADR-11), `sqlc diff` in CI.
- **Deferred with reason:** RLS and migrator/app DB roles to Stage 25 (see schema-constraints.md section 10).
- **Decisions and assumptions:** ADR-12 accepted, ADR-14 added; S-24 to S-28.
- **Open items:** S-15 (extra permission keys) was seeded as written, so treat it as confirmed unless you object; S-17, S-18, S-21 still to confirm.
- **Next stage needs:** nothing blocking. Stage 6 (authentication: login, logout, me, sessions, CSRF, lockout, optional PIN).

