# Assumptions and defaults in use

Every default below keeps the build moving and must be confirmed with the client before go-live. Stage = where it was first adopted.

| ID | Default used | Stage | Who must confirm |
|---|---|---|---|
| DQ-01 | Business name "Zuscar Venue", text receipt header, supplied logo set | 0 | Owner |
| DQ-02 | Windows 10/11, current Chrome or Edge, USB printer | 0 | Owner / IT |
| DQ-03 | Generic 80 mm ESC/POS-class USB printer, browser printing, 72 mm print width | 0 | Owner (DG-03 test) |
| DQ-04 | Kitchen a short walk away, no screen, paper tickets | 0 | Owner |
| DQ-05 | Single ISP, occasional outages, manual procedure | 0 | Owner |
| DQ-06 | Tables numbered 1-20, editable; sample menu in dev seed only | 0 | Owner |
| DQ-07 | Units and conversions configurable per item; sample bottle/crate/shot in dev seed | 0 | Owner / bar manager |
| DQ-08 | Recipes off; deduct per sold unit only for stock-tracked items | 0 | Owner |
| DQ-09 | 13 rooms, 3 types; check-in 14:00, check-out 11:00; `confirmed` and `checked_in` block, `tentative` does not | 0 | Owner |
| DQ-10 | Cash, manual M-Pesa (pending confirmation), manual card/bank | 0 | Owner |
| DQ-11 | No eTIMS assumed; tax-invoice module is a stub (DG-01) | 0 | Accountant |
| DQ-12 | Tax-inclusive prices at configurable rate; gapless receipt numbers; profit = sales - stock cost - expenses, labelled estimated | 0 | Accountant |
| DQ-13 | Starter roles per brief section 4; owner is Super Admin | 0 | Owner |
| DQ-14 | Reports per brief 7.8 | 0 | Owner |
| DQ-15 | No budget assumed; paid always-on tiers for live use | 0 | Owner |
| DQ-16 | No go-live date; pilot after final stage | 0 | Owner |
| DQ-17 | No import in v1; CSV import is a Could | 0 | Owner |
| DQ-18 | Audit logs kept 7 years; daily backups; restore tested before go-live | 0 | Owner / accountant |
| A-01 | KES only, one location, one timezone (Africa/Nairobi) | 0 | Owner |
| A-02 | One shared terminal first | 0 | Owner |
| A-03 | Windows 10/11 with current Chrome or Edge | 0 | Owner |
| A-04 | Prices tax-inclusive; rate is configuration | 0 | Accountant |
| A-05 | Receipt numbers gapless and unique | 0 | Accountant |
| A-06 | Negative stock: warn and require permitted override with reason | 0 | Owner |
| A-07 | PIN fast-switch for floor staff on the trusted terminal | 0 | Owner |
| A-08 | Audit retention at least 7 years | 0 | Accountant |

Build-process assumption (not from the brief): sandbox toolchain versions are whatever installs here (see PROGRESS.md); the owner's local versions are verified through the commands listed in each report.

## Design-stage proposals (not in the brief; added Stage 2)

Defaults chosen by the build to keep going. Each must be confirmed by the named person; none changes a brief requirement.

| ID | Default used | Stage | Who must confirm |
|---|---|---|---|
| S-01 | BIGINT identity primary keys | 2 | Owner (technical) |
| S-02 | Enumerations as TEXT + CHECK, not PG enums | 2 | Owner (technical) |
| S-03 | Stock quantities are integers in each item's base unit; conversions are integer ratios and must come out whole | 2 | Owner / bar manager |
| S-04 | Business settings (name, tax rate, timezone, receipt text) edited only with system.admin | 2 | Owner |
| S-05 | Kitchen ticket numbers restart each business day; other document numbers are continuous | 2 | Owner |
| S-06 | Idempotency keys kept at least 24 h | 2 | Owner (technical) |
| S-07 | Finalised orders locked by trigger as well as service | 2 | Owner (technical) |
| S-08 | sales.payment_status stored explicitly and reconciled against payments by test | 2 | Owner (technical) |
| S-09 | folio_payments represented by payments.folio_id (one table of money received) | 2 | Owner (technical) |
| S-10 | Payment amount, method and target immutable after insert; only status moves | 2 | Accountant |
| S-11 | Guest ID document numbers not collected by default | 2 | Owner (privacy) |
| S-12 | A stay is 1:1 with a reservation; walk-ins create a reservation already checked in | 2 | Owner |
| S-13 | CONFIRMED by owner (Stage 3): Business Manager does NOT hold roles.manage or permissions.assign; only Super Admin does. No-escalation guard still applies if those keys are ever granted elsewhere | 2, 3 | Confirmed |
| S-14 | Super Admin is a non-operational role; the owner also holds Business Manager | 2 | Owner |
| S-15 | Extra permission keys added: sales.reprint, orders.manage, kitchen.view, kitchen.update, catalogue.manage, shifts.manage, inventory.wastage, inventory.override_negative, rooms.view, folios.view, folios.post, audit.view | 2 | Owner |
| S-16 | Sensitive admin actions need password re-entry within 5 minutes | 2 | Owner |
| S-17 | Kitchen tickets may skip statuses forward (e.g. submitted to served) because the default kitchen has no screen | 3 | Owner |
| S-18 | A sale can be voided only from finalised with no refunds; later corrections are refunds | 3 | Owner / accountant |
| S-19 | Room becomes physically reserved on the arrival day only; a reprint job ends in state reprinted (the brief's fourth print state) | 3 | Owner |
| S-20 | A settled folio can be reopened by folios.manage for a late posting, audited | 3 | Owner |
| S-21 | Closing a shift is allowed with pending non-cash payments; they are listed separately on the cash-up | 3 | Owner / accountant |
| S-22 | Session defaults: idle expiry 2 h, absolute expiry 12 h (a shift), login lockout after 5 attempts for 15 min (env-configurable) | 4 | Owner |
| S-23 | Request body limit 1 MiB; API refuses to start in production with insecure cookie, non-https origin or no database URL | 4 | Owner (technical) |
| S-24 | Argon2id parameters: 64 MiB memory, 2 passes, 2 lanes, 16-byte salt, 32-byte key; re-check on the real Render instance size before go-live | 5 | Owner (technical) |
| S-25 | Password policy: 12 to 128 characters, must not contain the username, not one repeated character, never the documented dev password | 5 | Owner |
| S-26 | The tax rate is never defaulted: bootstrap requires `--tax-rate-bp` supplied by the operator after the accountant confirms it | 5 | Accountant |
| S-27 | Usernames are lowercase `a-z 0-9 . _ -`, 3 to 40 characters, never reused | 5 | Owner |
| S-28 | Document counters created at bootstrap: receipt (start number supplied), order, folio, purchase, reservation, refund, transfer, count. Kitchen ticket counters are created per business day when first needed (S-05) | 5 | Accountant |

