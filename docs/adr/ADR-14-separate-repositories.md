# ADR-14: Two independent repositories

Status: Accepted (owner decision, Stage 5)

## Context
The owner deploys the frontend to Vercel and the backend to Render from separate Git repositories and wants each to be runnable on its own. Earlier stages (0 to 4) used one monorepo (prompt section 6).

## Decision
- `zuscar-backend` (this repository, folder `backend/` in the delivery zip): Go API, database migrations and queries, Dockerfile and Compose, and ALL project documents (`docs/`).
- `zuscar-frontend` (folder `frontend/`): Next.js app, brand assets, its own `.env.example`, CI and README. No Docker: Vercel builds from source.
- Each folder in the zip is a self-contained Git repository with its own history. Stages 0 to 4 history exists only in the earlier monorepo zips.
- The only runtime coupling is HTTP: the web app forwards `/api/*` to `BACKEND_URL` (ADR-05); the backend checks the browser origin against `FRONTEND_URL`.
- Naming: backend `FRONTEND_URL` (was `APP_ORIGIN`); frontend `BACKEND_URL` (was `API_INTERNAL_URL`) and `NEXT_PUBLIC_APP_URL`.

## Consequences
- Prompt section 6 (monorepo) is superseded for layout; everything else in the prompt stands. Stage zips now contain `zuscar/backend` and `zuscar/frontend`.
- Cross-repo checks cannot be one command. The contract between them is `docs/api/openapi.yaml` and `docs/api/routes.csv` (backend repo); a mismatch is caught by running both locally.
- Both CI workflows exist but have not run.
