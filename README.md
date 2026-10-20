# Zuscar backend

Go API for Zuscar, a hospitality POS and operations system for one Kenyan venue. This repository is **independent**: it deploys to Render on its own. The web app lives in the separate `zuscar-frontend` repository (deploys to Vercel).

Project documents (requirements brief, architecture, ADRs, schema, RBAC, API plan, progress, assumptions) live in `docs/` here. Start with `docs/PROGRESS.md`.

**Status: Stage 18.** The API covers sign-in and roles, settings and tax, the catalogue (menu, prices, modifiers), tables and orders, kitchen tickets, sales with payments, cashier shifts, receipt and ticket printing, voids and refunds, outage reconciliation (paper receipts entered after a system outage), and stock (items, units and conversions, places, an append-only movement ledger, opening balances, adjustments and reversals, transfers between places, wastage, and sales that take stock). Rooms, folios, purchasing, stock counts, reports and the payment-provider integrations come in later stages (`docs/PROGRESS.md` has the plan and what each stage did). The web screens live in the separate frontend repository.

## Run it locally (everything in Docker)

You need Docker Desktop (Docker Compose v2). You do **not** need Go installed.

1. **Create your env file.** From this folder:
   - macOS/Linux: `cp .env.example .env`
   - Windows PowerShell: `Copy-Item .env.example .env`

   Open `.env` and change `POSTGRES_PASSWORD`, and put the same password inside `DATABASE_URL` (it is written out in full on purpose, so what the API uses is visible).
2. **Start PostgreSQL and the API:** `docker compose up --build -d`
3. **Check it is alive:** open http://localhost:8080/healthz (shows `{"status":"ok"}`) and http://localhost:8080/readyz (shows `{"status":"ready"}` when the database is reachable). `docker compose ps` should show both containers healthy.
4. **Create the database tables (an explicit step, never automatic):**
   `docker compose run --rm --entrypoint /app/migrate backend up`
   Re-running it is safe; `docker compose run --rm --entrypoint /app/migrate backend status` lists what is applied.
5. **Load data.** Pick ONE:
   - **Fake development data** (9 obviously fake staff users, all named `dev.*`):
     `docker compose run --rm --entrypoint /app/seed-dev backend`
     Every seeded user has the documented dev-only password `DevOnly-Zuscar-2026`. It also adds a small FAKE menu (6 categories, 11 products with SKUs starting `DEV-`, 6 modifiers) when the database has no categories yet; running it again never duplicates or resets the menu. This command refuses to run when `APP_ENV=production` and refuses a database that holds real users.
   - **A clean, production-style start** (one real Super Admin, permission and role templates, business settings, tax rate, counters; nothing else):
     `docker compose run --rm --entrypoint /app/bootstrap backend --username owner --display-name "Your Name" --business-name "Your Venue" --tax-rate-bp 1600`
     It asks for a password at a prompt (minimum 12 characters; typed twice, never stored in a file). `--tax-rate-bp` is in basis points and defaults to `1600` (16.00%). **That default is provisional, for development only**: before go-live the real tax configuration must be verified against your VAT registration and KRA requirements (`docs/tax-configuration.md`). Three tax categories are created (standard, zero-rated, exempt). This system does not integrate with eTIMS, so its receipts are not KRA tax invoices. Add `--receipt-start 5001` to continue your paper receipt book. The new user must change the password at first login.

   You cannot do both on the same database. To switch, wipe it: `docker compose down -v`, then repeat from step 2.
6. **Useful commands:** logs `docker compose logs -f backend` · stop `docker compose down` (data is kept) · stop and wipe `docker compose down -v`.
7. **Try signing in (no screen needed yet).** Seeded with `seed-dev`: user `dev.cashier`, password `DevOnly-Zuscar-2026`. From a terminal (macOS/Linux/Git Bash):
   ```
   curl -i -c jar.txt -H "Origin: http://localhost:3000" -H "Content-Type: application/json" \
     -d '{"username":"dev.cashier","password":"DevOnly-Zuscar-2026"}' http://localhost:8080/api/v1/auth/login
   curl -b jar.txt http://localhost:8080/api/v1/auth/me
   ```
   `me` lists the roles and permissions. A user created with `bootstrap` must change the password first (`POST /api/v1/auth/change-password`, which needs the `X-CSRF-Token` header and the Origin header). Five wrong passwords lock an account for 15 minutes.
