# Table-by-table constraint notes (Stage 2)

Design only. Items marked SPIKE were exercised on PostgreSQL 16.15 in `docs/spikes/stage2-constraints-spike.sql` (scratch database, 2026-10-13); everything else is unexecuted design. Proposed conventions are logged as S-xx in ASSUMPTIONS.md.

## 0. Conventions
- Primary keys `BIGINT GENERATED ALWAYS AS IDENTITY` (S-01). Human numbers (receipt, ticket, reservation) are separate columns from counters.
- Enumerations are `TEXT` with `CHECK (... IN (...))`, easier to migrate than PG enums (S-02).
- Money: `BIGINT` `*_cents`, CHECK `>= 0` unless a signed ledger column. Tax rates: integer basis points (`rate_bp`). Tax-inclusive line: `tax = total - round_half_up(total * 10000 / (10000 + rate_bp))`, stored per line with the rate (A-04, 7.10). Rounding lives in one Go `money` package and one test table.
- Stock quantities: `BIGINT` in each item's base unit (ml, g, piece), never floats. Conversions are integer ratios `factor_num/factor_den` (S-03).
- Timestamps `timestamptz` UTC; business dates are `date` columns computed in Africa/Nairobi at write time (`business_date`).
- Every table: `created_at`, and `created_by` where a user acts. Soft state via `status`; no cascading deletes on history (`ON DELETE RESTRICT` everywhere).
- All tables `ENABLE ROW LEVEL SECURITY` with no policies for Supabase public roles (brief 12.2; guarded `DO` block so local PostgreSQL without those roles still migrates; UNVERIFIED on Supabase).
- DB roles (Stage 5): `zuscar_migrator` (owner, DDL) and `zuscar_app` (DML only). `zuscar_app` has no TRUNCATE and no UPDATE/DELETE on immutable tables (defence in depth beside triggers).
- Immutability helper `reject_mutation()` trigger (UPDATE/DELETE/TRUNCATE) on: audit_logs, inventory_movements, sale_items, folio_items, refunds, refund_items, cash_movements, print_attempts, price_history, room_status_history, ticket_status_history, integration_events (payload columns). SPIKE (ledger).

## 1. Identity and access
| Table | Key columns | Constraints and notes |
|---|---|---|
| users | username, display_name, password_hash (Argon2id), status, must_change_password, failed_login_count, locked_until, deactivated_at | `username` unique case-insensitive (citext or lower index), never reused; `status IN (active, inactive)`; no hard delete once referenced (FKs RESTRICT). Last-active-Super-Admin trigger (below). |
| roles | code, name, is_protected, is_template | `code` unique. `super_admin` is `is_protected = true`; protected roles cannot be deleted or edited through role APIs. Delete blocked while `user_roles` rows exist (FK RESTRICT). |
| permissions | key | Unique; seeded from RBAC list; no admin screen creates permissions. |
| user_roles | (user_id, role_id) PK, assigned_by, assigned_at | A user may hold several roles (owner = Super Admin + Business Manager). |
| role_permissions | (role_id, permission_id) PK | Trigger `guard_system_admin`: `system.admin` only on a protected role. SPIKE. |
| sessions | user_id, token_hash BYTEA unique, created_at, last_seen_at, idle_expires_at, absolute_expires_at, revoked_at, revoke_reason, ip, user_agent | Only SHA-256 of token stored. Index (user_id) WHERE revoked_at IS NULL. Deactivation revokes all sessions in the same transaction. |
| user_pins | user_id PK, pin_hash, failed_count, locked_until | Optional (A-07). Only for users whose roles are floor roles; managers and admins cannot have a PIN (service rule plus CHECK via trigger at Stage 6). |
| login_attempts | username_norm, ip, attempted_at, success | Backs rate limiting and lockout across restarts. Pruned by age. |
| audit_logs | occurred_at, actor_user_id (nullable for CLI/system), actor_label, action, entity_type, entity_id, reason, result, before_json, after_json, request_id, ip | Immutable by trigger. Written in the same transaction as the action. No secrets in JSON (field allow-list per action). Retention 7 years (DQ-18): no delete path. Index (entity_type, entity_id), (actor_user_id, occurred_at), (action, occurred_at). |

