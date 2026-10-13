# ADR-09: Print is separate from persistence

Status: Accepted

## Context
Printer faults must not corrupt sales (NN-05, AC-03).

## Decision
Sale and ticket commit first. print_jobs record attempts; retry affects only the job; reprints link to the original and carry a REPRINT marker.

## Consequences
Browser printing first; a Windows print bridge is only considered after DG-03.
