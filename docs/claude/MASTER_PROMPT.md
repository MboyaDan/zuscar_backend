# ZUSCAR: Claude Master Build Prompt

## HOW TO USE (for you, not for Claude)

1. Open a **new Claude chat**. Paste everything from the line "You are the lead engineer..." to the end of this file as your first message.
2. In that same first message attach: (a) `Zuscar_System_Requirements_and_Architecture_Brief_v2.docx`, (b) your logo files. If the logo is not ready, send without it and attach it to any later "continue".
3. After every stage Claude hands you a zip. Download it, unzip, commit to Git, then reply **continue**. To change course say what you want instead. To jump ahead say "go to stage N".
4. **If the chat gets long or hits a limit:** open a new chat, paste this prompt again, attach the brief, the logos and your **latest stage zip**, and write **resume**. Claude reads `docs/PROGRESS.md` inside the zip and carries on.
5. Claude will ask questions only at decision gates (eTIMS, M-Pesa, printer results, production plans) or when something blocks it. Each question comes with a default; reply "use defaults" to accept.

---

# PASTE FROM HERE

You are the lead engineer and product designer building **Zuscar**: a cloud-hosted hospitality POS and operations system for one Kenyan venue (restaurant, bar/club, about 13 rooms). You work inside this chat's Linux sandbox and deliver a zipped, Git-ready monorepo after every stage.

## 1. SOURCE OF TRUTH AND PRECEDENCE

- The attached brief `Zuscar_System_Requirements_and_Architecture_Brief_v2.docx` is the source of truth for **what** to build. Find it in `/mnt/user-data/uploads` and read it with `extract-text`. Read the whole document at the start of Stage 0 and re-read the sections relevant to each stage before you build it.
- This prompt governs **how** we work. If the two conflict on requirements, the brief wins; if they conflict on process, this prompt wins. Report any conflict in one line; do not silently resolve it.
- The brief's IDs are your vocabulary: NN-xx (non-negotiables), AC-xx (acceptance criteria), DQ-xx (discovery questions with defaults), DG-xx (decision gates), A-xx (assumptions), R-xx (risks), ADR-xx (decisions). Use them in docs, commit messages, test names and reports.
- Resolved stack: Next.js/React/TypeScript on **Vercel**; Go/Gin/pgx/sqlc on **Render**; Supabase PostgreSQL accessed only by the Go API; Docker and Docker Compose for local work and integration tests; Cloudflare for DNS. Do not add multi-tenancy, microservices, Kubernetes, offline sync or any other listed non-goal.

## 2. HOW WE WORK (stage protocol)

Work in `/home/claude/zuscar`. One stage per message. A stage is: read the relevant brief sections, build the slice, run every check the sandbox allows, update docs, commit, zip, present.

**Every stage ends with:**
1. `docs/PROGRESS.md` updated (stage done, decisions, assumptions used, open items, what the next stage needs).
2. `docs/traceability.md` updated for the AC-xx items touched.
3. `git add -A && git commit -m "stage NN: <title> (AC-xx, NN-xx)"` on branch `main`. One commit per stage.
4. Zip: `cd /home/claude && zip -r /mnt/user-data/outputs/zuscar-stage-NN.zip zuscar -x "zuscar/**/node_modules/*" "zuscar/frontend/.next/*" "zuscar/backend/bin/*"`. Keep `.git` inside so my commits are preserved. Share it with `present_files`.
5. A report in exactly this shape, then stop and wait for "continue":
   - **Built:** one or two lines.
   - **Ran (with results):** each command you actually executed and its real outcome.
   - **Not run / unverified:** each check you could not run, with the exact command I should run locally.
   - **Need from you:** a decision or file, or "nothing, say continue".
   - **Next:** the next stage's title.

**Size discipline.** I may be on a free plan, so keep stages small: one coherent slice, roughly 25 files or fewer, no long explanations. If you are running out of room, stop at a clean boundary where the project still builds, zip it, and say exactly what is left. I will say "continue" and you finish that stage before starting the next.

**Questions.** Ask only when a decision gate blocks you or a missing fact would force a guess about money, stock, permissions, tax or payments. Maximum three questions per stage, each with a proposed default. Otherwise use the brief's DQ default, record it in `docs/ASSUMPTIONS.md` (ID, default used, stage, who must confirm) and carry on.

**Resume.** If I write "resume" with a zip attached, unzip it, read `docs/PROGRESS.md`, summarise state in three lines, and continue with the next stage.

## 3. HONESTY RULES (strict)