Super Admin guards (AC-11, AC-12; full rules in `rbac.md`):
- `guard_last_super_admin()` constraint trigger on users (UPDATE OF status, DELETE) and user_roles (DELETE). It takes `pg_advisory_xact_lock` then requires one active user with `super_admin`. SPIKE: single and concurrent attempts (second session blocked ~2 s, then failed).
- The same logic is also enforced in the service for friendly errors; the trigger is the backstop.

## 2. Settings, counters, idempotency
| Table | Notes |
|---|---|
| system_settings | `key` PK, `value` JSONB, updated_by/at. Keys: business_name, address, phone, receipt_footer, timezone (Africa/Nairobi), currency (KES), tax_inclusive, negative_stock_policy, checkin_time, checkout_time, idempotency_ttl_hours. Changes audited. Edit requires `system.admin` (S-04). |
| tax_rates (addition) | code unique, name, rate_bp CHECK 0..10000, active. Needed because VAT treatment may differ for food, drinks and accommodation (DQ-12). products and room_types reference one. Rate changes never rewrite old sale lines (they snapshot). |
| document_counters | PK (doc_type, scope); next_value BIGINT; prefix. `scope` is '' for global sequences (receipt, folio, purchase, reservation, transfer, count, refund) and the business date for kitchen tickets (S-05). Increment with `SELECT ... FOR UPDATE` inside the business transaction so rollbacks leave no gap (7.7, A-05). Opening value set at go-live to continue the venue's receipt book. |
| idempotency_records | idem_key, endpoint, user_id, request_hash BYTEA, status (processing, completed), response_status, response_body JSONB, created_at, expires_at | UNIQUE (idem_key, endpoint). Insert `processing` row first; a concurrent duplicate sees the row and returns 409 or waits; same hash and completed returns stored response; different hash returns 422 (ADR-08). Expired rows removed by a documented job, never within 24 h (S-06). |

## 3. Catalogue, orders, kitchen
| Table | Constraints and notes |
|---|---|
| categories | module IN (restaurant, bar); name unique per module; sort_order; active. |
| products | category_id FK; name; sku nullable unique; module; `price_cents >= 0`; tax_rate_id FK; is_available; track_stock; inventory_item_id FK nullable; kitchen_routed bool. CHECK `track_stock = (inventory_item_id IS NOT NULL)`. Price edits write price_history and are audited. |
| price_history | product_id, price_cents, effective_from, changed_by. Immutable (trigger rejects UPDATE, DELETE, TRUNCATE). Stage 9a: `products` has a composite FK (category_id, module) to `categories (id, module)`, so a product cannot sit in another module's category; `track_stock` and `inventory_item_id` are added in Stage 17. |
| modifiers / product_modifiers | modifier name, `price_delta_cents` (signed), active; link table PK (product_id, modifier_id). |
| dining_tables | label unique, seats, active. Brief calls them tables; renamed to avoid the SQL keyword. |
| orders | order_no (counter), order_type IN (dine_in, takeaway), table_id FK nullable (required for dine_in), status IN (open, sent, finalised, cancelled), opened_by, waiter_id, note, version int (optimistic lock). Edits only while open or sent: enforced in service and by trigger rejecting changes to finalised rows (S-07). Index (status), (table_id) WHERE status IN (open, sent). |
| order_items | order_id, product_id, qty > 0, unit_price_cents (server-set from products at add time, never client-supplied), status IN (active, cancelled), cancelled_by, cancel_reason, note. Cancelling after send creates an update ticket line (AC-04). |
| order_item_modifiers | order_item_id, modifier_id, price_delta_cents snapshot. |
| kitchen_tickets | order_id, business_date, ticket_no, kind IN (original, update), parent_ticket_id (NOT NULL iff kind = update), status IN (submitted, preparing, ready, served, cancelled), created_by. UNIQUE (business_date, ticket_no). Status has no link to payment (AC-04). Persisted before any print (NN-05). |
| kitchen_ticket_items | ticket_id, order_item_id, name_snapshot, qty, change_type IN (add, cancel, modify), modifiers_text, note. No prices (ticket shows none). |
| ticket_status_history | ticket_id, from_status, to_status, changed_by, at. Immutable. |

