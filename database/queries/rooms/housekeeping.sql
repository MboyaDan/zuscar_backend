-- Stage 24: housekeeping tasks.

-- name: ListHousekeepingTasks :many
SELECT t.id, t.room_id, r.number AS room_number, r.physical_status AS room_status, t.kind, t.status, t.note,
       COALESCE(t.assigned_to, 0)::bigint AS assigned_to, COALESCE(au.display_name, '')::text AS assigned_name,
       t.created_by, cu.display_name AS created_name, t.created_at, t.started_at, t.finished_at
FROM housekeeping_tasks t
JOIN rooms r ON r.id = t.room_id
JOIN users cu ON cu.id = t.created_by
LEFT JOIN users au ON au.id = t.assigned_to
WHERE (sqlc.arg('status')::text = '' OR (sqlc.arg('status')::text = 'active' AND t.status IN ('open', 'in_progress')) OR t.status = sqlc.arg('status')::text)
  AND (sqlc.arg('room_id')::bigint = 0 OR t.room_id = sqlc.arg('room_id')::bigint)
  AND (sqlc.arg('only_id')::bigint = 0 OR t.id = sqlc.arg('only_id')::bigint)
ORDER BY (t.status IN ('open', 'in_progress')) DESC, t.id DESC
LIMIT sqlc.arg('row_limit')::int;

-- name: LockHousekeepingTask :one
SELECT id, room_id, kind, status, assigned_to FROM housekeeping_tasks WHERE id = $1 FOR UPDATE;

-- name: InsertHousekeepingTask :one
INSERT INTO housekeeping_tasks (room_id, kind, note, assigned_to, created_by) VALUES ($1, $2, $3, $4, $5) RETURNING id;

-- name: ActiveCleanTask :one
SELECT id, status FROM housekeeping_tasks WHERE room_id = $1 AND kind = 'clean' AND status IN ('open', 'in_progress') FOR UPDATE;

-- name: StartHousekeepingTask :exec
UPDATE housekeeping_tasks SET status = 'in_progress', started_at = now(), assigned_to = COALESCE(assigned_to, $2) WHERE id = $1;

-- name: DoneHousekeepingTask :exec
UPDATE housekeeping_tasks SET status = 'done', started_at = COALESCE(started_at, now()), finished_at = now(), finished_by = $2 WHERE id = $1;

-- name: CancelHousekeepingTask :exec
UPDATE housekeeping_tasks SET status = 'cancelled', finished_at = now(), finished_by = $2 WHERE id = $1;

-- name: UpdateHousekeepingTask :exec
UPDATE housekeeping_tasks SET note = $2, assigned_to = $3 WHERE id = $1;

-- name: UserExistsActive :one
SELECT EXISTS (SELECT 1 FROM users WHERE id = $1 AND status = 'active');