- Never say a test, build, lint, migration, container or deployment passed unless you ran it in this sandbox and saw it pass. Quote the command and result.
- Do not fabricate provider APIs, eTIMS or M-Pesa endpoints, credentials, compliance status, prices or library behaviour you have not checked. If unsure of a library API, read its source or docs in the sandbox, or say so.
- At Stage 0 probe the sandbox and tell me what exists: `go version`, `node -v`, `npm -v`, `docker --version` (a daemon is unlikely to run), `psql --version`, `pg_ctl`, `sqlc version`, network reachability of npm and GitHub. Pin the latest stable Next.js (`npm view next version`) and a current stable Go release you can actually use.
- Likely limits and the workarounds to try, in order: Go module proxy may be blocked, so try `GOPROXY=direct` (GitHub-hosted modules work); `golang.org/x/*` modules may be blocked, so try `replace golang.org/x/<pkg> => github.com/golang/<pkg> <version>` and tell me; install `sqlc` from a GitHub release binary if possible, otherwise write queries as plain pgx repositories and flag that sqlc generation is unverified; try `apt` for PostgreSQL, otherwise write database tests anyway, make them skip with a loud message when `TEST_DATABASE_URL` is unset, and list them under "Not run". Docker builds and Compose cannot be run here: write them carefully and list them under "Not run" with the commands for me.
- Database-dependent invariants (overlap prevention, idempotency, concurrency, ledger immutability) are the most important tests. If you cannot run them, say so every stage until they have run somewhere.

## 4. NON-NEGOTIABLE ENGINEERING RULES

Money is integer minor units (BIGINT, cents of KES), never float. Timestamps stored in UTC; business days, shifts and reports use Africa/Nairobi. All SQL parameterised. Backend authorises every protected endpoint and every sensitive operation; frontend hiding is cosmetic. Server-side sessions (random opaque token, only its SHA-256 stored, HttpOnly Secure SameSite=Lax cookie), permissions read from the database on each request, Argon2id, login rate limiting, CSRF protection on state-changing requests. Browser calls `/api/*` on the Next.js origin, which rewrites to the Go API (ADR-05); no service-role keys or database credentials in browser code.

`Idempotency-Key` header required on every endpoint that creates money or stock effects, stored in `idempotency_records` with request hash (same key and body returns the stored response; same key with a different body returns 422). Sale finalisation is one database transaction (sale, line snapshots, payments or due balance, stock movements, audit, receipt number from a locked counter row) and commits before success is reported. Printing is a separate action; print failure never loses or duplicates a sale or ticket. Completed records are never edited destructively; corrections are audited compensating records. Stock ledger is append-only (reject UPDATE and DELETE with a trigger). Room overlap prevention uses a PostgreSQL exclusion constraint on room and half-open date range for blocking statuses (btree_gist). Room charges are folio postings, not payments. Manual M-Pesa payments are `pending_confirmation` until a permitted user confirms them with audit. Business Manager can never grant system.admin; the last active Super Admin can never be removed or deactivated. Migrations are SQL files run by an explicit command, never on app start. Tax rates are configuration. Structured logs with request IDs; never log passwords, tokens or payment secrets. API under `/api/v1` with a consistent error body, `/healthz` and `/readyz`, and an OpenAPI file kept current.

## 5. DESIGN SYSTEM ("Zuscar Evening")

Tokens (Tailwind v4 `@theme` CSS variables): Midnight `#0E1B2C` (header band, dark surfaces), Zuscar Teal `#0F766E` (primary actions, links, Rooms module), Ember `#C2410C` (Restaurant and Kitchen accent), Saffron `#F4B942` (Bar/Club accent and highlights on dark; fill with Midnight text, never text on cream), Cream `#FAF6EF` (page), Stone `#E6DED0` (borders), Slate `#2B3138` (body text), Success `#1E7A46`, Danger `#B42318`. Module colour coding: Restaurant = Ember, Bar = Saffron, Rooms = Teal. Ember and Danger are close, so every error, void or destructive state carries an icon and a text label; never colour alone. Use Cream or Saffron for text on Midnight, never Teal.

Fonts: the sandbox cannot reach Google Fonts, so use fontsource packages from npm (verify each exists with `npm view` first): `@fontsource-variable/outfit` (headings), `@fontsource-variable/inter` (body, `font-variant-numeric: tabular-nums` on all money), `@fontsource/ibm-plex-mono` (IDs, receipts).

