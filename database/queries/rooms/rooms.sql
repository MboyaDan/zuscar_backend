-- Stage 22: room types, rooms, guests, reservations and availability.

-- name: BusinessDate :one
SELECT (now() AT TIME ZONE COALESCE((SELECT value #>> '{}' FROM system_settings WHERE key = 'timezone')::text, 'Africa/Nairobi'::text))::date AS business_date;

-- A gapless counter: the row lock serialises callers and a rolled-back booking gives its number back (A-05).
-- name: NextCounterValue :one
UPDATE document_counters SET next_value = next_value + 1, updated_at = now()
WHERE doc_type = $1 AND scope = '' RETURNING (next_value - 1)::bigint AS value;

-- name: GetTaxRateActive :one
SELECT id, active FROM tax_rates WHERE id = $1;

-- name: ListRoomTypes :many
SELECT t.id, t.name, t.base_rate_cents, t.capacity, t.tax_rate_id, x.name AS tax_name, x.rate_bp AS tax_rate_bp, t.active,
       (SELECT count(*) FROM rooms r WHERE r.room_type_id = t.id AND r.active)::int AS room_count
FROM room_types t JOIN tax_rates x ON x.id = t.tax_rate_id
WHERE (sqlc.arg('only_active')::boolean = false OR t.active) AND (sqlc.arg('only_id')::bigint = 0 OR t.id = sqlc.arg('only_id')::bigint)
ORDER BY t.base_rate_cents, lower(t.name);

-- name: InsertRoomType :one
INSERT INTO room_types (name, base_rate_cents, capacity, tax_rate_id) VALUES ($1, $2, $3, $4) RETURNING id;

-- name: UpdateRoomType :exec
UPDATE room_types SET name = $2, base_rate_cents = $3, capacity = $4, tax_rate_id = $5, active = $6, updated_at = now() WHERE id = $1;

-- name: CountActiveRoomsOfType :one
SELECT count(*)::int FROM rooms WHERE room_type_id = $1 AND active;

-- name: ListRooms :many
SELECT r.id, r.number, r.room_type_id, t.name AS room_type, t.base_rate_cents, t.capacity, r.floor, r.physical_status, r.active
FROM rooms r JOIN room_types t ON t.id = r.room_type_id
WHERE (sqlc.arg('only_active')::boolean = false OR r.active) AND (sqlc.arg('only_id')::bigint = 0 OR r.id = sqlc.arg('only_id')::bigint)
ORDER BY length(r.number), lower(r.number);

-- name: InsertRoom :one
INSERT INTO rooms (number, room_type_id, floor) VALUES ($1, $2, $3) RETURNING id;

-- name: UpdateRoom :exec
UPDATE rooms SET number = $2, room_type_id = $3, floor = $4, active = $5, updated_at = now() WHERE id = $1;

-- name: CountUpcomingBlocks :one
SELECT count(*)::int FROM reservations WHERE room_id = $1 AND status IN ('confirmed', 'checked_in') AND check_out > $2::date;

-- name: ListGuests :many
SELECT id, full_name, COALESCE(phone, '')::text AS phone, COALESCE(email, '')::text AS email, COALESCE(notes, '')::text AS notes
FROM guests
WHERE (sqlc.arg('only_id')::bigint = 0 OR id = sqlc.arg('only_id')::bigint)
  AND (sqlc.arg('q')::text = '' OR full_name ILIKE '%' || sqlc.arg('q')::text || '%' OR phone ILIKE '%' || sqlc.arg('q')::text || '%')
  AND (sqlc.arg('before_id')::bigint = 0 OR id < sqlc.arg('before_id')::bigint)
ORDER BY id DESC LIMIT sqlc.arg('row_limit')::int;

-- name: InsertGuest :one
INSERT INTO guests (full_name, phone, email, notes) VALUES ($1, $2, $3, $4) RETURNING id;

-- name: UpdateGuest :exec
UPDATE guests SET full_name = $2, phone = $3, email = $4, notes = $5, updated_at = now() WHERE id = $1;

-- name: ListReservations :many
SELECT r.id, r.reservation_no, r.guest_id, g.full_name AS guest_name, COALESCE(g.phone, '')::text AS guest_phone,
       r.room_id, rm.number AS room_number, rt.name AS room_type, r.status, r.check_in, r.check_out, (r.check_out - r.check_in)::int AS nights,
       r.adults, r.nightly_rate_cents, COALESCE(r.notes, '')::text AS notes, COALESCE(r.cancelled_reason, '')::text AS cancelled_reason, r.created_at,
       COALESCE(st.id, 0)::bigint AS stay_id, COALESCE(f.id, 0)::bigint AS folio_id
FROM reservations r
LEFT JOIN stays st ON st.reservation_id = r.id
LEFT JOIN guest_folios f ON f.stay_id = st.id
JOIN guests g ON g.id = r.guest_id
JOIN rooms rm ON rm.id = r.room_id
JOIN room_types rt ON rt.id = rm.room_type_id
WHERE (sqlc.arg('only_id')::bigint = 0 OR r.id = sqlc.arg('only_id')::bigint)
  AND (sqlc.arg('status')::text = '' OR r.status = sqlc.arg('status')::text)
  AND (sqlc.arg('room_id')::bigint = 0 OR r.room_id = sqlc.arg('room_id')::bigint)
  AND (sqlc.arg('guest_id')::bigint = 0 OR r.guest_id = sqlc.arg('guest_id')::bigint)
  AND (sqlc.narg('window_from')::date IS NULL OR r.check_out > sqlc.narg('window_from')::date)
  AND (sqlc.narg('window_to')::date IS NULL OR r.check_in < sqlc.narg('window_to')::date)
  AND (sqlc.arg('before_id')::bigint = 0 OR r.id < sqlc.arg('before_id')::bigint)
ORDER BY r.id DESC LIMIT sqlc.arg('row_limit')::int;

-- name: LockReservation :one
SELECT id, status, room_id, guest_id, check_in, check_out, adults, nightly_rate_cents, COALESCE(notes, '')::text AS notes, reservation_no
FROM reservations WHERE id = $1 FOR UPDATE;

-- name: InsertReservation :one
INSERT INTO reservations (reservation_no, guest_id, room_id, check_in, check_out, status, adults, nightly_rate_cents, notes, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id;

-- name: UpdateReservation :exec
UPDATE reservations SET guest_id = $2, room_id = $3, check_in = $4, check_out = $5, adults = $6, nightly_rate_cents = $7, notes = $8, updated_at = now()
WHERE id = $1;

-- name: ConfirmReservation :exec
UPDATE reservations SET status = 'confirmed', updated_at = now() WHERE id = $1;

-- name: CancelReservation :exec
UPDATE reservations SET status = 'cancelled', cancelled_reason = $2, cancelled_by = $3, cancelled_at = now(), updated_at = now() WHERE id = $1;

-- name: MarkNoShow :exec
UPDATE reservations SET status = 'no_show', updated_at = now() WHERE id = $1;

-- Informational only: the exclusion constraint decides at booking time (AC-07).
-- name: Availability :many
SELECT rm.id, rm.number, rm.floor, rt.id AS room_type_id, rt.name AS room_type, rt.base_rate_cents, rt.capacity,
       COALESCE(b.id, 0)::bigint AS blocking_id, COALESCE(b.reservation_no, 0)::bigint AS blocking_no,
       b.check_in AS blocking_check_in, b.check_out AS blocking_check_out, COALESCE(b.status, '')::text AS blocking_status,
       (SELECT count(*) FROM reservations h WHERE h.room_id = rm.id AND h.status = 'tentative'
          AND h.check_in < sqlc.arg('stay_to')::date AND h.check_out > sqlc.arg('stay_from')::date)::int AS holds
FROM rooms rm
JOIN room_types rt ON rt.id = rm.room_type_id
LEFT JOIN LATERAL (
  SELECT r.id, r.reservation_no, r.check_in, r.check_out, r.status FROM reservations r
  WHERE r.room_id = rm.id AND r.status IN ('confirmed', 'checked_in')
    AND r.check_in < sqlc.arg('stay_to')::date AND r.check_out > sqlc.arg('stay_from')::date
  ORDER BY r.check_in LIMIT 1
) b ON true
WHERE rm.active AND rt.active AND rm.physical_status <> 'out_of_service'
  AND (sqlc.arg('room_type_id')::bigint = 0 OR rt.id = sqlc.arg('room_type_id')::bigint)
ORDER BY length(rm.number), lower(rm.number);
