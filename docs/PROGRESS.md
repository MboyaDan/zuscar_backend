# Zuscar build progress

## RESUME HERE (written after Stage 7a; read this section first in a fresh chat)

**Workflow update:** work is now delivered as per-repo patches from GitHub (`docs/claude/WORKFLOW.md`), not zips. Stage 7 is split: 7a (user administration, done, below) and 7b (roles, permissions, audit-logs, settings, tax-rates, tax-verified endpoint, AC-10 route-table test). The next stage is **7b**. Where the older text below says "zip" or "Stage 7", read it with this in mind. Latest CI seen by the owner: backend run on `f627361` succeeded (go and docker jobs).

Master prompt and brief: `docs/system-requirements.docx` is the brief (source of truth for WHAT to build). The master prompt (stage protocol, honesty rules, design system) is not stored in the repo: ask the owner to paste it again, then apply section 2 below, which lists every change the owner made to it since. Stages 0 to 6 are done; the next one is Stage 7. Stop after each stage and wait for the owner to say "continue".

### 1. Current state
- **Backend repo** (folder `zuscar/backend`, branch `main`): last completed stage **6**, commit `ceb3772` (Stage 5 is `7e1b20b`). History for stages 0 to 4 is NOT in this repo; it exists only in the earlier monorepo zips `zuscar-stage-00` to `zuscar-stage-04`. All project documents live here in `docs/`.
- **Frontend repo** (folder `zuscar/frontend`, branch `main`): last completed stage **5**, commit `ce99b9f` (the split into its own repo). Stage 6 made no frontend change. It has the shell page, brand logos through `components/Logo.tsx`, Tailwind tokens, self-hosted fonts, the Test environment banner, a `BackendStatus` card and the `/api/*` rewrite to `BACKEND_URL`. It has NO login screen and no app screens (Stage 8).
- **Latest deliverable:** `zuscar-stage-06.zip` containing `zuscar/backend` and `zuscar/frontend`, each with its own `.git`. This docs update is a patch (`zuscar-resume-docs.patch`), not a stage.
- **Backend contents:** API with `/healthz`, `/readyz`, `/api/v1/health` and `/api/v1/auth/{login,logout,me,change-password,reauth,pin-switch}`; migrations `0001` to `0007`; commands `migrate`, `bootstrap`, `seed-dev`; permission and role catalog; sqlc repositories for identity and settings; Docker Compose (postgres and backend) and a test runner service; CI workflow (never run).
- **Tests:** in a fresh clone with PostgreSQL 16.15 in the authoring sandbox: 94 pass, 0 fail, 0 skip. Without `TEST_DATABASE_URL`: 42 pass, 52 skip loudly. The database tests have run only in the authoring sandbox, never on the owner's machine or in CI.
- **Half-finished work:** none in code; every stage ended at a clean, committed boundary. Deliberate partial pieces: `SetPIN` exists as a service with no endpoint yet (Stage 7); `system_settings.tax_config_status` is set to `provisional_unverified` but nothing can mark it verified yet (Stage 7); there is no frontend for the auth endpoints (Stage 8); `docs/api/routes.csv` lists the whole planned API (155 routes) while `openapi.yaml` documents only built endpoints.
- **Known stale wording** (not corrected, superseded by ADR-14): `architecture.md`, `deployment/topology.md`, `erd.md` and the Stage 1 to 4 entries below still describe a single monorepo and `frontend/...` paths.
- **Authoring-sandbox notes** (only matter when working in a chat sandbox): the sandbox resets between tasks. `scripts/sandbox-setup.sh` rebuilds the toolchain (Go 1.27.2 is built from GitHub source, about 10 minutes; PostgreSQL 16 via apt) and `scripts/sandbox/resolve-modules.sh` resolves Go modules through a local proxy because `golang.org` and `proxy.golang.org` are blocked. PostgreSQL stops between tool calls, so start it inside the same command as the tests. The sandbox shell is `/bin/sh` (no brace expansion); `pkill -f` can match its own shell, so use patterns like `[b]in/api`. Docker and GitHub are not available there.
- **To resume:** the owner writes "resume" with `zuscar-stage-06.zip` attached. Unzip, read this section and `docs/OPEN_ITEMS.md`, summarise the state in three lines, then do Stage 7 only.

