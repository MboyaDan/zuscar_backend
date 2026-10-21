# Performance notes

## What was measured (Stage 26 follow-up, local PostgreSQL 16)

Database statements per API request, counted from the server log:

| Request | Before | After |
|---|---|---|
| `GET /auth/me` | 4 | 1 |
| `GET /sales` | 4 | 2 |
| `GET /inventory/balances` | 4 | 2 |
| `GET /rooms/in-house` | 6 | 4 |
| `GET /menu` | 7 | 5 |
| `GET /reports/sales` | 9 | 7 |

Why it matters: on a laptop a statement costs well under a millisecond. In production the API (Render) and the database (Supabase) are in different data centres, so each statement is a network round trip of tens of milliseconds, and they run one after another. Three of the four statements on a plain request were the sign-in check (session, roles, permissions). They are now one query (`GetSessionPrincipal`). Roles and permissions are still read from the database on every request, never cached, so a permission change applies on the next request (AC-10).

## Where the rest of the time goes on free tiers

1. **A sleeping API.** A free Render service sleeps after about 15 minutes without a request and takes up to a minute to wake. This is the biggest single cause of "slow". Fix: the keep-awake ping below.
2. **Distance between Render and Supabase.** Render has no Ireland region, so a Supabase project in `eu-west-1` (Ireland) talks to a Render service in Frankfurt across a border, on every statement. Putting the database in `eu-central-1` (Frankfurt) removes that. It is cheap to do before there is real data: create a new Supabase project in Frankfurt, then repeat the migrate, bootstrap and security SQL steps from the go-live guide, and update `DATABASE_URL` in Render and the backup secret in GitHub.
3. **A small shared CPU.** Free instances get a fraction of a CPU. Sign-in uses Argon2id, which is deliberately heavy, so sign-in takes seconds there and nothing else does. Do not weaken the hashing to fix this. Staying signed in all shift (the session lasts hours) means staff sign in once.
4. **Cold database connections.** The first query after a quiet period opens a fresh connection (TLS to the pooler). The ping keeps connections warm.

## Things that were considered and are not the fix

- **Redis or another cache.** At this size the cost is round trips, not heavy queries, and a cache adds a second system to run, secure and keep correct. A stale cache on a till (prices, permissions, stock) is worse than a slow page. Revisit only if a measured query is slow with the data actually in the database.
- **More indexes.** The hot filters already have indexes (session token, sale business date, foreign keys on items and payments, audit logs by actor, action and entity). With a few thousand rows an index will not change anything. When data grows, look at `payments(confirmed_at)` for the collections report first.
- **Connection pooling changes.** The API keeps its own pool already. Do not switch to Supabase's transaction pooler (port 6543): it breaks prepared statements.

## Keep-awake ping (free tier)

Goal: one request to the API every 5 minutes so Render never sleeps, and so Supabase never pauses (`/readyz` runs a database query).

1. Find your API address in Render, for example `https://zuscar-backend.onrender.com`.
2. Use a free scheduler that allows a 5-minute interval. Either:
   - **cron-job.org:** create an account, Create cronjob, URL `https://<service>.onrender.com/readyz`, Execution schedule: every 5 minutes, enable "Notify me when the job fails", save.
   - **UptimeRobot:** create an account, Add New Monitor, type HTTP(s), URL as above, monitoring interval 5 minutes, add your email as the alert contact, save. Check its current free-plan terms for commercial use.
3. Ping the Render address, not the Vercel one.
4. Open the Render dashboard, Logs, and confirm `GET /readyz 200` lines appear every 5 minutes.
5. Free Render gives 750 instance hours a month for the whole workspace. One service kept awake all month uses about 744, so it only fits if it is the only free service in that workspace. A second always-on free service will make both stop.
6. The ping does not help when Render restarts the service (it may do so at any time) or during a deploy. Staff should open the app a few minutes before opening time.
7. When the budget allows, the first upgrade is Render Starter (always on, more CPU), which makes the ping unnecessary.
