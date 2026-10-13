# ADR-11: Go toolchain and module sourcing

Status: Accepted (Stage 4)

## Context
Brief and prompt require Go with Gin and pgx. Latest Gin (v1.12.0) and pgx/v5 (v5.11.0) need Go 1.25 or later. The authoring sandbox only offers Go 1.24 through apt and blocks proxy.golang.org, go.dev and vanity import domains.

## Decision
- The project targets Go 1.27 (`go 1.27` in backend/go.mod; Docker builds with `golang:1.27`, tag unverified). Latest GitHub tag seen on 2026-10-13: go1.27.2.
- In the sandbox, Go 1.27.2 was built from the GitHub source using the apt Go 1.24.13 as bootstrap (`go version` reports go1.27.2). `go.mod` contains no replace directives.
- Sandbox module resolution uses a local file proxy built from GitHub mirrors (scripts/sandbox/). It affects only the authoring sandbox.
- Dependencies: gin v1.12.0, pgx/v5 v5.11.0 (exact versions chosen by `go mod tidy`; Next.js 16.4.0 and TypeScript 5.9.3 are pinned in the frontend; TypeScript 7.x exists on npm but was not tried with Next).

## Consequences
- go.sum was generated with GOSUMDB=off: hashes are computed from GitHub tags using the standard module zip algorithm but are not confirmed against the public checksum database. On a normal machine run `cd backend && go mod tidy && go mod verify`; any diff in go.mod or go.sum means the sandbox copy was wrong and your result wins. CI repeats this check.
- Go 1.27.x is the newest release seen; if the owner prefers the previous release, change `go` in go.mod and re-tidy.
