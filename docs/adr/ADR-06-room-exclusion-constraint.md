# ADR-06: Exclusion constraint for room overlap

Status: Accepted

## Context
Availability checks alone fail under concurrency (brief 10.3).

## Decision
btree_gist exclusion constraint on (room_id, daterange(check_in, check_out, '[)')) for blocking statuses only. Blocking statuses are configuration-in-schema per DQ-09 default: confirmed and checked_in.

## Consequences
Sandbox check on 2026-10-12: PostgreSQL 16.15 with btree_gist rejected an overlapping range and accepted back-to-back ranges. Availability of btree_gist on the chosen Supabase plan is still unverified.
