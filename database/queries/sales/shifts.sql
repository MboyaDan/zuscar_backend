-- Stage 12: cashier shifts and cash movements. Money is BIGINT cents.

-- The shift a person takes money on. FOR SHARE keeps a shift from being closed while a payment is being written to it;
-- CloseShift takes FOR UPDATE, so one waits for the other.
-- name: OpenShiftForUser :one
SELECT id FROM cashier_shifts WHERE user_id = $1 AND status = 'open' FOR SHARE;

-- name: InsertShift :one
INSERT INTO cashier_shifts (user_id, opening_float_cents, notes) VALUES ($1, $2, $3) RETURNING id;

-- name: LockShift :one
SELECT id, user_id, status, opening_float_cents FROM cashier_shifts WHERE id = $1 FOR UPDATE;

-- name: GetShift :one
SELECT sh.id, sh.user_id, u.display_name AS user_name, sh.status, sh.opened_at, sh.opening_float_cents, sh.closed_at, COALESCE(sh.closed_by, 0)::bigint AS closed_by,
       sh.declared_cash_cents, sh.expected_cash_cents, sh.variance_cents, sh.reconciled_at, COALESCE(sh.reconciled_by, 0)::bigint AS reconciled_by, sh.notes
FROM cashier_shifts sh JOIN users u ON u.id = sh.user_id
WHERE sh.id = $1;

-- name: GetOpenShiftIDForUser :one
SELECT id FROM cashier_shifts WHERE user_id = $1 AND status = 'open';

-- name: ListShifts :many
SELECT sh.id, sh.user_id, u.display_name AS user_name, sh.status, sh.opened_at, sh.opening_float_cents, sh.closed_at, COALESCE(sh.closed_by, 0)::bigint AS closed_by,
       sh.declared_cash_cents, sh.expected_cash_cents, sh.variance_cents, sh.reconciled_at, COALESCE(sh.reconciled_by, 0)::bigint AS reconciled_by, sh.notes
FROM cashier_shifts sh JOIN users u ON u.id = sh.user_id
WHERE (sqlc.arg('status')::text = '' OR sh.status = sqlc.arg('status')::text)
  AND (sqlc.arg('user_id')::bigint = 0 OR sh.user_id = sqlc.arg('user_id')::bigint)
  AND (sqlc.arg('before_id')::bigint = 0 OR sh.id < sqlc.arg('before_id')::bigint)
ORDER BY sh.id DESC
LIMIT sqlc.arg('row_limit')::int;

-- Everything the shift took, by method and status. Cash that is confirmed counts toward expected cash; the rest is listed apart.
-- name: ShiftPaymentTotals :many
SELECT method, status, count(*)::bigint AS payments, COALESCE(sum(amount_cents), 0)::bigint AS amount_cents
FROM payments WHERE shift_id = $1 GROUP BY method, status ORDER BY method, status;

-- name: ShiftMovementTotals :many
SELECT kind, count(*)::bigint AS movements, COALESCE(sum(amount_cents), 0)::bigint AS amount_cents
FROM cash_movements WHERE shift_id = $1 GROUP BY kind;

-- name: ShiftSaleCount :one
SELECT count(*)::bigint AS sales, COALESCE(sum(total_cents), 0)::bigint AS total_cents FROM sales WHERE shift_id = $1 AND status <> 'voided';

-- name: InsertCashMovement :one
INSERT INTO cash_movements (shift_id, kind, amount_cents, reason, created_by) VALUES ($1, $2, $3, $4, $5) RETURNING id;

-- name: ListCashMovements :many
SELECT m.id, m.kind, m.amount_cents, m.reason, m.created_by, u.display_name AS created_by_name, m.created_at
FROM cash_movements m JOIN users u ON u.id = m.created_by WHERE m.shift_id = $1 ORDER BY m.id;

-- name: CloseShift :exec
UPDATE cashier_shifts SET status = 'closed', closed_at = now(), closed_by = sqlc.arg('closed_by')::bigint, declared_cash_cents = sqlc.arg('declared')::bigint, expected_cash_cents = sqlc.arg('expected')::bigint,
       variance_cents = sqlc.arg('declared')::bigint - sqlc.arg('expected')::bigint,
       notes = CASE WHEN sqlc.arg('notes')::text = '' THEN notes ELSE sqlc.arg('notes')::text END
WHERE id = sqlc.arg('id')::bigint;

-- name: ReconcileShift :exec
UPDATE cashier_shifts SET status = 'reconciled', reconciled_at = now(), reconciled_by = sqlc.arg('reconciled_by')::bigint, notes = CASE WHEN sqlc.arg('note')::text = '' THEN notes ELSE sqlc.arg('note')::text END WHERE id = sqlc.arg('id')::bigint;