## 4. Sales, payments, shifts
| Table | Constraints and notes |
|---|---|
| sales | receipt_no BIGINT UNIQUE NOT NULL (from counter); order_id UNIQUE nullable (null for outage entries); business_date; subtotal, discount, tax, total (all `_cents`, CHECK total = subtotal - discount); status IN (finalised, partially_refunded, refunded, voided); payment_status IN (unpaid, partially_paid, paid, charged_to_folio); source IN (pos, outage_reconciliation); manual_receipt_no TEXT UNIQUE nullable (duplicate guard, AC-17); folio_id nullable; cashier_id, waiter_id, shift_id; idempotency link. CHECK `source = 'outage_reconciliation' IFF manual_receipt_no IS NOT NULL`. CHECK `payment_status = 'charged_to_folio' IFF folio_id IS NOT NULL` (NN-10). Trigger limits UPDATE to status and payment_status columns. `payment_status` is explicit and cross-checked by a reconciliation test against payments (S-08). |
| sale_items | sale_id, product_id FK, name_snapshot, module, qty > 0, unit_price_cents, discount_cents, discount_reason, discount_approved_by, tax_rate_bp, tax_cents, line_total_cents. CHECK discount_cents between 0 and qty*unit price. Fully immutable (trigger). AC-01 snapshots. |
| payments | sale_id nullable, folio_id nullable, CHECK exactly one non-null (S-09: folio_payments is represented here, one table of money received); purpose IN (sale_payment, deposit, folio_settlement); method IN (cash, mpesa_manual, card_manual, bank_manual); status IN (pending_confirmation, confirmed, failed, reversed); `amount_cents > 0`; reference nullable; shift_id; confirmed_by, confirmed_at. CHECK method = cash IMPLIES status confirmed on insert (service sets). Partial UNIQUE (method, reference) WHERE reference IS NOT NULL AND status IN (pending_confirmation, confirmed): a typed M-Pesa code cannot be recorded twice (AC-02, R-05). Trigger: amount, method, sale/folio never change after insert; only status transitions with timestamps (S-10). Room charge is never a payment method (NN-10). |
| payment_attempts | payment_id nullable, provider, provider_event_id, status, raw_ref. UNIQUE (provider, provider_event_id). Gated (DG-02): table exists, nothing writes to it until verified. |
| refunds / refund_items | refund_no (counter), sale_id, reason NOT NULL, approved_by, method, shift_id, amount_cents > 0, created_by; refund_items reference sale_item_id with qty and amount, CHECK cumulative refunded qty per sale_item <= sold qty (enforced in service under row lock; verified by test). Immutable. Matching payment (reversal/outgoing) and stock movements created in the same transaction. |
| sale_voids | sale_id UNIQUE, reason NOT NULL, voided_by, approved_by, voided_at. Void creates compensating payment and stock records; sale row keeps original data. |
| cashier_shifts | user_id, opened_at, opening_float_cents >= 0, closed_at, declared_cash_cents, expected_cash_cents (snapshot at close), variance_cents (= declared - expected), status IN (open, closed, reconciled), closed_by, reconciled_by, notes. Partial UNIQUE (user_id) WHERE status = 'open' (one open shift per user). CHECK closed fields present iff status <> open. |
| cash_movements | shift_id, kind IN (paid_in, paid_out, expense), amount_cents > 0, reason NOT NULL, created_by. Immutable. Expected cash = float + confirmed cash payments - cash refunds + paid_in - paid_out - expense, all from rows (AC-09). |
| manual_receipt_reconciliations | manual_receipt_no UNIQUE (1 to 32 characters), original_occurred_at, paper_total_cents, sale_id FK UNIQUE, entered_by, discrepancy_note (required when paper_total_cents differs from the sale total), source_note. Duplicate guard is the UNIQUE plus sales.manual_receipt_no. Insert trigger checks the sale is an outage sale with the same paper number and the time is not in the future; a deferred constraint trigger on `sales` refuses an outage sale committed without its entry; rows are immutable (migration 0017). |

