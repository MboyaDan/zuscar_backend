-- Stage 15a: voids and refunds. Money is BIGINT cents. Nothing here edits a sale or a payment; it adds records.

-- The sale row, locked for the whole correction (lock order: sale first, then the shift). The date decides whether a void is still allowed.
-- name: LockSaleForRefund :one
SELECT id, receipt_no, status, payment_status, total_cents, tax_cents, business_date, cashier_id, COALESCE(waiter_id, 0)::bigint AS waiter_id
FROM sales WHERE id = $1 FOR UPDATE;

-- name: InsertRefund :one
INSERT INTO refunds (refund_no, sale_id, kind, reason, approved_by, created_by, business_date, amount_cents, tax_cents)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id;

-- name: InsertRefundItem :exec
INSERT INTO refund_items (refund_id, sale_item_id, qty, amount_cents, tax_cents) VALUES ($1, $2, $3, $4, $5);

-- name: InsertRefundPayout :exec
INSERT INTO refund_payouts (refund_id, method, amount_cents, reference, shift_id) VALUES ($1, $2, $3, $4, $5);

-- name: SetSaleStatus :exec
UPDATE sales SET status = $2, updated_at = now() WHERE id = $1;

-- How much of each line has already been taken back.
-- name: RefundedQtyBySaleItem :many
SELECT ri.sale_item_id, COALESCE(sum(ri.qty), 0)::int AS qty, COALESCE(sum(ri.amount_cents), 0)::bigint AS amount_cents, COALESCE(sum(ri.tax_cents), 0)::bigint AS tax_cents
FROM refund_items ri JOIN refunds r ON r.id = ri.refund_id
WHERE r.sale_id = $1 GROUP BY ri.sale_item_id;

-- Confirmed money received and money returned, by method: a payout may not pass what that method received.
-- name: ConfirmedByMethod :many
SELECT method, COALESCE(sum(amount_cents), 0)::bigint AS amount_cents FROM payments WHERE sale_id = $1 AND status = 'confirmed' GROUP BY method;

-- name: ReturnedByMethod :many
SELECT p.method, COALESCE(sum(p.amount_cents), 0)::bigint AS amount_cents
FROM refund_payouts p JOIN refunds r ON r.id = p.refund_id WHERE r.sale_id = $1 GROUP BY p.method;

-- name: CountPendingPayments :one
SELECT count(*)::bigint FROM payments WHERE sale_id = $1 AND status = 'pending_confirmation';

-- name: CountRefundsForSale :one
SELECT count(*)::bigint FROM refunds WHERE sale_id = $1;

-- name: ListRefundsForSale :many
SELECT r.id, r.refund_no, r.kind, r.reason, r.approved_by, au.display_name AS approved_by_name, r.created_by, cu.display_name AS created_by_name,
       r.business_date, r.amount_cents, r.tax_cents, r.created_at
FROM refunds r JOIN users au ON au.id = r.approved_by JOIN users cu ON cu.id = r.created_by
WHERE r.sale_id = $1 ORDER BY r.id;

-- name: ListRefundItemsForSale :many
SELECT ri.refund_id, ri.sale_item_id, si.name_snapshot AS name, ri.qty, ri.amount_cents, ri.tax_cents
FROM refund_items ri JOIN refunds r ON r.id = ri.refund_id JOIN sale_items si ON si.id = ri.sale_item_id
WHERE r.sale_id = $1 ORDER BY ri.id;

-- name: ListRefundPayoutsForSale :many
SELECT p.refund_id, p.method, p.amount_cents, COALESCE(p.reference, '')::text AS reference, COALESCE(p.shift_id, 0)::bigint AS shift_id
FROM refund_payouts p JOIN refunds r ON r.id = p.refund_id WHERE r.sale_id = $1 ORDER BY p.id;

-- Cash handed back from this drawer (expected cash subtracts it).
-- name: ShiftCashRefunds :one
SELECT COALESCE(sum(amount_cents), 0)::bigint FROM refund_payouts WHERE shift_id = $1 AND method = 'cash';

-- name: ShiftVoidedCount :one
SELECT count(*)::bigint FROM sales WHERE shift_id = $1 AND status = 'voided';
