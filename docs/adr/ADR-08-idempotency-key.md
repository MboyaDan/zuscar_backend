# ADR-08: Idempotency-Key on money and stock endpoints

Status: Accepted

## Context
Safe retries (AC-01, AC-02).

## Decision
Required header; idempotency_records unique on (key, endpoint) stores user, request hash, response and status. Same hash returns the stored response; different hash returns 422. Keys expire after a documented period (default proposed: 24 h, see ASSUMPTIONS).

## Consequences
Clients generate a key per user intent, not per HTTP attempt.
