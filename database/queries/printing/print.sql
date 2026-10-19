-- name: InsertPrintJob :one
INSERT INTO print_jobs (doc_type, sale_id, ticket_id, order_id, reprint_of, copy_no, document, created_by)
VALUES (@doc_type, sqlc.narg('sale_id'), sqlc.narg('ticket_id'), sqlc.narg('order_id'), sqlc.narg('reprint_of'), @copy_no, @document, @created_by)
RETURNING id;

-- name: GetPrintJob :one
SELECT j.id, j.doc_type, j.sale_id, j.ticket_id, j.order_id, j.status, j.reprint_of, j.copy_no, j.document,
       j.created_by, u.display_name AS created_by_name, j.created_at, j.updated_at
FROM print_jobs j JOIN users u ON u.id = j.created_by
WHERE j.id = $1;

-- name: LockPrintJob :one
SELECT id, doc_type, sale_id, ticket_id, order_id, status, reprint_of, copy_no, created_by
FROM print_jobs WHERE id = $1 FOR UPDATE;

-- name: FindOriginalReceiptJob :one
SELECT id FROM print_jobs WHERE doc_type = 'receipt' AND sale_id = $1 AND reprint_of IS NULL;

-- name: FindOriginalTicketJob :one
SELECT id FROM print_jobs WHERE doc_type = 'kitchen_ticket' AND ticket_id = $1 AND reprint_of IS NULL;

-- name: CountReprints :one
SELECT count(*)::int AS n FROM print_jobs WHERE reprint_of = $1;

-- name: SetPrintJobStatus :exec
UPDATE print_jobs SET status = $2, updated_at = now() WHERE id = $1;

-- name: InsertPrintAttempt :one
INSERT INTO print_attempts (job_id, outcome, note, created_by) VALUES ($1, $2, $3, $4) RETURNING id;

-- name: ListPrintAttempts :many
SELECT a.id, a.outcome, a.note, a.created_by, u.display_name AS created_by_name, a.created_at
FROM print_attempts a JOIN users u ON u.id = a.created_by
WHERE a.job_id = $1 ORDER BY a.id;

-- name: ListPrintJobs :many
SELECT j.id, j.doc_type, j.sale_id, j.ticket_id, j.order_id, j.status, j.reprint_of, j.copy_no,
       j.created_by, u.display_name AS created_by_name, j.created_at, j.updated_at
FROM print_jobs j JOIN users u ON u.id = j.created_by
WHERE (@status::text = '' OR j.status = @status::text)
  AND (@doc_type::text = '' OR j.doc_type = @doc_type::text)
  AND (@created_by::bigint = 0 OR j.created_by = @created_by::bigint)
  AND (@before_id::bigint = 0 OR j.id < @before_id::bigint)
ORDER BY j.id DESC
LIMIT @row_limit::int;
