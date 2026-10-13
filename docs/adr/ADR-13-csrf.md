# ADR-13: CSRF protection design

Status: Proposed

## Context
SameSite=Lax alone is not enough for state-changing requests (brief 6).

## Decision
Double-submit: a non-HttpOnly csrf cookie is echoed in an X-CSRF-Token header on every state-changing request, plus Origin check against the app origin.

## Consequences
Details finalised at Stage 6.
