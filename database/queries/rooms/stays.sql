-- Stage 23: check-in, stays, folios, room status.

-- name: LockRoom :one
SELECT id, number, room_type_id, physical_status, active FROM rooms WHERE id = $1 FOR UPDATE;

-- name: SetRoomStatus :exec
UPDATE rooms SET physical_status = $2, updated_at = now() WHERE id = $1;

-- name: InsertRoomStatusHistory :exec
INSERT INTO room_status_history (room_id, from_status, to_status, changed_by, reason) VALUES ($1, $2, $3, $4, $5);

-- name: RoomTaxRate :one
SELECT x.rate_bp FROM rooms r JOIN room_types t ON t.id = r.room_type_id JOIN tax_rates x ON x.id = t.tax_rate_id WHERE r.id = $1;

-- name: CheckInReservation :exec
UPDATE reservations SET status = 'checked_in', updated_at = now() WHERE id = $1;

-- name: CheckOutReservation :exec
UPDATE reservations SET status = 'checked_out', updated_at = now() WHERE id = $1;

-- name: MoveReservationRoom :exec
UPDATE reservations SET room_id = $2, updated_at = now() WHERE id = $1;

-- name: ExtendReservation :exec
UPDATE reservations SET check_out = $2, updated_at = now() WHERE id = $1;

-- name: InsertStay :one
INSERT INTO stays (reservation_id, room_id, guest_id, expected_out, checked_in_by) VALUES ($1, $2, $3, $4, $5) RETURNING id;

-- name: LockStay :one
SELECT id, reservation_id, room_id, guest_id, status, expected_out FROM stays WHERE id = $1 FOR UPDATE;

-- name: MoveStayRoom :exec
UPDATE stays SET room_id = $2 WHERE id = $1;

-- name: SetStayExpectedOut :exec
UPDATE stays SET expected_out = $2 WHERE id = $1;

-- name: EndStay :exec
UPDATE stays SET status = 'checked_out', checked_out_at = now(), checked_out_by = $2 WHERE id = $1;

-- name: ListInHouse :many
SELECT st.id AS stay_id, COALESCE(f.id, 0)::bigint AS folio_id, st.room_id, rm.number AS room_number, rt.name AS room_type,
       st.guest_id, g.full_name AS guest_name, r.check_in, st.expected_out, r.adults, r.reservation_no,
       COALESCE((SELECT sum(i.amount_cents) FROM folio_items i WHERE i.folio_id = f.id), 0)::bigint AS items_cents,
       COALESCE(f.write_off_cents, 0)::bigint AS write_off_cents
FROM stays st
JOIN reservations r ON r.id = st.reservation_id
JOIN rooms rm ON rm.id = st.room_id
JOIN room_types rt ON rt.id = rm.room_type_id
JOIN guests g ON g.id = st.guest_id
LEFT JOIN guest_folios f ON f.stay_id = st.id
WHERE st.status = 'in_house'
ORDER BY length(rm.number), lower(rm.number);

-- name: InsertFolio :one
INSERT INTO guest_folios (folio_no, stay_id, guest_id) VALUES ($1, $2, $3) RETURNING id;

-- name: LockFolio :one
SELECT id, folio_no, stay_id, guest_id, status, write_off_cents FROM guest_folios WHERE id = $1 FOR UPDATE;

-- name: GetFolioHead :one
SELECT f.id, f.folio_no, f.status, f.write_off_cents, COALESCE(f.write_off_reason, '')::text AS write_off_reason, COALESCE(f.write_off_approved_by, 0)::bigint AS write_off_by,
       f.created_at, f.closed_at, f.guest_id, g.full_name AS guest_name, COALESCE(g.phone, '')::text AS guest_phone,
       COALESCE(st.id, 0)::bigint AS stay_id, COALESCE(st.status, '')::text AS stay_status, COALESCE(rm.id, 0)::bigint AS room_id, COALESCE(rm.number, '')::text AS room_number,
       r.check_in AS check_in, st.expected_out AS expected_out, COALESCE(r.id, 0)::bigint AS reservation_id, COALESCE(r.reservation_no, 0)::bigint AS reservation_no
FROM guest_folios f
JOIN guests g ON g.id = f.guest_id
LEFT JOIN stays st ON st.id = f.stay_id
LEFT JOIN reservations r ON r.id = st.reservation_id
LEFT JOIN rooms rm ON rm.id = st.room_id
WHERE f.id = $1;

-- name: ListFolioItems :many
SELECT i.id, i.kind, i.description, i.amount_cents, i.tax_cents, i.night_date, COALESCE(i.reason, '')::text AS reason, COALESCE(i.reverses_item_id, 0)::bigint AS reverses_item_id,
       COALESCE(i.sale_id, 0)::bigint AS sale_id, u.display_name AS posted_by_name, i.created_at,
       EXISTS (SELECT 1 FROM folio_items x WHERE x.reverses_item_id = i.id) AS reversed
FROM folio_items i JOIN users u ON u.id = i.posted_by
WHERE i.folio_id = $1 ORDER BY i.id;

-- name: GetFolioItem :one
SELECT id, folio_id, kind, description, amount_cents, tax_rate_bp, tax_cents FROM folio_items WHERE id = $1 AND folio_id = $2;

-- name: ItemReversed :one
SELECT EXISTS (SELECT 1 FROM folio_items WHERE reverses_item_id = $1);

-- name: FolioItemsTotal :one
SELECT COALESCE(sum(amount_cents), 0)::bigint FROM folio_items WHERE folio_id = $1;

-- name: InsertRoomNight :exec
INSERT INTO folio_items (folio_id, kind, description, amount_cents, tax_rate_bp, tax_cents, night_date, posted_by)
VALUES ($1, 'room_night', $2, $3, $4, $5, $6, $7);

-- name: InsertAdjustment :one
INSERT INTO folio_items (folio_id, kind, description, amount_cents, tax_rate_bp, tax_cents, reason, reverses_item_id, posted_by)
VALUES ($1, 'adjustment', $2, $3, $4, $5, $6, $7, $8) RETURNING id;

-- name: SetWriteOff :exec
UPDATE guest_folios SET write_off_cents = $2, write_off_approved_by = $3, write_off_reason = $4 WHERE id = $1;

-- name: CloseFolio :exec
UPDATE guest_folios SET status = 'closed', closed_at = now(), closed_by = $2 WHERE id = $1;

-- name: ReopenFolio :exec
UPDATE guest_folios SET status = 'open', closed_at = NULL, closed_by = NULL WHERE id = $1;

-- name: RoomBoard :many
SELECT r.id AS room_id, COALESCE(g.full_name, '')::text AS guest_name, COALESCE(st.id, 0)::bigint AS stay_id
FROM rooms r
LEFT JOIN stays st ON st.room_id = r.id AND st.status = 'in_house'
LEFT JOIN guests g ON g.id = st.guest_id;

-- name: FolioIDForStay :one
SELECT id FROM guest_folios WHERE stay_id = $1;

-- name: StayStatus :one
SELECT status FROM stays WHERE id = $1;

-- name: InsertPosCharge :one
INSERT INTO folio_items (folio_id, kind, description, amount_cents, tax_rate_bp, tax_cents, sale_id, posted_by)
VALUES ($1, 'pos_charge', $2, $3, 0, $4, $5, $6) RETURNING id;
