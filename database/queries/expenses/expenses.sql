-- Stage 26: the expenses ledger (docs/behaviour.md). Money is BIGINT cents.

-- name: InsertExpense :one
INSERT INTO expenses (expense_date, description, category, amount_cents, paid_by, paid_with, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id;

-- name: GetExpense :one
SELECT e.id, e.expense_date, e.description, e.category, e.amount_cents, e.paid_by, e.paid_with, e.created_by, u.display_name AS created_by_name,
       e.created_at, e.voided_at, e.void_reason
FROM expenses e JOIN users u ON u.id = e.created_by WHERE e.id = $1;

-- name: LockExpense :one
SELECT id, voided_at FROM expenses WHERE id = $1 FOR UPDATE;

-- name: VoidExpense :exec
UPDATE expenses SET voided_at = now(), voided_by = $2, void_reason = $3 WHERE id = $1;

-- name: ListExpenses :many
SELECT e.id, e.expense_date, e.description, e.category, e.amount_cents, e.paid_by, e.paid_with, e.created_by, u.display_name AS created_by_name,
       e.created_at, e.voided_at, e.void_reason
FROM expenses e JOIN users u ON u.id = e.created_by
WHERE e.expense_date BETWEEN sqlc.arg('from_day')::date AND sqlc.arg('to_day')::date
  AND (sqlc.arg('include_voided')::boolean OR e.voided_at IS NULL)
ORDER BY e.expense_date DESC, e.id DESC
LIMIT sqlc.arg('row_limit')::int;
