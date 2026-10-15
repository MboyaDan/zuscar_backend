# ADR-13: CSRF protection design

Status: Accepted (Stage 6)

## Context
SameSite=Lax alone is not enough for state-changing requests. Requests reach the API through the Next.js rewrite, so the browser origin is the web app's origin.

## Decision
- At login the server creates a random CSRF token, stores only its SHA-256 on the session (`sessions.csrf_hash`) and sets it in a script-readable cookie `zuscar_csrf`.
- Every state-changing request on an authenticated route must send the same value in `X-CSRF-Token`; it must equal the cookie AND hash to the value stored on that session. A cookie planted by an attacker therefore fails, because it is not bound to the session.
- Additionally, if an `Origin` header is present it must equal `FRONTEND_URL`; if there is no Origin, a `Referer` must match; if neither exists (scripts, curl) the token check still applies.
- The login endpoint has no session yet, so it applies only the Origin/Referer check.
- The session cookie is `zuscar_session`: HttpOnly, SameSite=Lax, Secure when `COOKIE_SECURE=true`, Path=/.

## Consequences
- Verified locally (tests and a run through the Next.js dev proxy): missing, wrong, foreign-session, forged-pair, foreign-Origin and foreign-Referer requests are all rejected with 403 csrf_failed, and a valid request passes.
- Not verified: that Vercel's rewrite forwards the Origin header and Set-Cookie unchanged on a deployed project. If it strips Origin, the token check still protects, but the Origin check would not.
