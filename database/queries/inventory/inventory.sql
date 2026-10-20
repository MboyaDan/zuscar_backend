-- Stage 17: inventory. Quantities are BIGINT in each item's base unit. Balances come from the ledger (stock_balances view).

-- name: ListUnits :many
SELECT id, code, name FROM units_of_measure ORDER BY code;

-- name: GetUnit :one
SELECT id, code, name FROM units_of_measure WHERE id = $1;

-- name: InsertUnit :one
INSERT INTO units_of_measure (code, name) VALUES ($1, $2) RETURNING id, code, name;

-- name: ListLocations :many
SELECT id, code, name, active FROM stock_locations ORDER BY id;

-- name: GetLocation :one
SELECT id, code, name, active FROM stock_locations WHERE id = $1;

-- name: GetLocationByCode :one
SELECT id, code, name, active FROM stock_locations WHERE code = $1;

-- name: InsertLocation :one
INSERT INTO stock_locations (code, name) VALUES ($1, $2) RETURNING id, code, name, active;

-- name: ListItems :many
SELECT i.id, i.name, COALESCE(i.sku, '')::text AS sku, i.base_unit_id, u.code AS base_unit_code, u.name AS base_unit_name, i.low_stock_threshold, i.active,
       COALESCE((SELECT sum(b.qty_base) FROM stock_balances b WHERE b.item_id = i.id), 0)::bigint AS total_base
FROM inventory_items i JOIN units_of_measure u ON u.id = i.base_unit_id
WHERE (sqlc.arg('only_active')::boolean = false OR i.active)
  AND (sqlc.arg('q')::text = '' OR i.name ILIKE '%' || sqlc.arg('q')::text || '%' OR COALESCE(i.sku, '') ILIKE '%' || sqlc.arg('q')::text || '%')
ORDER BY lower(i.name);

-- name: GetItem :one
SELECT i.id, i.name, COALESCE(i.sku, '')::text AS sku, i.base_unit_id, u.code AS base_unit_code, u.name AS base_unit_name, i.low_stock_threshold, i.active,
       COALESCE((SELECT sum(b.qty_base) FROM stock_balances b WHERE b.item_id = i.id), 0)::bigint AS total_base
FROM inventory_items i JOIN units_of_measure u ON u.id = i.base_unit_id WHERE i.id = $1;

-- Takes the item's row so two stock changes to the same item take turns. Always lock items in id order (no deadlocks).
-- name: LockItems :many
SELECT id, name, active FROM inventory_items WHERE id = ANY($1::bigint[]) ORDER BY id FOR UPDATE;

-- name: InsertItem :one
INSERT INTO inventory_items (name, sku, base_unit_id, low_stock_threshold) VALUES ($1, $2, $3, $4) RETURNING id;

-- name: UpdateItem :exec
UPDATE inventory_items SET name = $2, sku = $3, low_stock_threshold = $4, active = $5, updated_at = now() WHERE id = $1;

-- name: ListConversions :many
SELECT c.item_id, c.from_unit_id, u.code AS from_unit_code, u.name AS from_unit_name, c.factor_num, c.factor_den
FROM unit_conversions c JOIN units_of_measure u ON u.id = c.from_unit_id
WHERE c.item_id = ANY($1::bigint[]) ORDER BY c.item_id, u.code;

-- name: GetConversion :one
SELECT factor_num, factor_den FROM unit_conversions WHERE item_id = $1 AND from_unit_id = $2;

-- name: DeleteConversions :exec
DELETE FROM unit_conversions WHERE item_id = $1;

-- name: InsertConversion :exec
INSERT INTO unit_conversions (item_id, from_unit_id, to_unit_id, factor_num, factor_den) VALUES ($1, $2, $3, $4, $5);

-- name: ListBalances :many
SELECT b.item_id, i.name AS item_name, b.location_id, l.code AS location_code, l.name AS location_name, b.qty_base, b.movements, b.last_movement_at::timestamptz AS last_movement_at,
       u.code AS base_unit_code
FROM stock_balances b
JOIN inventory_items i ON i.id = b.item_id
JOIN stock_locations l ON l.id = b.location_id
JOIN units_of_measure u ON u.id = i.base_unit_id
WHERE (sqlc.arg('item_id')::bigint = 0 OR b.item_id = sqlc.arg('item_id')::bigint)
  AND (sqlc.arg('location_id')::bigint = 0 OR b.location_id = sqlc.arg('location_id')::bigint)
