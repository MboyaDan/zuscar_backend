# ADR-05: Next.js rewrite proxy to the API

Status: Accepted

## Context
Avoid cross-site cookies and broad CORS between vercel.app and onrender.com.

## Decision
The browser calls /api/* on the Next.js origin; a rewrite forwards to the Go API (BACKEND_URL). The API sets cookies for that single origin.

## Consequences
- Verified in Stage 4: the rewrite destination is baked into `.next/routes-manifest.json` at build time, so `BACKEND_URL` must be set when the web app is built (a Vercel build environment variable, a Docker build argument). Changing it needs a rebuild.
- Verified in Stage 4 against a local standalone build: a request to `/api/v1/...` on the Next origin reached the Go API and returned its JSON error body with the request ID forwarded.
Rewrite behaviour with cookies, request size and timeouts must be checked on a deployed Vercel preview (unverified until then, see deployment/topology.md).
