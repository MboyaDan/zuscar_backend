# ADR-04: Server-side sessions, permissions read per request

Status: Accepted

## Context
Immediate revocation and no stale claims (brief 6).

## Decision
Random opaque token, only SHA-256 stored in sessions, HttpOnly Secure SameSite=Lax cookie. Roles and permissions are read from the database on each request. Argon2id for passwords.

## Consequences
One extra query per request, acceptable at this scale. Idle and absolute expiry are configuration.