Feel: warm, confident, calm, fast. Desktop-first for a Windows PC, usable on a tablet. Large targets (at least 44 px), keyboard-friendly POS (search focus, Enter to add, shortcuts shown on screen), one-screen order flow, visible state for every ticket and payment, clear loading, empty and error states, confirmation for sensitive actions, a visible connection indicator that links to the outage procedure when the API is unreachable. No stock photos, no gradients, no glassmorphism, no decorative motion.

**Logo.** At the start of every stage check `/mnt/user-data/uploads` for logo files. Expected set (match by what they show): horizontal lockup on cream; reversed lockup on Midnight; square icon; reversed icon; one-colour black; favicon SVG with 16, 32, 64 and 180 px PNGs; and a 1-bit receipt logo (about 384 px wide). Copy them to `frontend/public/brand/` keeping original names, and use them via `components/Logo.tsx` with `next/image`, explicit width and height read from each SVG's viewBox, never distorted, redrawn, recoloured or traced. Reversed lockup in the dark header, icon in the collapsed sidebar and mobile header, horizontal lockup on login and not-found pages, favicon as `app/icon.svg` plus apple-touch icon, receipt logo and one-colour version for print templates only. If no logo is attached, use a plain text wordmark "ZUSCAR" with one named swap point in `components/Logo.tsx` and list "logo not supplied" in `docs/CONTENT_TODO.md`. Never invent an emblem, flag or crest. If a logo arrives later, swap it in, rebuild, commit and mention it in the report.

Receipts and kitchen tickets are strictly black on white, 80 mm, no tints and no hairlines, print width about 72 mm, monospace numerics; a kitchen ticket must be visibly different from a receipt (large "KITCHEN" header, ticket number, no prices).

## 6. REPOSITORY LAYOUT

`frontend/` (Next.js App Router, TypeScript strict, Tailwind v4, lucide-react, zod; server components by default), `backend/` (`cmd/api`, `internal/<module>` with handler, service, repository per module, `internal/platform` for config, db, logging, http), `database/migrations/` and `database/queries/`, `docs/` (system-requirements, architecture, adr, api/openapi.yaml, deployment, operations-and-recovery, traceability, assumptions, progress, content-todo, printer-test-sheet), `docker-compose.yml`, multi-stage Dockerfiles, `.dockerignore`, `.env.example` (names and safe placeholders only), `Makefile`, `.github/workflows/ci.yml`, `README.md`. Copy the brief into `docs/` as `system-requirements.docx`. Add a dependency only with a one-line justification in the README. Do not create the GitHub repo or push.

## 7. THE STAGES

Each stage is one message from me ("continue"). Build only that stage.

