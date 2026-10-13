# ADR-02: Go API is the only database client

Status: Accepted

## Context
Credentials and rules must live in one place (NN-06).

## Decision
Only the Go API connects to PostgreSQL. Browser code never receives database credentials or Supabase keys; the Supabase Data API is not used.

## Consequences
Application tables have RLS enabled with no public policies as defence in depth (verify against current Supabase docs). Every read goes through an authorised API endpoint.
