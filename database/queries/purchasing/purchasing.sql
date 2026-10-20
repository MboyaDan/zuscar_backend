-- Stage 19 and 20: suppliers and purchases. A purchase line keeps what was ordered and, once received, what arrived.

-- name: ListSuppliers :many
SELECT id, name, COALESCE(contact, '')::text AS contact, active FROM suppliers
WHERE (sqlc.arg('only_active')::boolean = false OR active) ORDER BY lower(name);

-- name: GetSupplier :one
SELECT id, name, COALESCE(contact, '')::text AS contact, active FROM suppliers WHERE id = $1;

-- name: InsertSupplier :one
INSERT INTO suppliers (name, contact) VALUES ($1, $2) RETURNING id;

-- name: UpdateSupplier :exec
UPDATE suppliers SET name = $2, contact = $3, active = $4, updated_at = now() WHERE id = $1;

-- A gapless counter: the row lock serialises callers and a rolled-back purchase gives its number back (A-05).
-- name: NextCounterValue :one
UPDATE document_counters SET next_value = next_value + 1, updated_at = now()
WHERE doc_type = $1 AND scope = '' RETURNING (next_value - 1)::bigint AS value;

-- name: InsertPurchase :one
INSERT INTO purchases (purchase_no, supplier_id, invoice_ref, note, created_by) VALUES ($1, $2, $3, $4, $5) RETURNING id;

-- name: InsertPurchaseItem :exec
INSERT INTO purchase_items (purchase_id, item_id, entered_qty, entered_unit_id, qty_base, unit_cost_cents) VALUES ($1, $2, $3, $4, $5, $6);

-- name: RecordPurchaseItemReceived :exec
UPDATE purchase_items SET received_qty = $3, received_qty_base = $4, received_unit_cost_cents = $5 WHERE purchase_id = $1 AND item_id = $2;

-- name: LockPurchase :one
SELECT id, status FROM purchases WHERE id = $1 FOR UPDATE;

-- name: MarkPurchaseReceived :exec
UPDATE purchases SET status = 'received', location_id = $2, received_by = $3, received_at = now(), invoice_ref = COALESCE($4, invoice_ref) WHERE id = $1;

-- name: MarkPurchaseCancelled :exec
UPDATE purchases SET status = 'cancelled', cancelled_by = $2, cancelled_at = now() WHERE id = $1;

-- name: ListPurchases :many
SELECT p.id, p.purchase_no, p.supplier_id, s.name AS supplier_name, p.status, COALESCE(p.invoice_ref, '')::text AS invoice_ref, COALESCE(p.note, '')::text AS note,
       p.location_id, COALESCE(l.name, '')::text AS location_name, p.created_at, p.received_at::timestamptz AS received_at,
       (SELECT count(*) FROM purchase_items i WHERE i.purchase_id = p.id)::bigint AS line_count,
       (SELECT COALESCE(sum(COALESCE(i.received_qty * i.received_unit_cost_cents, i.entered_qty * i.unit_cost_cents)), 0) FROM purchase_items i WHERE i.purchase_id = p.id)::bigint AS total_cents,
       (SELECT COALESCE(sum(i.entered_qty * i.unit_cost_cents), 0) FROM purchase_items i WHERE i.purchase_id = p.id)::bigint AS ordered_cents
FROM purchases p JOIN suppliers s ON s.id = p.supplier_id LEFT JOIN stock_locations l ON l.id = p.location_id
WHERE (sqlc.arg('status')::text = '' OR p.status = sqlc.arg('status')::text)
  AND (sqlc.arg('supplier_id')::bigint = 0 OR p.supplier_id = sqlc.arg('supplier_id')::bigint)
  AND (sqlc.arg('before_id')::bigint = 0 OR p.id < sqlc.arg('before_id')::bigint)
ORDER BY p.id DESC LIMIT sqlc.arg('row_limit')::int;

-- name: GetPurchase :one
SELECT p.id, p.purchase_no, p.supplier_id, s.name AS supplier_name, p.status, COALESCE(p.invoice_ref, '')::text AS invoice_ref, COALESCE(p.note, '')::text AS note,
       p.location_id, COALESCE(l.name, '')::text AS location_name, p.created_at, p.received_at::timestamptz AS received_at,
       (SELECT count(*) FROM purchase_items i WHERE i.purchase_id = p.id)::bigint AS line_count,
       (SELECT COALESCE(sum(COALESCE(i.received_qty * i.received_unit_cost_cents, i.entered_qty * i.unit_cost_cents)), 0) FROM purchase_items i WHERE i.purchase_id = p.id)::bigint AS total_cents,
       (SELECT COALESCE(sum(i.entered_qty * i.unit_cost_cents), 0) FROM purchase_items i WHERE i.purchase_id = p.id)::bigint AS ordered_cents
FROM purchases p JOIN suppliers s ON s.id = p.supplier_id LEFT JOIN stock_locations l ON l.id = p.location_id WHERE p.id = $1;

-- name: ListPurchaseItems :many
SELECT pi.item_id, i.name AS item_name, u.code AS base_unit_code, pi.entered_qty, eu.id AS entered_unit_id, eu.code AS entered_unit_code, pi.qty_base, pi.unit_cost_cents,
       pi.received_qty, pi.received_qty_base, pi.received_unit_cost_cents,
       COALESCE(pi.received_qty * pi.received_unit_cost_cents, pi.entered_qty * pi.unit_cost_cents)::bigint AS line_cents
FROM purchase_items pi JOIN inventory_items i ON i.id = pi.item_id JOIN units_of_measure u ON u.id = i.base_unit_id JOIN units_of_measure eu ON eu.id = pi.entered_unit_id
WHERE pi.purchase_id = $1 ORDER BY pi.id;
