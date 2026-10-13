# ADR-10: Vercel for frontend, Render for API

Status: Accepted

## Context
Owner decision; replaces brief v1 (brief 17).

## Decision
Next.js on Vercel (native build), Go API as a Docker web service on Render.

## Consequences
Two vendors to operate; a frontend Dockerfile is kept for local and integration use only.
