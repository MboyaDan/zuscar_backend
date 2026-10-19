-- Stage 11a: sales and payments. Money is BIGINT cents.

-- name: InsertSale :one
INSERT INTO sales (receipt_no, order_id, business_date, subtotal_cents, discount_cents, tax_cents, total_cents, payment_status, cashier_id, waiter_id, tax_status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING id;

-- name: InsertSaleItem :exec
INSERT INTO sale_items (sale_id, order_item_id, product_id, name_snapshot, module, qty, unit_price_cents, modifiers_text, modifiers_cents,
                        discount_cents, discount_reason, discount_approved_by, tax_code, tax_treatment, tax_rate_bp, tax_cents, line_total_cents)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17);

-- name: InsertPayment :one
INSERT INTO payments (sale_id, purpose, method, status, amount_cents, reference, created_by, confirmed_by, confirmed_at, note)
VALUES ($1, 'sale_payment', $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id;

-- name: SetSalePaymentStatus :exec
UPDATE sales SET payment_status = $2, updated_at = now() WHERE id = $1;

-- name: PaymentTotals :one
SELECT COALESCE(sum(amount_cents) FILTER (WHERE status = 'confirmed'), 0)::bigint AS confirmed_cents,
       COALESCE(sum(amount_cents) FILTER (WHERE status = 'pending_confirmation'), 0)::bigint AS pending_cents
FROM payments WHERE sale_id = $1;

-- name: GetSaleHead :one
SELECT s.id, s.receipt_no, COALESCE(s.order_id, 0)::bigint AS order_id, COALESCE(o.order_no, 0)::bigint AS order_no, s.business_date, s.subtotal_cents, s.discount_cents,
       s.tax_cents, s.total_cents, s.status, s.payment_status, s.source, s.cashier_id, cu.display_name AS cashier_name, COALESCE(s.waiter_id, 0)::bigint AS waiter_id,
       s.tax_status, s.created_at, s.updated_at
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