## 5. Inventory and purchasing
| Table | Constraints and notes |
|---|---|
| units_of_measure | code unique, name. |
| inventory_items | name, sku unique nullable, base_unit_id, low_stock_threshold (base units, nullable), active. No hard-coded bottle/crate/shot (7.2). |
| unit_conversions | item_id, from_unit_id, to_unit_id, factor_num > 0, factor_den > 0; UNIQUE (item_id, from_unit_id, to_unit_id). Result of a conversion must be integral in base units or the entry is rejected (S-03). |
| stock_locations | code unique (main_store, bar, kitchen at minimum), name, active. |
| inventory_movements | item_id, location_id, `qty_delta <> 0` (signed, base units), movement_type IN (opening_balance, receipt, transfer_out, transfer_in, sale_consumption, sale_return, recipe_consumption, wastage, breakage, complimentary, count_adjustment, adjustment, reversal), reason (NOT NULL for wastage, breakage, complimentary, adjustment, count_adjustment, reversal and negative-override), entered_qty and entered_unit_id (what the user typed, for traceability), unit_cost_cents nullable, ref_type, ref_id, transfer_group UUID, reverses_movement_id UNIQUE nullable, override_reason, created_by. Immutable (UPDATE, DELETE, TRUNCATE rejected). SPIKE. Deferred constraint trigger: each transfer_group nets to zero. SPIKE. Partial UNIQUE (ref_type, ref_id, item_id, movement_type) WHERE movement_type IN (sale_consumption, recipe_consumption): retries cannot deduct twice. Balances: `SELECT SUM(qty_delta) ... GROUP BY item, location` (view); a maintained summary only if needed (decide Stage 17, ADR-07). Negative stock: service warns and requires `inventory.override_negative` plus override_reason (A-06). |
| recipes / recipe_items | recipe per product (UNIQUE product_id), active (off unless configured, DQ-08); items: inventory_item_id, qty_base > 0. |
| suppliers | name unique, contact, active. |
| purchases / purchase_items | purchase_no (counter), supplier_id, status IN (draft, ordered, received, cancelled), invoice_ref, received_at/by; items: item_id, entered_qty, entered_unit_id, qty_base > 0, unit_cost_cents >= 0. Receiving writes `receipt` movements in the same transaction; a received purchase is immutable (corrections by reversal). |
| stock_counts / stock_count_lines | count_no, location_id, status IN (draft, submitted, approved), approved_by; lines: item_id, expected_base (snapshot), counted_base, variance_base; approval creates `count_adjustment` movements linked via adjustment_movement_id. UNIQUE (count_id, item_id). |

