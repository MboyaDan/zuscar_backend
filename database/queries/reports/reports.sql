-- Stage 25: read-only report queries. Money is BIGINT cents. Nothing here writes.

-- name: SalesByDay :many
SELECT s.business_date, count(*)::bigint AS sales_count, COALESCE(sum(s.subtotal_cents), 0)::bigint AS gross_cents,
       COALESCE(sum(s.discount_cents), 0)::bigint AS discount_cents, COALESCE(sum(s.tax_cents), 0)::bigint AS tax_cents,
       COALESCE(sum(s.total_cents), 0)::bigint AS total_cents,
       COALESCE(sum(s.total_cents) FILTER (WHERE s.payment_status = 'charged_to_folio'), 0)::bigint AS room_charged_cents
FROM sales s
WHERE s.business_date BETWEEN sqlc.arg('from_day')::date AND sqlc.arg('to_day')::date
  AND (sqlc.arg('cashier_id')::bigint = 0 OR s.cashier_id = sqlc.arg('cashier_id')::bigint)
GROUP BY s.business_date ORDER BY s.business_date;

-- name: ReturnsByDay :many
SELECT r.business_date, count(*) FILTER (WHERE r.kind = 'refund')::bigint AS refunds, count(*) FILTER (WHERE r.kind = 'void')::bigint AS voids,
       COALESCE(sum(r.amount_cents), 0)::bigint AS amount_cents
FROM refunds r
WHERE r.business_date BETWEEN sqlc.arg('from_day')::date AND sqlc.arg('to_day')::date
GROUP BY r.business_date ORDER BY r.business_date;

-- name: SalesByModule :many
SELECT si.module, COALESCE(sum(si.qty), 0)::bigint AS items_sold, COALESCE(sum(si.line_total_cents), 0)::bigint AS total_cents, COALESCE(sum(si.tax_cents), 0)::bigint AS tax_cents
FROM sale_items si JOIN sales s ON s.id = si.sale_id
WHERE s.business_date BETWEEN sqlc.arg('from_day')::date AND sqlc.arg('to_day')::date
GROUP BY si.module ORDER BY si.module;

-- name: SalesByPaymentStatus :many
SELECT s.payment_status, count(*)::bigint AS sales_count, COALESCE(sum(s.total_cents), 0)::bigint AS total_cents
FROM sales s
WHERE s.business_date BETWEEN sqlc.arg('from_day')::date AND sqlc.arg('to_day')::date
GROUP BY s.payment_status ORDER BY s.payment_status;

-- name: TopProducts :many
SELECT si.name_snapshot AS name, si.module, COALESCE(sum(si.qty), 0)::bigint AS qty, COALESCE(sum(si.line_total_cents), 0)::bigint AS total_cents
FROM sale_items si JOIN sales s ON s.id = si.sale_id
WHERE s.business_date BETWEEN sqlc.arg('from_day')::date AND sqlc.arg('to_day')::date
GROUP BY si.name_snapshot, si.module ORDER BY sum(si.line_total_cents) DESC, si.name_snapshot LIMIT 15;

-- name: CollectionsByMethod :many
SELECT (p.confirmed_at AT TIME ZONE sqlc.arg('tz')::text)::date AS day, p.method, p.purpose, count(*)::bigint AS payments, COALESCE(sum(p.amount_cents), 0)::bigint AS amount_cents
FROM payments p
WHERE p.status = 'confirmed'
  AND (p.confirmed_at AT TIME ZONE sqlc.arg('tz')::text)::date BETWEEN sqlc.arg('from_day')::date AND sqlc.arg('to_day')::date
  AND (sqlc.arg('cashier_id')::bigint = 0 OR p.created_by = sqlc.arg('cashier_id')::bigint)
GROUP BY 1, p.method, p.purpose ORDER BY 1, p.method, p.purpose;

-- name: PayoutsByMethod :many
SELECT r.business_date AS day, po.method, count(*)::bigint AS payouts, COALESCE(sum(po.amount_cents), 0)::bigint AS amount_cents
FROM refund_payouts po JOIN refunds r ON r.id = po.refund_id
WHERE r.business_date BETWEEN sqlc.arg('from_day')::date AND sqlc.arg('to_day')::date
GROUP BY 1, po.method ORDER BY 1, po.method;

-- name: PendingPayments :many
SELECT p.method, count(*)::bigint AS payments, COALESCE(sum(p.amount_cents), 0)::bigint AS amount_cents
FROM payments p WHERE p.status = 'pending_confirmation' GROUP BY p.method ORDER BY p.method;

-- name: SalesDue :many
SELECT s.id, s.receipt_no, s.business_date, s.total_cents, s.payment_status, cu.display_name AS cashier_name,
       COALESCE((SELECT sum(p.amount_cents) FROM payments p WHERE p.sale_id = s.id AND p.status = 'confirmed'), 0)::bigint AS paid_cents,
       COALESCE((SELECT sum(r.amount_cents) FROM refunds r WHERE r.sale_id = s.id), 0)::bigint AS refunded_cents