ORDER BY lower(i.name), l.id;

-- name: ListBalancesForItems :many
SELECT item_id, location_id, qty_base FROM stock_balances WHERE item_id = ANY($1::bigint[]);

-- The balance of one item at one place. Call after LockItems so nothing changes underneath.
-- name: BalanceOf :one
SELECT COALESCE(sum(qty_delta), 0)::bigint AS qty_base FROM inventory_movements WHERE item_id = $1 AND location_id = $2;

-- name: InsertMovement :one
INSERT INTO inventory_movements (item_id, location_id, qty_delta, movement_type, reason, entered_qty, entered_unit_id, unit_cost_cents, ref_type, ref_id, reverses_movement_id, override_reason, created_by, transfer_group)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, sqlc.narg('transfer_group')::uuid) RETURNING id;

-- name: NewTransferGroup :one
SELECT gen_random_uuid()::uuid AS id;

-- name: GetMovement :one
SELECT id, item_id, location_id, qty_delta, movement_type FROM inventory_movements WHERE id = $1;

-- name: ListMovements :many
SELECT m.id, m.item_id, i.name AS item_name, u.code AS base_unit_code, m.location_id, l.name AS location_name, m.qty_delta, m.movement_type, COALESCE(m.reason, '')::text AS reason,
       m.entered_qty, COALESCE(eu.code, '')::text AS entered_unit_code, COALESCE(m.ref_type, '')::text AS ref_type, COALESCE(m.ref_id, 0)::bigint AS ref_id,
       COALESCE(m.reverses_movement_id, 0)::bigint AS reverses_movement_id,
       EXISTS (SELECT 1 FROM inventory_movements r WHERE r.reverses_movement_id = m.id) AS reversed,
       COALESCE(m.override_reason, '')::text AS override_reason, m.created_by, cu.display_name AS created_by_name, m.created_at
FROM inventory_movements m
JOIN inventory_items i ON i.id = m.item_id
JOIN units_of_measure u ON u.id = i.base_unit_id
JOIN stock_locations l ON l.id = m.location_id
JOIN users cu ON cu.id = m.created_by
LEFT JOIN units_of_measure eu ON eu.id = m.entered_unit_id
WHERE (sqlc.arg('item_id')::bigint = 0 OR m.item_id = sqlc.arg('item_id')::bigint)
  AND (sqlc.arg('location_id')::bigint = 0 OR m.location_id = sqlc.arg('location_id')::bigint)
  AND (sqlc.arg('movement_type')::text = '' OR m.movement_type = sqlc.arg('movement_type')::text)
  AND (sqlc.arg('before_id')::bigint = 0 OR m.id < sqlc.arg('before_id')::bigint)
ORDER BY m.id DESC
LIMIT sqlc.arg('row_limit')::int;

-- name: MovementReversed :one
SELECT EXISTS (SELECT 1 FROM inventory_movements WHERE reverses_movement_id = $1)::boolean AS reversed;

-- name: ListMovementsByIDs :many
SELECT m.id, m.item_id, i.name AS item_name, u.code AS base_unit_code, m.location_id, l.name AS location_name, m.qty_delta, m.movement_type, COALESCE(m.reason, '')::text AS reason,
       m.entered_qty, COALESCE(eu.code, '')::text AS entered_unit_code, COALESCE(m.ref_type, '')::text AS ref_type, COALESCE(m.ref_id, 0)::bigint AS ref_id,
       COALESCE(m.reverses_movement_id, 0)::bigint AS reverses_movement_id,
       EXISTS (SELECT 1 FROM inventory_movements r WHERE r.reverses_movement_id = m.id) AS reversed,
       COALESCE(m.override_reason, '')::text AS override_reason, m.created_by, cu.display_name AS created_by_name, m.created_at
FROM inventory_movements m
JOIN inventory_items i ON i.id = m.item_id
JOIN units_of_measure u ON u.id = i.base_unit_id
JOIN stock_locations l ON l.id = m.location_id
JOIN users cu ON cu.id = m.created_by
LEFT JOIN units_of_measure eu ON eu.id = m.entered_unit_id
WHERE m.id = ANY($1::bigint[])
ORDER BY m.id;

-- Receipts of a purchase that a reversal has since undone (the purchase itself stays received).
-- name: ReversedReceiptItems :many
SELECT m.item_id FROM inventory_movements m
WHERE m.ref_type = 'purchase' AND m.ref_id = $1 AND m.movement_type = 'receipt'
  AND EXISTS (SELECT 1 FROM inventory_movements r WHERE r.reverses_movement_id = m.id);

