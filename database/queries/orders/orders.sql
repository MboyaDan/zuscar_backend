-- Stage 10a: dining tables and orders. Money is BIGINT cents.

-- name: ListTables :many
SELECT t.id, t.label, t.seats, t.active, t.created_at,
       COALESCE((SELECT o.id FROM orders o WHERE o.table_id = t.id AND o.status IN ('open', 'sent') ORDER BY o.id LIMIT 1), 0)::bigint AS open_order_id -- 0 means none
FROM dining_tables t ORDER BY t.label, t.id;

-- name: GetTable :one
SELECT id, label, seats, active, created_at FROM dining_tables WHERE id = $1;

-- name: CountTables :one
SELECT count(*) FROM dining_tables;

-- name: CreateTable :one
INSERT INTO dining_tables (label, seats) VALUES ($1, $2) RETURNING id;

-- name: UpdateTable :exec
UPDATE dining_tables SET label = $2, seats = $3, active = $4 WHERE id = $1;

-- A gapless counter: the row lock serialises callers and a rolled-back order gives its number back (A-05).
-- name: NextCounterValue :one
UPDATE document_counters SET next_value = next_value + 1, updated_at = now()
WHERE doc_type = $1 AND scope = '' RETURNING (next_value - 1)::bigint AS value;

-- name: CreateOrder :one
INSERT INTO orders (order_no, order_type, table_id, opened_by, waiter_id, note) VALUES ($1, $2, $3, $4, $4, $5) RETURNING id;

-- name: LockOrder :one
SELECT id, status, version, opened_by FROM orders WHERE id = $1 FOR UPDATE;

-- name: BumpOrder :exec
UPDATE orders SET version = version + 1, updated_at = now() WHERE id = $1;

-- name: CancelOrder :exec
UPDATE orders SET status = 'cancelled', cancel_reason = $2, cancelled_by = $3, cancelled_at = now(), version = version + 1, updated_at = now() WHERE id = $1;

-- name: GetOrderHead :one
SELECT o.id, o.order_no, o.order_type, o.table_id, COALESCE(t.label, '')::text AS table_label, o.status, o.opened_by,
       u.display_name AS opened_by_name, o.note, o.version, o.cancel_reason, o.created_at, o.updated_at,
       COALESCE((SELECT sum((i.unit_price_cents + COALESCE((SELECT sum(m.price_delta_cents) FROM order_item_modifiers m WHERE m.order_item_id = i.id), 0)) * i.qty)
                 FROM order_items i WHERE i.order_id = o.id AND i.status = 'active'), 0)::bigint AS subtotal_cents,
       (SELECT count(*) FROM order_items i WHERE i.order_id = o.id AND i.status = 'active')::int AS item_count
FROM orders o
LEFT JOIN dining_tables t ON t.id = o.table_id
JOIN users u ON u.id = o.opened_by
WHERE o.id = $1;

-- name: ListOrders :many
SELECT o.id, o.order_no, o.order_type, o.table_id, COALESCE(t.label, '')::text AS table_label, o.status, o.opened_by,
       u.display_name AS opened_by_name, o.note, o.version, o.cancel_reason, o.created_at, o.updated_at,
       COALESCE((SELECT sum((i.unit_price_cents + COALESCE((SELECT sum(m.price_delta_cents) FROM order_item_modifiers m WHERE m.order_item_id = i.id), 0)) * i.qty)
                 FROM order_items i WHERE i.order_id = o.id AND i.status = 'active'), 0)::bigint AS subtotal_cents,
       (SELECT count(*) FROM order_items i WHERE i.order_id = o.id AND i.status = 'active')::int AS item_count
FROM orders o
LEFT JOIN dining_tables t ON t.id = o.table_id
JOIN users u ON u.id = o.opened_by
WHERE (sqlc.narg('status')::text IS NULL OR o.status = sqlc.narg('status')::text)
  AND (sqlc.narg('table_id')::bigint IS NULL OR o.table_id = sqlc.narg('table_id')::bigint)
  AND (sqlc.narg('opened_by')::bigint IS NULL OR o.opened_by = sqlc.narg('opened_by')::bigint)
  AND (sqlc.narg('before_id')::bigint IS NULL OR o.id < sqlc.narg('before_id')::bigint)
