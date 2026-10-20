-- Stage 11a: sales and payments. Money is BIGINT cents.

-- name: InsertSale :one
INSERT INTO sales (receipt_no, order_id, business_date, subtotal_cents, discount_cents, tax_cents, total_cents, payment_status, cashier_id, waiter_id, tax_status, shift_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING id;

-- name: InsertSaleItem :exec
INSERT INTO sale_items (sale_id, order_item_id, product_id, name_snapshot, module, qty, unit_price_cents, modifiers_text, modifiers_cents,
                        discount_cents, discount_reason, discount_approved_by, tax_code, tax_treatment, tax_rate_bp, tax_cents, line_total_cents,
                        inventory_item_id, stock_units_per_sale, stock_location_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20);

-- name: ListSaleStock :many
SELECT id AS sale_item_id, qty, inventory_item_id, stock_units_per_sale, stock_location_id
FROM sale_items WHERE sale_id = $1 AND inventory_item_id IS NOT NULL ORDER BY id;

-- name: InsertPayment :one
INSERT INTO payments (sale_id, purpose, method, status, amount_cents, reference, created_by, confirmed_by, confirmed_at, note, shift_id)
VALUES ($1, 'sale_payment', $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id;

-- name: SetSalePaymentStatus :exec
UPDATE sales SET payment_status = $2, updated_at = now() WHERE id = $1;

-- name: PaymentTotals :one
SELECT COALESCE(sum(amount_cents) FILTER (WHERE status = 'confirmed'), 0)::bigint AS confirmed_cents,
       COALESCE(sum(amount_cents) FILTER (WHERE status = 'pending_confirmation'), 0)::bigint AS pending_cents
FROM payments WHERE sale_id = $1;

-- name: GetSaleHead :one
SELECT s.id, s.receipt_no, COALESCE(s.order_id, 0)::bigint AS order_id, COALESCE(o.order_no, 0)::bigint AS order_no, s.business_date, s.subtotal_cents, s.discount_cents,
       s.tax_cents, s.total_cents, s.status, s.payment_status, s.source, s.cashier_id, cu.display_name AS cashier_name, COALESCE(s.waiter_id, 0)::bigint AS waiter_id,
       s.tax_status, s.created_at, s.updated_at, COALESCE(s.shift_id, 0)::bigint AS shift_id
FROM sales s
JOIN users cu ON cu.id = s.cashier_id
LEFT JOIN orders o ON o.id = s.order_id
WHERE s.id = $1;

-- name: GetSaleIDByOrder :one
SELECT id FROM sales WHERE order_id = $1;

-- name: ListSaleItems :many
SELECT id, product_id, name_snapshot, module, qty, unit_price_cents, modifiers_text, modifiers_cents, discount_cents, COALESCE(discount_reason, '')::text AS discount_reason,
       tax_code, tax_treatment, tax_rate_bp, tax_cents, line_total_cents
FROM sale_items WHERE sale_id = $1 ORDER BY id;

-- name: ListSalePayments :many
SELECT id, method, status, amount_cents, COALESCE(reference, '')::text AS reference, created_by, created_at, confirmed_at
FROM payments WHERE sale_id = $1 ORDER BY id;

-- Stage 11b: later payments, confirmation, failure, and the sales and payments lists.

-- name: LockSale :one
SELECT id, receipt_no, status, payment_status, total_cents, cashier_id, COALESCE(waiter_id, 0)::bigint AS waiter_id
FROM sales WHERE id = $1 FOR UPDATE;

-- name: GetPaymentTarget :one
SELECT id, sale_id, folio_id FROM payments WHERE id = $1;

-- name: LockPayment :one
SELECT id, sale_id, status FROM payments WHERE id = $1 FOR UPDATE;

-- name: ConfirmPayment :exec
UPDATE payments SET status = 'confirmed', confirmed_by = $2, confirmed_at = now(), note = $3, updated_at = now() WHERE id = $1;

-- name: FailPayment :exec
UPDATE payments SET status = 'failed', failed_by = $2, failed_at = now(), note = $3, updated_at = now() WHERE id = $1;

-- name: GetPaymentRecord :one
SELECT p.id, p.sale_id, p.folio_id, p.purpose, COALESCE(s.receipt_no, 0)::bigint AS receipt_no, p.method, p.status, p.amount_cents, COALESCE(p.reference, '')::text AS reference,
       p.created_by, cu.display_name AS created_by_name, p.created_at, COALESCE(p.confirmed_by, 0)::bigint AS confirmed_by, p.confirmed_at,
       COALESCE(p.failed_by, 0)::bigint AS failed_by, p.failed_at, p.note, COALESCE(p.shift_id, 0)::bigint AS shift_id
FROM payments p
LEFT JOIN sales s ON s.id = p.sale_id
JOIN users cu ON cu.id = p.created_by
WHERE p.id = $1;

-- Newest first. only_user limits the list to payments on sales that person took or served (S-47); zero means no limit.
-- name: ListPayments :many
SELECT p.id, p.sale_id, p.folio_id, p.purpose, COALESCE(s.receipt_no, 0)::bigint AS receipt_no, p.method, p.status, p.amount_cents, COALESCE(p.reference, '')::text AS reference,
       p.created_by, cu.display_name AS created_by_name, p.created_at, COALESCE(p.confirmed_by, 0)::bigint AS confirmed_by, p.confirmed_at,
       COALESCE(p.failed_by, 0)::bigint AS failed_by, p.failed_at, p.note, COALESCE(p.shift_id, 0)::bigint AS shift_id
FROM payments p
LEFT JOIN sales s ON s.id = p.sale_id
JOIN users cu ON cu.id = p.created_by
WHERE (sqlc.arg('status')::text = '' OR p.status = sqlc.arg('status')::text)
  AND (sqlc.arg('method')::text = '' OR p.method = sqlc.arg('method')::text)
  AND (sqlc.arg('shift_id')::bigint = 0 OR p.shift_id = sqlc.arg('shift_id')::bigint)
  AND (sqlc.arg('sale_id')::bigint = 0 OR p.sale_id = sqlc.arg('sale_id')::bigint)
  AND (sqlc.arg('folio_id')::bigint = 0 OR p.folio_id = sqlc.arg('folio_id')::bigint)
  AND (sqlc.arg('only_user')::bigint = 0 OR (p.sale_id IS NOT NULL AND (s.cashier_id = sqlc.arg('only_user')::bigint OR s.waiter_id = sqlc.arg('only_user')::bigint)))
  AND (sqlc.arg('before_id')::bigint = 0 OR p.id < sqlc.arg('before_id')::bigint)
ORDER BY p.id DESC
LIMIT sqlc.arg('row_limit')::int;

-- name: ListSales :many
SELECT s.id, s.receipt_no, COALESCE(s.order_id, 0)::bigint AS order_id, COALESCE(o.order_no, 0)::bigint AS order_no, s.business_date, s.subtotal_cents,
       s.discount_cents, s.tax_cents, s.total_cents, s.status, s.payment_status, s.source, s.cashier_id, cu.display_name AS cashier_name,
       COALESCE(s.waiter_id, 0)::bigint AS waiter_id, s.created_at,
       COALESCE((SELECT sum(p.amount_cents) FROM payments p WHERE p.sale_id = s.id AND p.status = 'confirmed'), 0)::bigint AS paid_cents,
       COALESCE((SELECT sum(p.amount_cents) FROM payments p WHERE p.sale_id = s.id AND p.status = 'pending_confirmation'), 0)::bigint AS pending_cents
FROM sales s
JOIN users cu ON cu.id = s.cashier_id
LEFT JOIN orders o ON o.id = s.order_id
WHERE (sqlc.arg('status')::text = '' OR s.status = sqlc.arg('status')::text)
  AND (sqlc.arg('payment_status')::text = '' OR s.payment_status = sqlc.arg('payment_status')::text)
  AND (sqlc.narg('business_date')::date IS NULL OR s.business_date = sqlc.narg('business_date')::date)
  AND (sqlc.arg('only_user')::bigint = 0 OR s.cashier_id = sqlc.arg('only_user')::bigint OR s.waiter_id = sqlc.arg('only_user')::bigint)
  AND (sqlc.arg('before_id')::bigint = 0 OR s.id < sqlc.arg('before_id')::bigint)
ORDER BY s.id DESC
LIMIT sqlc.arg('row_limit')::int;

-- Folio payments (Stage 23). The folio row is locked by the rooms module before these run.
-- name: InsertFolioPayment :one
INSERT INTO payments (folio_id, purpose, method, status, amount_cents, reference, created_by, confirmed_by, confirmed_at, note, shift_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING id;

-- name: FolioPaymentTotals :one
SELECT COALESCE(sum(amount_cents) FILTER (WHERE status = 'confirmed'), 0)::bigint AS confirmed_cents,
       COALESCE(sum(amount_cents) FILTER (WHERE status = 'pending_confirmation'), 0)::bigint AS pending_cents
FROM payments WHERE folio_id = $1;