-- Stage 21: stock counts.

-- A gapless counter: the row lock serialises callers and a rolled-back count gives its number back (A-05).
-- name: NextCounterValue :one
UPDATE document_counters SET next_value = next_value + 1, updated_at = now()
WHERE doc_type = $1 AND scope = '' RETURNING (next_value - 1)::bigint AS value;

-- name: InsertCount :one
INSERT INTO stock_counts (count_no, location_id, note, created_by) VALUES ($1, $2, $3, $4) RETURNING id;

-- name: LockCount :one
SELECT id, count_no, location_id, status FROM stock_counts WHERE id = $1 FOR UPDATE;

-- name: MarkCountApproved :exec
UPDATE stock_counts SET status = 'approved', approved_by = $2, approved_at = now() WHERE id = $1;

-- name: MarkCountCancelled :exec
UPDATE stock_counts SET status = 'cancelled', cancelled_by = $2, cancelled_at = now() WHERE id = $1;

-- name: GetCount :one
SELECT c.id, c.count_no, c.location_id, l.name AS location_name, c.status, COALESCE(c.note, '')::text AS note, c.created_by, cu.display_name AS created_by_name, c.created_at,
       COALESCE(c.approved_by, 0)::bigint AS approved_by, COALESCE(au.display_name, '')::text AS approved_by_name, c.approved_at::timestamptz AS approved_at,
       (SELECT count(*) FROM stock_count_lines x WHERE x.count_id = c.id)::bigint AS line_count,
       (SELECT count(*) FROM stock_count_lines x WHERE x.count_id = c.id AND x.counted_base <> x.expected_base)::bigint AS difference_count
FROM stock_counts c JOIN stock_locations l ON l.id = c.location_id JOIN users cu ON cu.id = c.created_by LEFT JOIN users au ON au.id = c.approved_by
WHERE c.id = $1;

-- name: ListCounts :many
SELECT c.id, c.count_no, c.location_id, l.name AS location_name, c.status, COALESCE(c.note, '')::text AS note, c.created_by, cu.display_name AS created_by_name, c.created_at,
       COALESCE(c.approved_by, 0)::bigint AS approved_by, COALESCE(au.display_name, '')::text AS approved_by_name, c.approved_at::timestamptz AS approved_at,
       (SELECT count(*) FROM stock_count_lines x WHERE x.count_id = c.id)::bigint AS line_count,
       (SELECT count(*) FROM stock_count_lines x WHERE x.count_id = c.id AND x.counted_base <> x.expected_base)::bigint AS difference_count
FROM stock_counts c JOIN stock_locations l ON l.id = c.location_id JOIN users cu ON cu.id = c.created_by LEFT JOIN users au ON au.id = c.approved_by
WHERE (sqlc.arg('status')::text = '' OR c.status = sqlc.arg('status')::text)
  AND (sqlc.arg('before_id')::bigint = 0 OR c.id < sqlc.arg('before_id')::bigint)
ORDER BY c.id DESC LIMIT sqlc.arg('row_limit')::int;

-- name: ListCountLines :many
SELECT x.item_id, i.name AS item_name, u.code AS base_unit_code, x.counted_base, x.entered_qty, x.entered_unit_id, eu.code AS entered_unit_code, x.expected_base, x.saved_at
FROM stock_count_lines x JOIN inventory_items i ON i.id = x.item_id JOIN units_of_measure u ON u.id = i.base_unit_id JOIN units_of_measure eu ON eu.id = x.entered_unit_id
WHERE x.count_id = $1 ORDER BY lower(i.name);

-- name: GetCountLine :one
SELECT counted_base, expected_base FROM stock_count_lines WHERE count_id = $1 AND item_id = $2;

-- name: UpsertCountLine :exec
INSERT INTO stock_count_lines (count_id, item_id, counted_base, entered_qty, entered_unit_id, expected_base)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (count_id, item_id) DO UPDATE SET counted_base = EXCLUDED.counted_base, entered_qty = EXCLUDED.entered_qty, entered_unit_id = EXCLUDED.entered_unit_id,
  expected_base = EXCLUDED.expected_base, saved_at = now();

-- name: DeleteCountLinesExcept :exec
DELETE FROM stock_count_lines WHERE count_id = $1 AND NOT (item_id = ANY($2::bigint[]));