ORDER BY o.id DESC
LIMIT sqlc.arg('row_limit')::int;

-- name: GetProductForOrder :one
SELECT p.id, p.name, p.price_cents, p.kitchen_routed, (p.is_available AND c.active) AS sellable
FROM products p JOIN categories c ON c.id = p.category_id WHERE p.id = $1;

-- Modifiers that are linked to the product, still active, and among the ones asked for.
-- name: ListOfferedModifiers :many
SELECT m.id, m.name, m.price_delta_cents FROM modifiers m
JOIN product_modifiers pm ON pm.modifier_id = m.id
WHERE pm.product_id = $1 AND m.active AND m.id = ANY($2::bigint[]) ORDER BY m.id;

-- name: InsertOrderItem :one
INSERT INTO order_items (order_id, product_id, qty, unit_price_cents, note) VALUES ($1, $2, $3, $4, $5) RETURNING id;

-- name: InsertOrderItemModifier :exec
INSERT INTO order_item_modifiers (order_item_id, modifier_id, price_delta_cents) VALUES ($1, $2, $3);

-- name: ListOrderItems :many
SELECT i.id, i.order_id, i.product_id, p.name AS product_name, p.kitchen_routed, i.qty, i.unit_price_cents, i.status, i.note,
       COALESCE(i.cancel_reason, '')::text AS cancel_reason, i.created_at,
       EXISTS (SELECT 1 FROM kitchen_ticket_items k WHERE k.order_item_id = i.id AND k.change_type = 'add') AS sent_to_kitchen
FROM order_items i JOIN products p ON p.id = i.product_id
WHERE i.order_id = $1 ORDER BY i.id;

-- name: ListOrderItemModifiers :many
SELECT im.order_item_id, im.modifier_id, m.name, im.price_delta_cents
FROM order_item_modifiers im JOIN modifiers m ON m.id = im.modifier_id
WHERE im.order_item_id = ANY($1::bigint[]) ORDER BY im.order_item_id, im.modifier_id;

-- name: GetOrderItem :one
SELECT id, order_id, qty, note, status FROM order_items WHERE id = $1 AND order_id = $2;

-- name: UpdateOrderItem :exec
UPDATE order_items SET qty = $2, note = $3 WHERE id = $1;

-- name: CancelOrderItem :exec
UPDATE order_items SET status = 'cancelled', cancel_reason = $2, cancelled_by = $3, cancelled_at = now() WHERE id = $1;

-- Stage 11a: what the sales module needs from an order. Sales never reads these tables itself.
-- name: ListSaleLines :many
SELECT i.id, i.product_id, p.name AS product_name, c.module, i.qty, i.unit_price_cents,
       COALESCE((SELECT sum(m.price_delta_cents) FROM order_item_modifiers m WHERE m.order_item_id = i.id), 0)::bigint AS modifiers_cents,
       COALESCE((SELECT string_agg(mo.name, ', ' ORDER BY mo.name) FROM order_item_modifiers im JOIN modifiers mo ON mo.id = im.modifier_id WHERE im.order_item_id = i.id), '')::text AS modifiers_text,
       t.code AS tax_code, t.treatment AS tax_treatment, t.rate_bp AS tax_rate_bp
FROM order_items i
JOIN products p ON p.id = i.product_id
JOIN categories c ON c.id = p.category_id
JOIN tax_rates t ON t.id = p.tax_rate_id
WHERE i.order_id = $1 AND i.status = 'active' ORDER BY i.id;

-- name: MarkOrderFinalised :exec
UPDATE orders SET status = 'finalised', version = version + 1, updated_at = now() WHERE id = $1 AND status IN ('open', 'sent');
