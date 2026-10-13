# ADR-03: Integer minor units for money

Status: Accepted

## Context
Exact arithmetic (NN-09).

## Decision
BIGINT cents of KES everywhere. Rates are stored as integer basis points. Rounding rule is defined once in a money package and tested. Formatting only at the edge.

## Consequences
No floats in Go, SQL or TypeScript money paths.
