# ADR-07: Append-only stock ledger with compensating entries

Status: Accepted

## Context
Every balance must be explainable (NN-08, AC-06).

## Decision
inventory_movements has a trigger rejecting UPDATE and DELETE. Balances are derived from movements. Corrections are compensating movements with reasons.

## Consequences
Balance queries need indexes or a maintained summary; decide at the inventory stage.