8. **Run the tests (including the database tests):** `docker compose run --rm test`

If you have Go 1.27 installed you can also run `go test ./...`, but database tests then **skip** unless `TEST_DATABASE_URL` is set. A skipped test verified nothing.

## How the URLs and secrets are configured (nothing hard-coded)
The API reads everything from environment variables (`internal/platform/config`). Locally, Docker Compose loads them from `.env`. In production, you set the same names in the Render dashboard.

| Variable | Local (`.env`) | Render (production) |
|---|---|---|
| `APP_ENV` | `development` | `production` (the API then refuses insecure settings) |
| `DATABASE_URL` | `postgres://zuscar:...@postgres:5432/zuscar?sslmode=disable` (host `postgres` = the Compose service) | your Supabase connection string (check Supabase's current docs for direct vs pooler and `sslmode`) |
| `DB_SIMPLE_PROTOCOL` | `false` | `true` only if you use a transaction-mode pooler |
| `FRONTEND_URL` | `http://localhost:3000` | your https Vercel URL or custom domain, exactly as the browser shows it |
| `COOKIE_SECURE` | `false` | `true` |
| `PORT` / `API_PORT` | `8080` | Render sets `PORT` itself and the API uses it (verify in Render's docs) |
| `LOG_LEVEL`, `SESSION_IDLE_TTL`, `SESSION_ABSOLUTE_TTL`, `LOGIN_MAX_ATTEMPTS`, `LOGIN_LOCKOUT_WINDOW`, `LOGIN_IP_MAX_ATTEMPTS`, `IDEMPOTENCY_TTL`, `MAX_REQUEST_BYTES` | defaults in `.env.example` | optional |
| `TRUSTED_PROXIES` | empty | leave empty until the real Vercel/Render proxy addresses are verified (per-IP limits depend on it) |

`FRONTEND_URL` must match the browser's address exactly (`localhost` and `127.0.0.1` are different origins): it is what the API compares against for CSRF protection. There is no CORS to configure, because the browser only talks to the web app's own address and the web app forwards `/api/*` to this backend.

## Deploy to Render (outline, unverified)
1. Push this repository to Git. In Render, create a **Web Service** from it with **Runtime: Docker**. The last stage of the `Dockerfile` is the production image, so no target needs to be chosen.
2. Set the environment variables above. Set the health check path to `/healthz`. Use a paid always-on instance for live use.
3. Run migrations and bootstrap **once, from your own machine** (they are explicit operator actions): build the image (`docker build -t zuscar-backend .`) and run
   `docker run --rm -e DATABASE_URL="<supabase url>" --entrypoint /app/migrate zuscar-backend up`, then the same with `-it`, `--entrypoint /app/bootstrap` and the flags as in step 5. Use a **fresh** Supabase database, never a copy of a test one.
4. Copy the Render service URL into the frontend's `BACKEND_URL` on Vercel and redeploy the frontend.

## Layout
`cmd/` (api, migrate, bootstrap, seed-dev) · `internal/` (platform, identity, settings, catalogue, orders, inventory, sales, printing, provision, devseed) · `database/migrations` (SQL, applied in order) · `database/queries` (sqlc) · `docs/` · `scripts/sandbox` (authoring-sandbox helpers only, ignore).

## Dependencies and why
`gin-gonic/gin` (HTTP router, mandated by the brief) · `jackc/pgx/v5` (PostgreSQL driver and pool, mandated) · `golang.org/x/crypto` (Argon2id password hashing) · `golang.org/x/term` (hidden password prompt in `bootstrap`). Generated repository code comes from sqlc (`make sqlc`, not a runtime dependency).
