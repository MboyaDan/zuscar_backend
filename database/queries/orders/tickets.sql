-- Stage 10b-1: kitchen tickets. A ticket is raised inside the order transaction that causes it.

-- The business day in the venue's time zone setting (S-05); falls back to Africa/Nairobi.
-- name: BusinessDate :one
SELECT (now() AT TIME ZONE COALESCE((SELECT value #>> '{}' FROM system_settings WHERE key = 'timezone')::text, 'Africa/Nairobi'::text))::date AS business_date;

-- name: OpenTicketCounter :exec
INSERT INTO document_counters (doc_type, scope, prefix, next_value) VALUES ('kitchen_ticket', $1, '', 1)
ON CONFLICT (doc_type, scope) DO NOTHING;

-- Gapless within a business day: the row lock serialises callers and a rolled-back ticket gives its number back (A-05).
-- name: NextTicketNo :one
UPDATE document_counters SET next_value = next_value + 1, updated_at = now()
WHERE doc_type = 'kitchen_ticket' AND scope = $1 RETURNING (next_value - 1)::bigint AS value;

-- name: InsertTicket :one
INSERT INTO kitchen_tickets (order_id, business_date, ticket_no, kind, parent_ticket_id, created_by)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING id;

-- name: InsertTicketItem :exec
INSERT INTO kitchen_ticket_items (ticket_id, order_item_id, name_snapshot, qty, change_type, modifiers_text, note)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: InsertTicketHistory :exec
INSERT INTO ticket_status_history (ticket_id, from_status, to_status, changed_by) VALUES ($1, $2, $3, $4);

-- Lines that still have to go to the kitchen: active, kitchen-routed, never put on a ticket.
-- name: ListUnsentKitchenItems :many
SELECT i.id, p.name, i.qty, i.note,
       COALESCE((SELECT string_agg(m.name, ', ' ORDER BY m.name) FROM order_item_modifiers im JOIN modifiers m ON m.id = im.modifier_id WHERE im.order_item_id = i.id), '')::text AS modifiers_text
FROM order_items i JOIN products p ON p.id = i.product_id
WHERE i.order_id = $1 AND i.status = 'active' AND p.kitchen_routed
  AND NOT EXISTS (SELECT 1 FROM kitchen_ticket_items k WHERE k.order_item_id = i.id AND k.change_type = 'add')
ORDER BY i.id;

-- name: GetItemForTicket :one
SELECT i.id, p.name, i.qty, i.note, i.status,
       COALESCE((SELECT string_agg(m.name, ', ' ORDER BY m.name) FROM order_item_modifiers im JOIN modifiers m ON m.id = im.modifier_id WHERE im.order_item_id = i.id), '')::text AS modifiers_text,
       EXISTS (SELECT 1 FROM kitchen_ticket_items k WHERE k.order_item_id = i.id AND k.change_type = 'add') AS sent
FROM order_items i JOIN products p ON p.id = i.product_id
WHERE i.id = $1 AND i.order_id = $2;

-- Lines the kitchen knows about and has not been told to drop.
-- name: ListLiveSentItems :many
SELECT i.id, p.name, i.qty, i.note,
       COALESCE((SELECT string_agg(m.name, ', ' ORDER BY m.name) FROM order_item_modifiers im JOIN modifiers m ON m.id = im.modifier_id WHERE im.order_item_id = i.id), '')::text AS modifiers_text
FROM order_items i JOIN products p ON p.id = i.product_id
WHERE i.order_id = $1 AND i.status = 'active'
  AND EXISTS (SELECT 1 FROM kitchen_ticket_items k WHERE k.order_item_id = i.id AND k.change_type = 'add')
  AND NOT EXISTS (SELECT 1 FROM kitchen_ticket_items k WHERE k.order_item_id = i.id AND k.change_type = 'cancel')
ORDER BY i.id;

-- name: GetOriginalTicketID :one
SELECT id FROM kitchen_tickets WHERE order_id = $1 AND kind = 'original';

-- name: MarkOrderSent :exec
UPDATE orders SET status = 'sent', version = version + 1, updated_at = now() WHERE id = $1;

-- name: LockOpenTicketsForOrder :many
SELECT id, status FROM kitchen_tickets WHERE order_id = $1 AND status IN ('submitted', 'preparing', 'ready') ORDER BY id FOR UPDATE;

-- name: LockTicket :one
SELECT id, status FROM kitchen_tickets WHERE id = $1 FOR UPDATE;

-- name: SetTicketStatus :exec
UPDATE kitchen_tickets SET status = $2, updated_at = now() WHERE id = $1;

-- name: GetTicketHead :one
SELECT t.id, t.order_id, o.order_no, o.order_type, COALESCE(dt.label, '')::text AS table_label, t.business_date, t.ticket_no, t.kind,
       t.parent_ticket_id, t.status, t.created_by, u.display_name AS created_by_name, t.created_at, t.updated_at
FROM kitchen_tickets t
JOIN orders o ON o.id = t.order_id
LEFT JOIN dining_tables dt ON dt.id = o.table_id
JOIN users u ON u.id = t.created_by
WHERE t.id = $1;

-- Oldest first, because that is the order the kitchen works in.
-- name: ListTickets :many
SELECT t.id, t.order_id, o.order_no, o.order_type, COALESCE(dt.label, '')::text AS table_label, t.business_date, t.ticket_no, t.kind,
       t.parent_ticket_id, t.status, t.created_by, u.display_name AS created_by_name, t.created_at, t.updated_at
FROM kitchen_tickets t
JOIN orders o ON o.id = t.order_id
LEFT JOIN dining_tables dt ON dt.id = o.table_id
JOIN users u ON u.id = t.created_by
WHERE t.status = ANY(sqlc.arg('statuses')::text[])
  AND (sqlc.narg('order_id')::bigint IS NULL OR t.order_id = sqlc.narg('order_id')::bigint)
  AND (sqlc.narg('after_id')::bigint IS NULL OR t.id > sqlc.narg('after_id')::bigint)
ORDER BY t.id
LIMIT sqlc.arg('row_limit')::int;

-- name: ListTicketItems :many
SELECT id, ticket_id, order_item_id, name_snapshot, qty, change_type, modifiers_text, note
FROM kitchen_ticket_items WHERE ticket_id = ANY($1::bigint[]) ORDER BY ticket_id, id;

-- name: ListTicketHistory :many
SELECT h.id, h.ticket_id, COALESCE(h.from_status, '')::text AS from_status, h.to_status, h.changed_by, u.display_name AS changed_by_name, h.changed_at
FROM ticket_status_history h JOIN users u ON u.id = h.changed_by WHERE h.ticket_id = $1 ORDER BY h.id;