FROM sales s JOIN users cu ON cu.id = s.cashier_id
WHERE s.status IN ('finalised', 'partially_refunded') AND s.payment_status IN ('unpaid', 'partially_paid')
ORDER BY s.business_date, s.id
LIMIT 500;

-- name: FoliosDue :many
SELECT f.id, f.folio_no, f.status, g.full_name AS guest_name, COALESCE(rm.number, '')::text AS room_number, COALESCE(st.status, '')::text AS stay_status,
       COALESCE((SELECT sum(i.amount_cents) FROM folio_items i WHERE i.folio_id = f.id), 0)::bigint AS items_cents,
       COALESCE((SELECT sum(p.amount_cents) FROM payments p WHERE p.folio_id = f.id AND p.status = 'confirmed'), 0)::bigint AS paid_cents,
       COALESCE((SELECT sum(p.amount_cents) FROM payments p WHERE p.folio_id = f.id AND p.status = 'pending_confirmation'), 0)::bigint AS pending_cents,
       f.write_off_cents
FROM guest_folios f
JOIN guests g ON g.id = f.guest_id
LEFT JOIN stays st ON st.id = f.stay_id
LEFT JOIN rooms rm ON rm.id = st.room_id
WHERE f.status = 'open'
ORDER BY f.folio_no
LIMIT 500;

-- name: DiscountLines :many
SELECT s.receipt_no, s.business_date, si.name_snapshot AS item, si.qty, si.discount_cents, COALESCE(si.discount_reason, '')::text AS reason,
       COALESCE(au.display_name, '')::text AS approved_by, cu.display_name AS cashier_name
FROM sale_items si
JOIN sales s ON s.id = si.sale_id
JOIN users cu ON cu.id = s.cashier_id
LEFT JOIN users au ON au.id = si.discount_approved_by
WHERE si.discount_cents > 0 AND s.business_date BETWEEN sqlc.arg('from_day')::date AND sqlc.arg('to_day')::date
ORDER BY s.business_date, s.receipt_no, si.id
LIMIT 1000;

-- name: RefundRows :many
SELECT r.refund_no, r.business_date, r.kind, s.receipt_no, r.amount_cents, r.reason, ap.display_name AS approved_by, cr.display_name AS created_by,
       COALESCE((SELECT string_agg(po.method, ', ' ORDER BY po.method) FROM refund_payouts po WHERE po.refund_id = r.id), '')::text AS paid_out_by
FROM refunds r
JOIN sales s ON s.id = r.sale_id
JOIN users ap ON ap.id = r.approved_by
JOIN users cr ON cr.id = r.created_by
WHERE r.business_date BETWEEN sqlc.arg('from_day')::date AND sqlc.arg('to_day')::date
ORDER BY r.business_date, r.refund_no
LIMIT 1000;

-- name: StockOnHand :many
SELECT i.id AS item_id, i.name AS item_name, u.code AS unit_code, l.id AS location_id, l.name AS location_name, COALESCE(b.qty_base, 0)::bigint AS qty_base,
       COALESCE(i.low_stock_threshold, -1)::bigint AS low_threshold,
       COALESCE((SELECT sum(b2.qty_base) FROM stock_balances b2 WHERE b2.item_id = i.id), 0)::bigint AS item_total
FROM inventory_items i
JOIN units_of_measure u ON u.id = i.base_unit_id
JOIN stock_locations l ON l.active
LEFT JOIN stock_balances b ON b.item_id = i.id AND b.location_id = l.id
WHERE i.active AND (b.qty_base IS NOT NULL OR i.low_stock_threshold IS NOT NULL)
ORDER BY lower(i.name), l.id;

-- name: RoomNights :many
SELECT d::date AS night,
       (SELECT count(*) FROM rooms WHERE active)::bigint AS rooms,
       COALESCE(x.nights, 0)::bigint AS nights,
       COALESCE(x.revenue, 0)::bigint AS revenue_cents,
       COALESCE(x.tax, 0)::bigint AS tax_cents
FROM generate_series(sqlc.arg('from_day')::date, sqlc.arg('to_day')::date, interval '1 day') d
LEFT JOIN (
  SELECT fi.night_date, count(*) AS nights, sum(fi.amount_cents + COALESCE(rv.amount_cents, 0)) AS revenue, sum(fi.tax_cents + COALESCE(rv.tax_cents, 0)) AS tax
  FROM folio_items fi LEFT JOIN folio_items rv ON rv.reverses_item_id = fi.id
  WHERE fi.kind = 'room_night'
  GROUP BY fi.night_date
) x ON x.night_date = d::date
ORDER BY d;

-- name: RoomStatusCounts :many
SELECT physical_status AS status, count(*)::bigint AS rooms FROM rooms WHERE active GROUP BY physical_status ORDER BY physical_status;

-- name: HouseToday :one
SELECT
  (SELECT count(*) FROM stays WHERE status = 'in_house')::bigint AS in_house,
  (SELECT count(*) FROM reservations WHERE status = 'confirmed' AND check_in = sqlc.arg('day')::date)::bigint AS arrivals,
  (SELECT count(*) FROM stays WHERE status = 'in_house' AND expected_out = sqlc.arg('day')::date)::bigint AS departures;