## 6. Rooms, guests, folios
| Table | Constraints and notes |
|---|---|
| room_types | name unique, base_rate_cents >= 0, capacity, tax_rate_id. |
| rooms | number unique, room_type_id, floor, physical_status IN (vacant_ready, reserved, occupied, dirty, cleaning, out_of_service), active. Count is data, not code (AC-07). |
| rate_plans (Could) | room_type_id, name, rate_cents, valid_from, valid_to, CHECK valid_to >= valid_from; base rate only in first build. |
| guests | full_name, phone, email nullable, notes; ID document number is not collected by default (privacy; confirm need, S-11). Search by name/phone indexed. |
| reservations | reservation_no (counter), guest_id, room_id, check_in, check_out DATE, CHECK check_out > check_in, status IN (tentative, confirmed, checked_in, checked_out, cancelled, no_show), adults, nightly_rate_cents snapshot, notes, cancelled_reason. `EXCLUDE USING gist (room_id WITH =, daterange(check_in, check_out, '[)') WITH &&) WHERE (status IN ('confirmed','checked_in'))` needs btree_gist (ADR-06). SPIKE: overlap rejected, back-to-back allowed, tentative does not block, tentative-to-confirmed conflict rejected, cancelled frees the room. Violation (SQLSTATE 23P01) maps to HTTP 409. Concurrency test written at Stage 20 (two transactions racing) is still the proof; not yet run. |
| stays | reservation_id UNIQUE, room_id, checked_in_at, expected_out, checked_out_at, status IN (in_house, checked_out). A walk-in creates a reservation already `checked_in`, so overlap protection lives in one table (S-12). Room moves update reservation.room_id under the same constraint, audited. |
| guest_folios | folio_no (counter), stay_id UNIQUE nullable (non-stay folios possible later), guest_id, status IN (open, settled, closed), write_off_cents >= 0, write_off_approved_by, closed_at. CHECK write_off_cents = 0 OR write_off_approved_by IS NOT NULL. Closing requires balance 0 or approved write-off: checked in the closing transaction (service) with a SPIKE-free test at Stage 21. Balance = SUM(folio_items) - SUM(confirmed payments with folio_id) (AC-08). |
| folio_items | folio_id, kind IN (room_night, pos_charge, adjustment), description, amount_cents (signed; nonzero), night_date, sale_id, reason, reverses_item_id, posted_by. UNIQUE (folio_id, night_date) WHERE kind = 'room_night'; UNIQUE (sale_id) WHERE kind = 'pos_charge' (a sale posts once, retries are safe); adjustments require reason. Immutable; corrections are new adjustment rows (NN-08). A folio posting is not a payment (NN-10). |
| housekeeping_tasks | room_id, task_type, status IN (open, in_progress, done, cancelled), assigned_to, created_by, completed_at. |
| room_status_history | room_id, from_status, to_status, changed_by, reason, at. Immutable; transitions validated in Stage 3 state diagram. |

## 7. Printing and integrations
| Table | Constraints and notes |
|---|---|
| receipt_records | document_type IN (receipt, unpaid_bill, folio_bill), doc_no, sale_id / folio_id (exactly one), snapshot JSONB (the rendered content, so a reprint is identical), created_at. UNIQUE (document_type, sale_id) for receipts. |
| print_jobs | document_type IN (receipt, unpaid_bill, folio_bill, kitchen_ticket), receipt_record_id or kitchen_ticket_id (exactly one), status IN (queued, printed, failed, reprinted), attempt_count, last_error (sanitised, no secrets), reprint_of_job_id (NOT NULL iff reprint), requested_by. Retry updates only the job; never touches sale or ticket (AC-03). |
| print_attempts | job_id, attempted_at, result, error. Immutable. "Printed" means the user confirmed the browser print dialog finished: the browser cannot prove paper came out (DG-03 note). |
| tax_invoices (stub) | sale_id, kind IN (invoice, credit_note), status IN (pending, accepted, rejected, retryable_failure), provider nullable, provider_ref, request_id, response_code, error_detail, attempts, next_retry_at. UNIQUE (sale_id, kind). Nothing writes here until DG-01 is verified (NN-12). |
| integration_events | provider, event_type, external_id, payload JSONB, received_at, processed_at, status. UNIQUE (provider, external_id). Persist first, then process (10.5). Gated (DG-02). |

## 8. Cross-cutting indexes (initial)
sales(business_date), sales(cashier_id, business_date), payments(shift_id), payments(sale_id), payments(folio_id), inventory_movements(item_id, location_id), inventory_movements(ref_type, ref_id), reservations(room_id, check_in), reservations(guest_id), folio_items(folio_id), audit_logs as in section 1. Add others only with a measured need.

## 9. Open items from this document
S-01..S-12 in ASSUMPTIONS.md. Nothing here has been run beyond the spike; the invariants marked SPIKE still need real migration-based tests at Stages 5, 17, 20 and 21.

