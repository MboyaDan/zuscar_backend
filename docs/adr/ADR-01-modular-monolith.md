# ADR-01: Modular monolith in a monorepo

Status: Accepted

## Context
One business, small team (NN-01, brief 17).

## Decision
One Go binary with internal packages per domain; one Next.js app; one repository.

## Consequences
Simple deploys and transactions across modules. Boundaries are enforced by package rules (architecture.md section 3), not by network.
