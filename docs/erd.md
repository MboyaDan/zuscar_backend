# Entity relationship diagrams (Stage 2)

Design only. Brief section 8 lists the entities; deviations and additions are marked in `schema-constraints.md`. Diagrams show keys and the columns that matter for relationships. Money columns end in `_cents` (BIGINT KES cents).

## 1. Identity and access
```mermaid
erDiagram
  users ||--o{ user_roles : has
  roles ||--o{ user_roles : assigned
  roles ||--o{ role_permissions : grants
  permissions ||--o{ role_permissions : granted
  users ||--o{ sessions : owns
  users ||--o| user_pins : "optional PIN"
  users ||--o{ audit_logs : acts
  users { bigint id PK
    text username UK
    text status "active or inactive"
    boolean must_change_password }
  roles { bigint id PK
    text code UK
    boolean is_protected }
  permissions { bigint id PK
    text key UK }
  sessions { bigint id PK
    bytea token_hash UK
    timestamptz idle_expires_at
    timestamptz absolute_expires_at
    timestamptz revoked_at }
  audit_logs { bigint id PK
    bigint actor_user_id FK
    text action
    text entity_type
    jsonb before_json
    jsonb after_json }
```

## 2. Settings, counters and idempotency
```mermaid
erDiagram
  system_settings { text key PK
    jsonb value }
  tax_rates { bigint id PK
    text code UK
    int rate_bp }
  document_counters { text doc_type PK
    text scope PK
    bigint next_value }
  idempotency_records { bigint id PK
    text idem_key
    text endpoint
    bytea request_hash
    int response_status
    jsonb response_body }
```

## 3. Catalogue, orders, kitchen
```mermaid
erDiagram
  categories ||--o{ products : contains
  tax_rates ||--o{ products : taxed_by
  products ||--o{ price_history : priced
  products ||--o{ product_modifiers : offers
  modifiers ||--o{ product_modifiers : offered
  dining_tables ||--o{ orders : seats
  orders ||--|{ order_items : has
  products ||--o{ order_items : ordered
  order_items ||--o{ order_item_modifiers : with
  modifiers ||--o{ order_item_modifiers : applied
  orders ||--o{ kitchen_tickets : sends
  kitchen_tickets ||--o{ kitchen_tickets : "update of (parent)"
  kitchen_tickets ||--|{ kitchen_ticket_items : lists
  order_items ||--o{ kitchen_ticket_items : referenced
  kitchen_tickets ||--o{ ticket_status_history : tracks
  orders { bigint id PK
    text status "open sent finalised cancelled"
    text order_type
    bigint table_id FK }
  kitchen_tickets { bigint id PK
    date business_date
    int ticket_no
    text kind "original or update"
    bigint parent_ticket_id FK
    text status }
  products { bigint id PK
    text module "restaurant or bar"
    bigint price_cents
    boolean track_stock }
```

## 4. Sales, payments, shifts
```mermaid
erDiagram
  orders ||--o| sales : finalised_as
  sales ||--|{ sale_items : snapshots
  sales ||--o{ payments : "paid by"
  sales ||--o{ refunds : corrected_by
  refunds ||--o{ refund_items : lists
  sale_items ||--o{ refund_items : refunded
  sales ||--o| sale_voids : voided_by
  payments ||--o{ payment_attempts : "provider events (gated)"
  cashier_shifts ||--o{ sales : during
  cashier_shifts ||--o{ payments : during
  cashier_shifts ||--o{ cash_movements : records
  guest_folios ||--o{ payments : "deposits and settlements"
  sales ||--o| manual_receipt_reconciliations : "outage entry"
  sales { bigint id PK
    bigint receipt_no UK
    bigint total_cents
    text status "finalised partially_refunded refunded voided"
    text payment_status "unpaid partially_paid paid charged_to_folio"
    text source "pos or outage_reconciliation"
    text manual_receipt_no UK }
  payments { bigint id PK
    bigint sale_id FK
    bigint folio_id FK
    text method
    text status "pending_confirmation confirmed failed reversed"
    bigint amount_cents }
  cashier_shifts { bigint id PK
    bigint opening_float_cents
    bigint declared_cash_cents
    text status "open closed reconciled" }
```

## 5. Inventory and purchasing
```mermaid
erDiagram
  units_of_measure ||--o{ inventory_items : base_unit
  inventory_items ||--o{ unit_conversions : converts
  inventory_items ||--o{ inventory_movements : moves
  stock_locations ||--o{ inventory_movements : at
  inventory_movements ||--o| inventory_movements : "reverses (compensating)"
  inventory_items ||--o{ recipe_items : ingredient
  recipes ||--|{ recipe_items : lists
  products ||--o| recipes : "optional"
  products }o--o| inventory_items : "stock-tracked link"
  suppliers ||--o{ purchases : supplies
  purchases ||--|{ purchase_items : lines
  inventory_items ||--o{ purchase_items : bought
  stock_locations ||--o{ stock_counts : counted_at
  stock_counts ||--|{ stock_count_lines : lines
  stock_count_lines ||--o| inventory_movements : "adjustment"
  inventory_movements { bigint id PK
    bigint qty_delta "base units, never 0"
    text movement_type
    uuid transfer_group
    bigint reverses_movement_id FK
    text ref_type
    bigint ref_id }
```

## 6. Rooms, guests, folios
```mermaid
erDiagram
  room_types ||--o{ rooms : classifies
  room_types ||--o{ rate_plans : priced
  rooms ||--o{ reservations : booked
  guests ||--o{ reservations : makes
  reservations ||--o| stays : "becomes"
  stays ||--|| guest_folios : "has one"
  guest_folios ||--o{ folio_items : postings
  sales ||--o| folio_items : "POS room charge"
  rooms ||--o{ housekeeping_tasks : needs
  rooms ||--o{ room_status_history : changes
  reservations { bigint id PK
    bigint room_id FK
    date check_in
    date check_out
    text status "tentative confirmed checked_in checked_out cancelled no_show" }
  rooms { bigint id PK
    text number UK
    text physical_status "vacant_ready reserved occupied dirty cleaning out_of_service" }
  guest_folios { bigint id PK
    text status "open settled closed"
    bigint write_off_cents }
  folio_items { bigint id PK
    text kind
    bigint amount_cents
    date night_date
    bigint sale_id FK
    bigint reverses_item_id FK }
```

## 7. Printing and integrations
```mermaid
erDiagram
  sales ||--o{ receipt_records : documented_by
  guest_folios ||--o{ receipt_records : documented_by
  kitchen_tickets ||--o{ print_jobs : printed_by
  receipt_records ||--o{ print_jobs : printed_by
  print_jobs ||--o{ print_attempts : tries
  print_jobs ||--o| print_jobs : "reprint of"
  sales ||--o{ tax_invoices : "stub (DG-01)"
  integration_events { bigint id PK
    text provider
    text external_id
    jsonb payload
    text status }
  print_jobs { bigint id PK
    text document_type
    text status "queued printed failed reprinted"
    bigint reprint_of_job_id FK }
```