## 10. Stage 5 implementation notes (what the first migrations actually contain)
Migrations `0001`..`0004` in `database/migrations`. Differences from the plan above, all deliberate:
- `permissions.is_protected` and `roles.is_protected` / `roles.is_template` columns; the system.admin guard is generic (a protected permission may only be held by a protected role) rather than hard-coding the key. Protected roles cannot be deleted, renamed or unprotected (trigger).
- `users.username` is a lowercase-only CHECK (`^[a-z0-9][a-z0-9._-]{2,39}$`) with a plain UNIQUE, instead of citext. `users_deactivated_consistent` requires `deactivated_at` exactly when status is inactive.
- Triggers implemented: audit_logs append-only (update, delete, truncate), protected-permission guard, protected-role guard, last-active-Super-Admin guard (advisory-lock serialised; tested including a two-connection race).
- `system_settings`, `tax_rates`, `document_counters` exist now because bootstrap seeds them. Counter rows are never reset by re-running base data.
- DEFERRED to Stage 25 with the deployment role design: Row Level Security and the split migrator/app database roles. Reason: with RLS enabled and no policies, a non-owner application role sees no rows, so the role model and policies must be designed together and tested on Supabase. Until then the API connects as the database owner and relies on the API's authorisation and the triggers.

## 11. Stage 6 implementation notes
- `0005_tax_treatment.sql`: `tax_rates.treatment` (standard, zero_rated, exempt); zero-rated and exempt require `rate_bp = 0`. Products and room types will reference a category with NOT NULL and no default. From Stage 11, `sale_items` also snapshots `tax_treatment` next to `tax_rate_bp`; reports break tax down by treatment. See `tax-configuration.md`.
- `0006_sessions_auth.sql`: `sessions.csrf_hash` (SHA-256, bound to the session), `sessions.auth_method` (password or pin) and `sessions.reauth_at`. Pre-existing sessions are discarded (none could exist before login).
- `0007_super_admin_guard_precision.sql`: the last-Super-Admin trigger now acts only for users who hold `super_admin` (and for removal of that role). The original version blocked deactivating anyone when no Super Admin existed; the Stage 6 tests found it.
- `0018_inventory.sql` to `0020_sale_stock_snapshot.sql` (Stage 17): `units_of_measure` (pc, ml, g seeded), `stock_locations` (main_store, bar, kitchen seeded), `inventory_items` (unique name and SKU ignoring case; a trigger keeps the base unit fixed), `unit_conversions` (must end in the item's base unit), `inventory_movements` (append-only; sign rules per type; a reason for wastage, breakage, complimentary, adjustment, count_adjustment and reversal; the extra type `sale_return` puts stock back for a void or restocked refund; `reverses_movement_id` UNIQUE with a trigger that makes a reversal the exact opposite of a reversible movement; partial UNIQUE (ref_type, ref_id, item_id, location_id, movement_type) for sale and recipe movements so a retry cannot deduct twice; partial UNIQUE for one opening balance per item and place) and the view `stock_balances`. `products` gains `track_stock`, `inventory_item_id` and `stock_units_per_sale` (track_stock is true exactly when an item is linked); `sale_items` gains `inventory_item_id`, `stock_units_per_sale` and `stock_location_id` (all or none), the snapshot a void or refund uses.
- `0021_transfers.sql` (Stage 18): `inventory_movements` CHECK that `transfer_group` is set exactly for `transfer_out` and `transfer_in`; a deferred constraint trigger that, at commit, requires each group to net to zero across exactly two places and one item; an index on `transfer_group`.
- `0022_purchasing.sql` (Stage 19): `suppliers` (unique lower-case name, never deleted); `purchases` (gapless `purchase_no`, status draft/received/cancelled, shape checks, a trigger that lets only a draft change and never deletes); `purchase_items` (once per item, immutable); a unique index so a purchase receives each item into a place once; a CHECK that every `receipt` movement names its purchase and carries a cost.
- `0023_receiving_actuals.sql` (Stage 20): `purchase_items` gains `received_qty`, `received_qty_base` and `received_unit_cost_cents` (all set together or all empty); a trigger that lets them be written once, only while the purchase is a draft, and never lets the ordered columns change or a line be deleted; the purchase guard refuses `received` unless every line says what arrived. Purchases received in Stage 19 are backfilled as arrived exactly as ordered.