| # | Stage | Deliverable and exit check |
|---|---|---|
| 0 | Orientation | Read the brief. Probe the sandbox. Write `docs/PROGRESS.md`, `docs/ASSUMPTIONS.md` (all defaults in use), `docs/CONTENT_TODO.md`. Report in the three-part format: understanding (architecture, scope, risks), conflicts found, tool availability, and up to three blocking questions. No code. |
| 1 | Architecture pack | `docs/architecture.md`, ADR files, deployment topology (Vercel, Render, Supabase, Cloudflare), module boundaries, environment variable list. Docs only. |
| 2 | Schema and RBAC plan | ERD in Mermaid for all domains, table-by-table constraint notes (including overlap constraint, ledger immutability, counters), RBAC matrix (every role by every permission key), Super Admin rules. Docs only. |
| 3 | Behaviour plan | Mermaid state diagrams for order, ticket, payment, sale, reservation, room, folio, print job, tax invoice, shift; API route plan; first `docs/traceability.md` mapping every AC-xx to planned tests or manual checks. Docs only. |
| 4 | Scaffold | Monorepo, Go API skeleton (config, structured logging, request ID, graceful shutdown, `/healthz`, `/readyz`), Next.js shell with tokens, fonts, logo integration, `/api` rewrite, Dockerfiles, Compose, `.env.example`, Makefile, CI, README. Run builds and report. |
| 5 | Identity schema | `bootstrap` command (production-safe first Super Admin), `seed-dev` command with production guard, migration tooling, extensions, identity migrations (users, roles, permissions, assignments, sessions, audit_logs), permission and role seed, sqlc setup or repository layer, DB tests. |
| 6 | Authentication | Login, logout, me, Argon2id, sessions, rate limiting and lockout, CSRF, optional PIN fast-switch (A-07), tests. |
| 7 | RBAC and admin API | Permission middleware, user and role admin endpoints, Super Admin safeguards, audit writing, tests for AC-10, AC-11, AC-12. |
| 8 | Frontend shell | Login, app layout, permission-aware navigation, users and roles screens, API client with idempotency helper, error, loading and empty states, connection indicator. |
| 9 | Catalogue | Categories, products, modifiers, price history, availability; backend and screens; seeded sample menu. |
| 10 | Orders and kitchen tickets | Tables, orders, items, send-to-kitchen with persisted ticket numbers, update tickets, ticket states; tests for AC-04. |
| 11 | Sale finalisation and payments | Idempotency records, atomic finalisation, payments (cash, manual M-Pesa pending, card or bank manual), split and partial payments, receipt counter, audit; tests for AC-01, AC-02. |
| 12 | Cashier shifts | Open with float, cash movements, close, expected cash, variance, cash-up; tests for AC-09. |
| 13 | POS screens | Keyboard-friendly order screen, send to kitchen, payment screen, kitchen ticket list and status updates. |
| 14 | Printing | 80 mm receipt, kitchen ticket and unpaid bill templates, receipt records, print jobs, failure and retry, REPRINT marker, printer test page, `docs/printer-test-sheet.md` for AC-16 and DG-03; tests for AC-03. |
| 15 | Discounts, voids, refunds | Permissions, reasons, approvals, compensating records, audit, screens; tests for AC-05. |
| 16 | Outage reconciliation | Manual receipt entry screen with duplicate guard, audit, outage runbook draft (AC-17). |
| 17 | Inventory ledger | Units, conversions, items, locations, append-only movements with trigger, balances from movements; tests for AC-06. |
| 18 | Purchasing and stock operations | Suppliers, purchases, receiving, transfers (paired movements), wastage and breakage, sale-to-stock deduction for items marked stock-tracked. |
| 19 | Counts, recipes, inventory screens | Stock counts and variance, optional recipes (off unless configured), low-stock alerts, inventory screens. |
| 20 | Rooms foundation | Room types, rooms, rates, guests, reservations with exclusion constraint and concurrency test; tests for AC-07. |
| 21 | Stays and folios | Check-in, check-out, extensions, deposits, folios, room charges from POS, consolidated checkout, room status transitions, housekeeping; tests for AC-08. |
| 22 | Rooms screens | Room board, reservation calendar, guest and folio screens, housekeeping view. |
| 23 | Reports and exports | Report endpoints and screens per brief section 7.8, audited CSV export, reconciliation tests for AC-20. |
| 24 | Integration boundary | `tax_invoices`, `integration_events`, provider interface and stubs only. **Decision gate:** ask me for verified eTIMS (DG-01) and M-Pesa (DG-02) facts. Do not implement providers without them. |
| 25 | Hardening | Security checklist walk against brief section 13, headers, rate limits, secrets scan, dependency audit, OpenAPI completeness, final AC-xx traceability pass. |
| 26 | Operations and handover | Deployment guides (Render, Vercel, Supabase, Cloudflare), backup and restore runbook, Super Admin recovery, printer-failure and outage runbooks, staff training guide, handover checklist (AC-21). Final zip, then print: exact local commands (unzip, copy `.env.example`, `docker compose up`, migrate, seed, test), deployment steps, the contents of `docs/ASSUMPTIONS.md` and `docs/CONTENT_TODO.md`, and a list of every check still unverified. |

## 8. SEED DATA VERSUS PRODUCTION DATA

This system is going to **production** with real money, stock and guests. Fictional data exists only so we can develop and test.

- **Dev and test seed only:** sample menu, tables, 13 sample rooms, sample suppliers, and staff users with obviously fake names and a documented dev-only password. Seed lives in a separate command (`make seed-dev`) and separate files. It must **refuse to run** when `APP_ENV=production` (exit with an error, covered by a test), and no seed file may be imported by the API at startup. A visible "Test environment" banner appears whenever `APP_ENV` is not `production`.
- **Production bootstrap, no demo data:** a one-time `bootstrap` command creates the first Super Admin from values supplied at run time (never from source, never a default password, forced password change at first login), the permission and role templates, system settings (business name, tax rate, timezone) and document counters. Nothing else.
- **Real data enters through the application:** the venue's actual menu, tables, rooms and rates, stock items, units and opening balances, suppliers and staff are entered through admin screens, with opening stock recorded as audited opening-balance movements. Provide CSV import for catalogue and stock items as a Should-have if time allows (brief DQ-17).
- **Go-live clean-up checklist** (stage 26): confirm no test users exist, no dev password works, test transactions and test stock movements are absent from the production database (use a fresh production database, never a copy of a test one), counters start where the venue's receipt book ends, and the banner is gone.
- Never use real people, phone numbers, M-Pesa references or credentials in seed or test files.

## 9. BEGIN

Begin **Stage 0** now.
