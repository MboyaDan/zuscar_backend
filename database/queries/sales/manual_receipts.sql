-- Stage 16: outage reconciliation. A paper receipt entered after the fact is a sale with source 'outage_reconciliation'.

-- name: InsertOutageSale :one
INSERT INTO sales (receipt_no, order_id, business_date, subtotal_cents, discount_cents, tax_cents, total_cents, payment_status, source, manual_receipt_no, cashier_id, tax_status)
VALUES ($1, NULL, $2, $3, 0, $4, $5, $6, 'outage_reconciliation', $7, $8, $9) RETURNING id;

-- name: InsertManualReceipt :one
INSERT INTO manual_receipt_reconciliations (manual_receipt_no, sale_id, original_occurred_at, paper_total_cents, entered_by, discrepancy_note, source_note)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id;

-- Newest first. manual_receipt_no, when given, is an exact (already normalised) match. before_id pages by entry id.
-- name: ListManualReceipts :many
SELECT m.id, m.manual_receipt_no, m.sale_id, s.receipt_no, m.original_occurred_at, m.paper_total_cents, s.total_cents AS sale_total_cents,
       s.status AS sale_status, m.entered_by, u.display_name AS entered_by_name, COALESCE(m.discrepancy_note, '')::text AS discrepancy_note, m.source_note, m.created_at
FROM manual_receipt_reconciliations m
JOIN sales s ON s.id = m.sale_id
JOIN users u ON u.id = m.entered_by
WHERE (sqlc.arg('manual_receipt_no')::text = '' OR m.manual_receipt_no = sqlc.arg('manual_receipt_no')::text)
  AND (sqlc.arg('before_id')::bigint = 0 OR m.id < sqlc.arg('before_id')::bigint)
ORDER BY m.id DESC
LIMIT sqlc.arg('row_limit')::int;

-- name: GetManualReceiptForSale :one
SELECT m.manual_receipt_no, m.original_occurred_at, m.paper_total_cents, COALESCE(m.discrepancy_note, '')::text AS discrepancy_note, m.source_note, m.entered_by, u.display_name AS entered_by_name
FROM manual_receipt_reconciliations m JOIN users u ON u.id = m.entered_by WHERE m.sale_id = $1;

-- name: ManualReceiptTaken :one
SELECT EXISTS (SELECT 1 FROM sales WHERE manual_receipt_no = $1)::boolean AS taken;