### 2. Owner instructions given in chat that change or add to the master prompt or the brief
1. Two independent repositories, `backend` and `frontend`, each with its own Git history, README, `.env.example` and CI; this supersedes the monorepo layout in prompt section 6 (ADR-14).
2. The stage zip contains exactly two folders, `zuscar/backend` and `zuscar/frontend`, each a Git repo; the zip command is no longer the monorepo one.
3. Docker is for the backend only (Compose with postgres and the backend, `env_file: .env`, modelled on the owner's Mezani example). The frontend does not use Docker; it runs locally with `npm install` and `npm run dev` and deploys to Vercel. The backend deploys to Render.
4. Every stage must keep clear step-by-step run instructions in each README: backend entirely in Docker, frontend with `npm install` and `npm run dev`.
5. URLs and secrets always come from environment variables, never hard-coded: backend `DATABASE_URL` and `FRONTEND_URL`; frontend `.env.local` with `NEXT_PUBLIC_APP_URL=http://localhost:3000` plus `BACKEND_URL` (the owner chose these names; they replaced `APP_ORIGIN` and `API_INTERNAL_URL`).
6. Business Manager does NOT hold `roles.manage` or `permissions.assign`; only Super Admin does (S-13).
7. The `bootstrap` flag `--tax-rate-bp` defaults to 1600 (16%) as a provisional development value only; the tax rate stays configuration; tax categories include standard, zero-rated and exempt; never assume every item is standard-rated; document that production tax configuration must be verified against the VAT registration and KRA requirements before go-live; make no eTIMS compliance claim until the integration route is verified and tested.
8. Track S-17, S-18 and S-21 as open items; keep the seeded S-15 permission keys unless a conflict with the agreed requirements is found.
9. Work only on what is asked in each message and wait for "continue" before the next stage (prompt section 2, restated for this docs update).
10. **Mentioned but not defined:** the owner's message requesting this update refers to "the patch workflow". No instruction defining it appears earlier in this chat, so none is applied. Until the owner defines it, stage deliverables stay as stage zips, and this docs update is delivered as a git patch applied with `git apply`.

### 3. Deviations (code or docs differ from the brief or master prompt, and why)
- **Two repos instead of a monorepo, and no frontend Dockerfile or Compose** (prompt section 6): owner decision (ADR-14). Docs and migrations live in the backend repo. The new repos start fresh histories.
- **Renamed variables** `APP_ORIGIN` to `FRONTEND_URL` and `API_INTERNAL_URL` to `BACKEND_URL`: owner naming. `NEXT_PUBLIC_APP_URL` added. The rewrite target is read at build time (verified).
- **Migrations use a small repo-owned runner**, not golang-migrate: brief allows an equivalent; smaller and fully tested (ADR-12).
- **Go 1.27.2 built from source in the sandbox; modules resolved through a local proxy; `go.sum` generated with checksum checking off** (ADR-11): the sandbox blocks the Go module proxy. `go.mod` has no replace lines. The owner must run `go mod tidy && go mod verify` locally.
- **TypeScript pinned to 5.9.3** although 7.x exists on npm: not tried with Next.
- **Favicon PNGs (16, 32, 64) and the 180 px Apple icon were rendered from the supplied favicon SVG** (only the SVG was supplied); the Apple icon sits on Cream. Listed in `CONTENT_TODO.md`.
- **Row Level Security and the migrator/app database roles are deferred to Stage 25** (brief section 12 mentions RLS as defence in depth): with RLS and no policies a non-owner app role sees no rows, so the role model and policies must be designed and tested on Supabase together.
- **Brief entity list changed** (`schema-constraints.md`): added `tax_rates` (with treatment), `user_pins`, `login_attempts`, `price_history`, `product_modifiers`, `order_item_modifiers`, `ticket_status_history`, `refund_items`, `sale_voids`, `print_attempts`; `folio_payments` is merged into `payments`; `tables` is `dining_tables`; stock quantities are base-unit integers.
- **Extra permission keys beyond the brief** (S-15), retained by the owner; Business Manager lacks the two role-administration keys (item 6 above).
- **CSRF design is stronger than the double-submit sketch**: the token is bound to the session (ADR-13).
- **Route plan gained `/api/v1/health`** (liveness through the web proxy for the connection indicator).
- **`PORT` is accepted when `API_PORT` is unset** (Render supplies `PORT`; not yet verified on Render).
- **Stage size discipline** (prompt section 2, about 25 files): Stages 4, 5 and 6 exceeded it (brand assets, lock files, many tests); noted in each stage entry.
- **Seed password constant lives in the `password` package** so the API and `bootstrap` can refuse it without importing seed code; `devseed` still refuses production and databases that hold real users.
- **Some tables were added before the stage the plan named** (settings, tax categories at Stage 5, session CSRF columns at Stage 6) because bootstrap and login needed them.

### 4. Open decisions, assumptions and checks not yet run
- **Open decisions** (details in `docs/OPEN_ITEMS.md`): S-17, S-18, S-21 are OPEN; S-15 retained; T-01 provisional tax configuration is a **go-live blocker**; T-02 explicit tax category per product and room type; T-03 and DG-01 eTIMS route; DG-02 M-Pesa route; DG-03 printer model and connection; DG-04 hosting plans and budget; business details and receipt text (`CONTENT_TODO.md`); the accountant questions in `docs/tax-configuration.md`.
- **Assumptions:** every default in use, with who must confirm, is in `ASSUMPTIONS.md` (A-xx, DQ-xx from the brief; S-01 to S-33 from this build). Only S-13 is confirmed. See its new status table and its interpretations list.
- **Checks not yet run:**
  - Docker: neither Dockerfile target nor Compose has ever been built or started; base-image tags `golang:1.27`, `postgres:16`, `gcr.io/distroless/static-debian12:nonroot` are unchecked. Owner command: `docker compose up --build -d`, then `docker compose run --rm test`.
  - CI: both GitHub workflows have never run; action versions are unverified.
  - `go.sum` against the public checksum database; `sqlc diff` in CI.
  - Database tests on the owner's machine or CI (they ran only in the sandbox on PostgreSQL 16.15).
  - Supabase: `btree_gist`, direct versus pooler connection, `sslmode`, backups, pausing; Render: `PORT`, health check path, always-on plan; Vercel: rewrite forwarding of cookies, `Origin` and `Set-Cookie`, long requests; real client IP behind proxies and `TRUSTED_PROXIES`.
  - Argon2id parameters on the real Render instance size (S-24).
  - Mermaid diagrams were parsed but never rendered visually; the frontend has no automated tests beyond lint, typecheck, build and the brand check; no accessibility or screen-reader test.
  - Hardware and operations: printer (AC-16, DG-03), backup restore drill (AC-15), outage runbook walkthrough (AC-17).
  - Not yet written, so not yet run: tests for idempotency and sale finalisation (Stage 11), the stock ledger (17 and 18), room overlap concurrency (20), folios (21), reports reconciliation (23), the AC-10 route-table test and AC-11 and AC-12 service tests (7).
  - Gated and not built: eTIMS and M-Pesa providers (stubs only at Stage 24).

### 5. Promised for later
- **Stage 7:** permission middleware, user and role admin endpoints, Super Admin safeguards, audit writing, `PUT /users/{id}/pin`, the settings endpoint that marks the tax configuration verified (system.admin, password re-entry, audited), the AC-10 route-table test and AC-11 and AC-12 service tests.
- **Stage 8:** login and app screens, permission-aware navigation, API client with the idempotency helper, connection indicator linking to the outage procedure; check the Vercel rewrite on a real preview deployment (Stages 6 to 8).
- **Stages 9 and 20:** products and room types get a NOT NULL tax category with no default. **Stage 11:** sale lines snapshot tax treatment and rate; reports break tax down by treatment. **Stage 10:** confirm S-17. **Stage 12:** confirm S-21. **Stage 15:** confirm S-18. **Stage 14:** `printer-test-sheet.md`. **Stage 17:** decide the stock balance strategy. **Stage 24:** ask the owner for verified eTIMS (DG-01) and M-Pesa (DG-02) facts; no provider code without them.
- **Stage 25:** Row Level Security and database roles on Supabase, verify `TRUSTED_PROXIES` on the deployed stack, security headers and CSP, secrets scan, dependency audit, choose an error-monitoring tool, OpenAPI completeness, final AC-xx traceability pass.
- **Stage 26:** deployment guides (Render, Vercel, Supabase, Cloudflare), backup and restore runbook, Super Admin recovery CLI, printer-failure and outage runbooks, staff training guide, handover checklist, go-live clean-up (no test users, no dev password works, fresh production database, counters continue the receipt book, Test environment banner gone, `tax_config_status` verified).
- **Every stage:** keep listing the database-dependent invariants as unrun anywhere beyond the sandbox until they run on the owner's machine or CI; keep both READMEs' run steps current; refresh `OPEN_ITEMS.md`.
- **Other:** swap in designer-made favicon PNGs if supplied; remove stale monorepo wording from the older docs; consider re-checking TypeScript 7 with Next.

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

## Stage 6: Authentication (done) and tax configuration
- **Owner instructions applied:** (1) `--tax-rate-bp` now defaults to 1600 as a PROVISIONAL development value, never a confirmed production tax configuration; the rate stays configuration (`TestTaxRateIsNotHardCoded` forbids the literal anywhere else); three tax categories exist (standard, zero-rated, exempt; migration 0005 with database CHECKs); `tax_config_status = provisional_unverified` and a loud bootstrap notice; products and room types must name a category explicitly (no implicit standard rate); `docs/tax-configuration.md` holds the go-live verification checklist (VAT registration, KRA requirements); no eTIMS compliance is claimed anywhere. (2) S-17, S-18, S-21 stay OPEN in `docs/OPEN_ITEMS.md`; S-15 retained as seeded after a conflict check (none found).
- **Built:** login, logout, me, change-password, reauth, pin-switch endpoints; Argon2id verification with a timing-equalising dummy hash and rehash-on-login; opaque 256-bit session tokens (only SHA-256 stored); sliding idle expiry and a hard absolute expiry; permissions and roles read from the database on every request; lockout (5th failure locks 15 min), per-username throttling for unknown names, per-IP limit; session-bound CSRF token plus Origin/Referer check (ADR-13 accepted); forced password change gate; password re-entry window (5 min, S-16); PIN fast-switch for floor roles (A-07); audit of security events with request ID and IP and no secrets; `internal/platform/auth` Principal for other modules; `TRUSTED_PROXIES` and `LOGIN_IP_MAX_ATTEMPTS`; migrations 0005 to 0007; OpenAPI auth paths (validated with swagger-parser).
- **Verified in the sandbox:** in a fresh clone with PostgreSQL 16.15: 94 Go tests pass, 0 fail, 0 skipped (without TEST_DATABASE_URL: 42 pass, 52 skip loudly). `go mod tidy` no diff, gofmt, vet and `sqlc diff` clean. Two mutation checks (weaken CSRF; remove the PIN expiry bound) made their tests fail. End-to-end with the real binaries and `npm run dev`: migrate (0001 to 0007), bootstrap without a tax flag (provisional notice and three categories), login through the Next proxy returned both cookies (session HttpOnly), CSRF rejections, password change, logout, 401 afterwards.
- **Bug found by the tests and fixed:** the last-Super-Admin trigger blocked deactivating any user when no Super Admin existed. Migration 0007 makes it act only for Super Admins (`TestGuardIgnoresUsersWhoAreNotSuperAdmins`).
- **Not verified:** Docker artefacts and CI (no Docker or GitHub here); Vercel and Render behaviour (cookies, Origin, Set-Cookie through the rewrite; real client IP behind proxies); Supabase; `go.sum` against the checksum database. No frontend change this stage, so the frontend repo has no new commit. Login SCREENS are Stage 8.
- **Decisions and assumptions:** S-26 revised; S-29 to S-33 added (ASSUMPTIONS.md); ADR-13 accepted.
- **Next stage needs:** nothing blocking. Stage 7: permission middleware, user and role admin endpoints, Super Admin safeguards, audit writing, the tax-verification settings endpoint, and the AC-10, AC-11, AC-12 service tests.

## Stage 7a: User administration (done, backend only)
- **Built:** the ten `/users` routes (list with keyset paging, create, get, patch display name, roles, deactivate, reactivate, reset-password, PIN, delete); service-level permission checks plus route middleware; protected-account rule (only `system.admin` touches a Super Admin); no-escalation rule; password re-entry for roles, status, reset and delete; self-action refusal; deactivation revokes sessions in the same transaction; delete only for accounts with no history; audit rows with before and after values and no secrets; 13 sqlc queries; OpenAPI paths.
- **Verified in the sandbox (PostgreSQL 16.15):** the new tests pass: `TestAC10_UserAdminRoutesDenyWithoutPermission`, `TestAC11_ManagerHasNoRoleAdminPermissions`, `TestAC11_ManagerCannotGrantOrTouchSuperAdmin`, `TestAC11_RoleGrantNeedsHeldPermissions`, `TestUserAdminLifecycle`, `TestReauthRequiredForSensitiveUserChanges`, `TestAC12_LastSuperAdminCannotBeRemoved`, `TestAC12_ConcurrentDeactivationLeavesOne` (5 rounds), `TestAC12_DeactivationKeepsHistory`. Mutation checks (guard removed) made the tests fail. OpenAPI validates.
- **Decisions:** S-34 (approved by the owner: a Super Admin may grant any role; everyone else only roles whose permissions they hold), S-35 to S-37 (default in use).
- **Not verified:** Docker and CI for this change; the service-level AC-11 test about a role administrator (needs the 7b role endpoints).
- **Next stage needs:** nothing blocking. Stage 7b.

## Stage 7b-1: Roles and permissions (done, backend only)
- **Built:** `GET/POST /roles`, `PATCH/DELETE /roles/{id}`, `PUT /roles/{id}/permissions`, `GET /permissions`; protected roles and `system.admin` untouchable; no-escalation for permission grants; password re-entry and audit with before/after; affected users returned; 9 sqlc queries; OpenAPI paths.
- **Verified in the sandbox:** `TestRoleAdminLifecycle`, `TestAC11_RoleAdminWithoutSystemAdminCannotGrantIt`; mutation checks (escalation, protected permission, protected role) make them fail. CI for Stage 7a was reported green (go and docker jobs) by the owner.
- **Decisions to confirm:** S-38, S-39.
- **Next:** Stage 7b-2: settings, tax-rates, the tax-verified status change, audit-logs (security vs operational scope), and `TestAC10_EveryProtectedRouteDeniesWithoutPermission` over `routes.csv`.

## Stage 7b-2: Settings, tax categories, audit log, AC-10 route table (done, backend only)
- **Built:** `GET/PATCH /settings`, `GET/POST /tax-rates`, `PATCH /tax-rates/{id}`, `GET /audit-logs` (security vs operational scope); the tax-verified status change through `PATCH /settings` (records who and when, own audit action); settings module with `platform/audit.Recorder`; `TestAC10_EveryProtectedRouteDeniesWithoutPermission` reading `routes.csv`; OpenAPI paths and docs.
- **Verified in the sandbox:** `TestSettingsAndTaxLifecycle`, `TestAuditLogsScope`, `TestAC10_EveryProtectedRouteDeniesWithoutPermission` pass; mutation checks (route unregistered, security scope removed, re-authentication removed, auto-unverify removed) make them fail.
- **Decisions to confirm:** S-40 to S-42.
- **Stage 7 is complete with this patch.** Next: Stage 8, the frontend shell (login, layout, permission-aware navigation, users and roles screens), which starts in the frontend repo against the openapi.yaml in this one.

## Stage 8, backend part: assignable roles
- **Built:** `GET /api/v1/roles/assignable` (users.update): the roles the caller may grant, by the same rule as user role changes, so a Business Manager (no `roles.manage`) can fill in a role picker. Row added to `routes.csv` (stage 8); the AC-10 route-table test now covers stage 8 rows. Test `TestAssignableRolesFollowTheGrantRule`; mutation check (filter removed) fails it.
- **Frontend expectation:** the users screens call this endpoint for the role picker; `GET /roles` is used only by the roles screen (roles.manage).

## Stage 8c (frontend repo): roles, audit log, settings and tax configuration screens
- **Built (frontend):** `/roles` (roles list, permission checkboxes grouped by area, add, edit, delete, save with the affected users shown; protected roles and protected permissions are read-only); `/audit` (filters for action, record type, result and dates, paging, before/after details); `/settings` with the tax configuration card: provisional or verified badge, who and when once verified, the tax categories as they are now, a plain note that the business may not be VAT-registered, the seven go-live items from `docs/tax-configuration.md`, and "Mark as verified", disabled until all seven are ticked, then a confirmation dialog and password re-entry. Navigation entries: Roles (`roles.manage`), Audit log (`audit.view`), Settings (`system.admin`).
- **Fix (frontend):** the sign-in page logo showed as a broken image because the signed-out redirect in `proxy.ts` also caught `/brand/*`; `/brand/` is now excluded from the matcher.
- **Frontend expectation (no backend change):** uses `GET/POST /roles`, `PATCH/DELETE /roles/{id}`, `PUT /roles/{id}/permissions`, `GET /permissions`, `GET /audit-logs`, `GET/PATCH /settings`, `GET /tax-rates` exactly as in `openapi.yaml`. The checklist ticks are not stored; the verification is recorded by the backend (who, when, audit event `settings.tax_config_verified`).
- **Assumptions used:** Settings is shown in the menu to `system.admin` only (the API lets any signed-in user read settings); the checklist text is a copy of `docs/tax-configuration.md` and must change with it.
- **Stage 8 is complete with this patch.** Next: Stage 9 (catalogue: categories and products, with an explicit tax category on every product).

## Stage 9a (backend): catalogue API
- **Built:** migration `0008_catalogue.sql` (categories, products, price_history, modifiers, product_modifiers); sqlc queries; `internal/catalogue` (service, handler); routes `GET /menu` (orders.manage) and, with catalogue.manage, categories (list, add, change), products (list with filters, add, change, price, availability, price-history), modifiers (list, add, change). `openapi.yaml` and `routes.csv` updated (one added row: `GET /products/{id}/price-history`).
- **Rules held by the database and tested:** a product's module must match its category's module (composite foreign key); `price_history` rejects UPDATE, DELETE and TRUNCATE; a product must name an active tax category, with no default; prices are integer cents, 0 to 100,000,000.
- **Audit events:** category.created/updated, product.created/updated/price_changed/availability_changed, modifier.created/updated.
- **Not in 9a:** `track_stock` and `inventory_item_id` on products (they arrive with inventory in Stage 17); the seeded sample menu and the catalogue screens (9b and 9c).
- **You must run a migration:** `docker compose run --rm backend /app/migrate up` (0008) before the frontend catalogue screens are merged.

## Stage 9b (backend): sample menu in seed-dev
- **Built:** `seed-dev` now also adds a fake sample menu (restaurant: Mains, Sides, Hot drinks; bar: Soft drinks, Beer, Spirits; 11 products with SKUs `DEV-...`, 6 modifiers linked to products, an opening price_history row each). It only runs when the database has no categories, so a repeat run never duplicates or resets edits. Bootstrap (production-style) data is unchanged: no menu.
- **Dev placeholder:** every sample product uses the `standard` tax category only because a product must name one. It is not a tax decision (`docs/tax-configuration.md`).
- **Tests:** `TestSeedAddsSampleMenuOnceAndConsistently` (size, fake SKUs, tax category and price on every row, price history, repeat-run safety); also checked by breaking the re-run guard on purpose, which made it fail.
- **Traceability:** no acceptance criterion maps to the catalogue, so `docs/traceability.md` is unchanged.
- **Next:** 9c, the catalogue screens (frontend).

## Stage 9c (frontend repo): catalogue screens
- **Built (frontend):** `/catalogue` with three tabs. Products: search, filter by module, category and availability; add and edit; change price (shows the earlier prices); one-press Available or Sold out. Categories: add, edit, order, deactivate. Modifiers: add, edit, signed price change, deactivate. Menu entry "Catalogue" for `catalogue.manage`.
- **Tax and kitchen are never defaulted:** the product form starts with "Choose a tax category" and "Choose" for the kitchen decision, and the Add button stays disabled until both are chosen.
- **Money:** `lib/money.ts` turns text into whole cents without floating point (rejects more than two decimals, negatives for prices, and amounts above KES 1,000,000).
- **Frontend expectation (no backend change):** uses exactly the Stage 9a routes in `openapi.yaml`. Checked against a real API with the seeded menu: login through the frontend proxy, lists (6 categories, 6 modifiers, 11 products), edit, price (history shows both prices), availability and the three create calls.
- **Also fixed here:** the 9b notes said 10 sample products; the seed adds 11.
- **Not covered:** no browser or screen-reader pass yet, and the repo has no frontend test runner.
- **Next:** Stage 10 (tables, orders, kitchen tickets).

## Stage 10a (backend): dining tables and orders
- **Built:** migration `0009_tables_orders.sql` (dining_tables, orders, order_items, order_item_modifiers); `internal/orders` (service, handler); routes for tables (list, add, change) and orders (list, open, get, add item, change item, cancel item, cancel order). `seed-dev` adds six fake tables. `openapi.yaml` updated; `routes.csv` unchanged.
- **Rules held by the database and tested:** a dine-in order has a table and a takeaway order has none; a cancelled or finalised order accepts no change, no new items, and is never deleted; an item's order, product and price never change; the server copies the price from the catalogue when a line is added.
- **Order numbers:** from the gapless `order` counter inside the same transaction (A-05). Tested with 10 simultaneous orders: numbers 3 to 12, no gaps, no repeats.
- **Concurrency:** every change locks the order row first; the optional `expected_version` returns 409 `order_changed` when someone else edited first.
- **Assumption S-43 (owner to confirm):** Waiter, Bartender and custom roles see and change only their own orders; Super Admin, Business Manager, Cashier and Accountant / Auditor see all. Another person's order answers 404.
- **Not in 10a (arrives in 10b):** send-to-kitchen, kitchen tickets and their numbers, the `Idempotency-Key` handling (`POST /orders` ignores the header for now), cancelling an order that was already sent (409 `order_sent`), and cancel tickets for sent lines. The route-denial test skips those routes until then.
- **You must run a migration:** `docker compose run --rm backend /app/migrate up` (0009).
