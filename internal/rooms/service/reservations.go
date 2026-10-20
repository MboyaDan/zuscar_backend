package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/platform/idempotency"
	"zuscar/backend/internal/rooms/repository"
)

// Reservations and availability (ASSUMPTIONS.md S-93 to S-96). Only confirmed and checked-in bookings hold a room;
// the database refuses a second one for the same night, so two people booking at once cannot both succeed (AC-07).

type ReservationView struct {
	ID               int64     `json:"id"`
	ReservationNo    int64     `json:"reservation_no"`
	GuestID          int64     `json:"guest_id"`
	GuestName        string    `json:"guest_name"`
	GuestPhone       string    `json:"guest_phone"`
	RoomID           int64     `json:"room_id"`
	RoomNumber       string    `json:"room_number"`
	RoomType         string    `json:"room_type"`
	Status           string    `json:"status"`
	CheckIn          string    `json:"check_in"`
	CheckOut         string    `json:"check_out"`
	Nights           int       `json:"nights"`
	Adults           int       `json:"adults"`
	NightlyRateCents int64     `json:"nightly_rate_cents"`
	TotalCents       int64     `json:"total_cents"` // nights x nightly rate, tax included (A-04)
	Notes            string    `json:"notes"`
	CancelledReason  string    `json:"cancelled_reason"`
	CreatedAt        time.Time `json:"created_at"`
	StayID           *int64    `json:"stay_id"`  // set once the guest has checked in
	FolioID          *int64    `json:"folio_id"` // the guest's account for that stay
}

type ReservationInput struct {
	GuestID          int64       `json:"guest_id"`
	Guest            *GuestInput `json:"guest"`
	RoomID           int64       `json:"room_id"`
	CheckIn          string      `json:"check_in"`
	CheckOut         string      `json:"check_out"`
	Adults           int         `json:"adults"`
	NightlyRateCents *int64      `json:"nightly_rate_cents"`
	Status           string      `json:"status"` // tentative or confirmed (default)
	Notes            string      `json:"notes"`
}

type ReservationPatch struct {
	GuestID          *int64  `json:"guest_id"`
	RoomID           *int64  `json:"room_id"`
	CheckIn          *string `json:"check_in"`
	CheckOut         *string `json:"check_out"`
	Adults           *int    `json:"adults"`
	NightlyRateCents *int64  `json:"nightly_rate_cents"`
	Notes            *string `json:"notes"`
}

type ReservationFilter struct {
	Status   string
	RoomID   int64
	GuestID  int64
	From, To string
	BeforeID int64
	Limit    int
}

func resView(r repository.ListReservationsRow) ReservationView {
	return ReservationView{ID: r.ID, ReservationNo: r.ReservationNo, GuestID: r.GuestID, GuestName: r.GuestName, GuestPhone: r.GuestPhone,
		RoomID: r.RoomID, RoomNumber: r.RoomNumber, RoomType: r.RoomType, Status: r.Status, CheckIn: dateStr(r.CheckIn), CheckOut: dateStr(r.CheckOut),
		Nights: int(r.Nights), Adults: int(r.Adults), NightlyRateCents: r.NightlyRateCents, TotalCents: int64(r.Nights) * r.NightlyRateCents,
		Notes: r.Notes, CancelledReason: r.CancelledReason, CreatedAt: r.CreatedAt.Time, StayID: idPtr(r.StayID), FolioID: idPtr(r.FolioID)}
}

func (s *Service) load(ctx context.Context, q *repository.Queries, id int64) (ReservationView, error) {
	rows, err := q.ListReservations(ctx, repository.ListReservationsParams{OnlyID: id, RowLimit: 1})
	if err != nil {
		return ReservationView{}, err
	}
	if len(rows) == 0 {
		return ReservationView{}, &NotFoundError{What: "reservation"}
	}
	return resView(rows[0]), nil
}

func (s *Service) GetReservation(ctx context.Context, actor *auth.Principal, id int64) (ReservationView, error) {
	if !can(actor) {
		return ReservationView{}, ErrForbidden
	}
	return s.load(ctx, repository.New(s.pool), id)
}

func (s *Service) ListReservations(ctx context.Context, actor *auth.Principal, f ReservationFilter) ([]ReservationView, error) {
	if !can(actor) {
		return nil, ErrForbidden
	}
	p := repository.ListReservationsParams{Status: f.Status, RoomID: f.RoomID, GuestID: f.GuestID, BeforeID: f.BeforeID, RowLimit: clampLimit(f.Limit)}
	if f.From != "" {
		t, err := parseDate(f.From, "from")
		if err != nil {
			return nil, err
		}
		p.WindowFrom = pgtype.Date{Time: t, Valid: true}
	}
	if f.To != "" {
		t, err := parseDate(f.To, "to")
		if err != nil {
			return nil, err
		}
		p.WindowTo = pgtype.Date{Time: t, Valid: true}
	}
	rows, err := repository.New(s.pool).ListReservations(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]ReservationView, 0, len(rows))
	for _, r := range rows {
		out = append(out, resView(r))
	}
	return out, nil
}

// stayWindow checks a pair of dates. New bookings may not start in the past; edits keep whatever start they had.
func stayWindow(in, out string, today time.Time, newBooking bool) (time.Time, time.Time, error) {
	ci, err := parseDate(in, "check_in")
	if err != nil {
		return ci, ci, err
	}
	co, err := parseDate(out, "check_out")
	if err != nil {
		return ci, co, err
	}
	if !co.After(ci) {
		return ci, co, &ValidationError{Field: "check_out", Msg: "must be after the arrival date"}
	}
	if int(co.Sub(ci).Hours()/24) > maxNights {
		return ci, co, &ValidationError{Field: "check_out", Msg: "a booking is at most 90 nights"}
	}
	if newBooking && ci.Before(today) {
		return ci, co, &ValidationError{Field: "check_in", Msg: "cannot be before today"}
	}
	return ci, co, nil
}

func overlapConflict(room string) error {
	return &ConflictError{Code: "room_unavailable", Field: "room_id", Msg: "Room " + room + " is already booked for some of those nights."}
}

// bookable returns the room's number and capacity when it can take a booking.
func bookable(ctx context.Context, q *repository.Queries, roomID int64) (repository.ListRoomsRow, error) {
	rows, err := q.ListRooms(ctx, repository.ListRoomsParams{OnlyID: roomID})
	if err != nil {
		return repository.ListRoomsRow{}, err
	}
	if len(rows) == 0 || !rows[0].Active || rows[0].PhysicalStatus == "out_of_service" {
		return repository.ListRoomsRow{}, &ValidationError{Field: "room_id", Msg: "must be a room that can take guests"}
	}
	return rows[0], nil
}

func checkAdults(adults int, room repository.ListRoomsRow) error {
	if adults < 1 || adults > int(room.Capacity) {
		return &ValidationError{Field: "adults", Msg: "must be from 1 to " + itoa(int64(room.Capacity)) + " for this room"}
	}
	return nil
}

func checkRate(rate int64) error {
	if rate < 0 || rate > maxRate {
		return &ValidationError{Field: "nightly_rate_cents", Msg: "must be from 0 to 10,000,000 cents"}
	}
	return nil
}

func (s *Service) CreateReservation(ctx context.Context, actor *auth.Principal, in ReservationInput, idem *idempotency.Key, m Meta) (ReservationView, *idempotency.Stored, error) {
	if !can(actor) {
		return ReservationView{}, nil, ErrForbidden
	}
	status := in.Status
	if status == "" {
		status = "confirmed"
	}
	if status != "tentative" && status != "confirmed" {
		return ReservationView{}, nil, &ValidationError{Field: "status", Msg: "must be tentative or confirmed"}
	}
	notes, err := optText(in.Notes, 500, "notes")
	if err != nil {
		return ReservationView{}, nil, err
	}
	if in.Adults == 0 {
		in.Adults = 1
	}
	if in.NightlyRateCents != nil {
		if err := checkRate(*in.NightlyRateCents); err != nil {
			return ReservationView{}, nil, err
		}
	}
	if (in.GuestID == 0) == (in.Guest == nil) {
		return ReservationView{}, nil, &ValidationError{Field: "guest_id", Msg: "give either an existing guest_id or a new guest, not both"}
	}
	if in.Guest != nil {
		if _, err := cleanGuestInput(*in.Guest); err != nil {
			return ReservationView{}, nil, err
		}
	}
	var out ReservationView
	var replay *idempotency.Stored
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if idem != nil {
			st, err := idempotency.Begin(ctx, tx, idem)
			if err != nil {
				return err
			}
			if st != nil {
				replay = st
				return nil
			}
		}
		q := repository.New(tx)
		today, err := s.today(ctx, q)
		if err != nil {
			return err
		}
		ci, co, err := stayWindow(in.CheckIn, in.CheckOut, today, true)
		if err != nil {
			return err
		}
		room, err := bookable(ctx, q, in.RoomID)
		if err != nil {
			return err
		}
		if err := checkAdults(in.Adults, room); err != nil {
			return err
		}
		rate := room.BaseRateCents
		if in.NightlyRateCents != nil {
			rate = *in.NightlyRateCents
		}
		guestID := in.GuestID
		if in.Guest != nil {
			if guestID, err = s.insertGuest(ctx, tx, actor, *in.Guest, m); err != nil {
				return err
			}
		} else if rows, err := q.ListGuests(ctx, repository.ListGuestsParams{OnlyID: guestID, RowLimit: 1}); err != nil {
			return err
		} else if len(rows) == 0 {
			return &ValidationError{Field: "guest_id", Msg: "does not exist"}
		}
		no, err := q.NextCounterValue(ctx, "reservation")
		if err != nil {
			return err
		}
		id, err := q.InsertReservation(ctx, repository.InsertReservationParams{ReservationNo: no, GuestID: guestID, RoomID: room.ID,
			CheckIn: pgtype.Date{Time: ci, Valid: true}, CheckOut: pgtype.Date{Time: co, Valid: true}, Status: status, Adults: int32(in.Adults),
			NightlyRateCents: rate, Notes: notes, CreatedBy: actor.UserID})
		if isOverlap(err) {
			return overlapConflict(room.Number)
		}
		if err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "rooms.reservation_created", "reservation", id, nil,
			map[string]any{"reservation_no": no, "room_id": room.ID, "guest_id": guestID, "check_in": in.CheckIn, "check_out": in.CheckOut, "status": status, "nightly_rate_cents": rate}); err != nil {
			return err
		}
		if out, err = s.load(ctx, q, id); err != nil {
			return err
		}
		if idem != nil {
			b, err := json.Marshal(map[string]any{"reservation": out})
			if err != nil {
				return err
			}
			return idempotency.Complete(ctx, tx, idem, 201, b)
		}
		return nil
	})
	return out, replay, err
}

// lock reads a reservation for update and refuses one that has ended.
func lock(ctx context.Context, q *repository.Queries, id int64) (repository.LockReservationRow, error) {
	r, err := q.LockReservation(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, &NotFoundError{What: "reservation"}
	}
	return r, err
}

func ended(status string) error {
	return &ConflictError{Code: "reservation_closed", Msg: "This booking is " + map[string]string{"cancelled": "cancelled", "no_show": "marked no-show", "checked_out": "checked out"}[status] + " and cannot change."}
}

func (s *Service) UpdateReservation(ctx context.Context, actor *auth.Principal, id int64, in ReservationPatch, m Meta) (ReservationView, error) {
	if !can(actor) {
		return ReservationView{}, ErrForbidden
	}
	var out ReservationView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := lock(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.Status != "tentative" && cur.Status != "confirmed" {
			if cur.Status == "checked_in" {
				return &ConflictError{Code: "reservation_in_house", Msg: "The guest is in the house. Stay changes are made from the stay."}
			}
			return ended(cur.Status)
		}
		guestID, roomID, adults, rate := cur.GuestID, cur.RoomID, int(cur.Adults), cur.NightlyRateCents
		ciS, coS, notesS := dateStr(cur.CheckIn), dateStr(cur.CheckOut), cur.Notes
		if in.GuestID != nil {
			guestID = *in.GuestID
		}
		if in.RoomID != nil {
			roomID = *in.RoomID
		}
		if in.CheckIn != nil {
			ciS = *in.CheckIn
		}
		if in.CheckOut != nil {
			coS = *in.CheckOut
		}
		if in.Adults != nil {
			adults = *in.Adults
		}
		if in.NightlyRateCents != nil {
			rate = *in.NightlyRateCents
		}
		if in.Notes != nil {
			notesS = *in.Notes
		}
		if err := checkRate(rate); err != nil {
			return err
		}
		notes, err := optText(notesS, 500, "notes")
		if err != nil {
			return err
		}
		today, err := s.today(ctx, q)
		if err != nil {
			return err
		}
		// A start that is not moving may already be in the past (a guest arriving today, late).
		ci, co, err := stayWindow(ciS, coS, today, ciS != dateStr(cur.CheckIn))
		if err != nil {
			return err
		}
		room, err := bookable(ctx, q, roomID)
		if err != nil && (roomID != cur.RoomID || in.Adults != nil) {
			return err
		} else if err != nil {
			rows, e2 := q.ListRooms(ctx, repository.ListRoomsParams{OnlyID: roomID})
			if e2 != nil || len(rows) == 0 {
				return err
			}
			room = rows[0]
		}
		if err := checkAdults(adults, room); err != nil {
			return err
		}
		if guestID != cur.GuestID {
			rows, err := q.ListGuests(ctx, repository.ListGuestsParams{OnlyID: guestID, RowLimit: 1})
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				return &ValidationError{Field: "guest_id", Msg: "does not exist"}
			}
		}
		err = q.UpdateReservation(ctx, repository.UpdateReservationParams{ID: id, GuestID: guestID, RoomID: roomID, CheckIn: pgtype.Date{Time: ci, Valid: true},
			CheckOut: pgtype.Date{Time: co, Valid: true}, Adults: int32(adults), NightlyRateCents: rate, Notes: notes})
		if isOverlap(err) {
			return overlapConflict(room.Number)
		}
		if err != nil {
			return err
		}
		if out, err = s.load(ctx, q, id); err != nil {
			return err
		}
		return s.record(ctx, tx, actor, m, "rooms.reservation_updated", "reservation", id,
			map[string]any{"room_id": cur.RoomID, "guest_id": cur.GuestID, "check_in": dateStr(cur.CheckIn), "check_out": dateStr(cur.CheckOut), "adults": cur.Adults, "nightly_rate_cents": cur.NightlyRateCents},
			map[string]any{"room_id": roomID, "guest_id": guestID, "check_in": ciS, "check_out": coS, "adults": adults, "nightly_rate_cents": rate})
	})
	return out, err
}

// Confirm turns a hold into a booking that blocks the room. It can fail with room_unavailable if someone else
// booked the room in the meantime.
func (s *Service) ConfirmReservation(ctx context.Context, actor *auth.Principal, id int64, m Meta) (ReservationView, error) {
	if !can(actor) {
		return ReservationView{}, ErrForbidden
	}
	var out ReservationView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := lock(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.Status != "tentative" {
			if cur.Status == "confirmed" {
				return &ConflictError{Code: "already_confirmed", Msg: "This booking is already confirmed."}
			}
			return &ConflictError{Code: "reservation_closed", Msg: "Only a held booking can be confirmed."}
		}
		today, err := s.today(ctx, q)
		if err != nil {
			return err
		}
		if !cur.CheckOut.Time.After(today) {
			return &ConflictError{Code: "reservation_past", Msg: "The stay is already over. Cancel this hold instead."}
		}
		room, err := bookable(ctx, q, cur.RoomID)
		if err != nil {
			return err
		}
		err = q.ConfirmReservation(ctx, id)
		if isOverlap(err) {
			return overlapConflict(room.Number)
		}
		if err != nil {
			return err
		}
		if out, err = s.load(ctx, q, id); err != nil {
			return err
		}
		return s.record(ctx, tx, actor, m, "rooms.reservation_confirmed", "reservation", id, map[string]any{"status": "tentative"}, map[string]any{"status": "confirmed"})
	})
	return out, err
}

func (s *Service) CancelReservation(ctx context.Context, actor *auth.Principal, id int64, reason string, m Meta) (ReservationView, error) {
	if !can(actor) {
		return ReservationView{}, ErrForbidden
	}
	why, err := required(reason, "reason", 200)
	if err != nil {
		return ReservationView{}, err
	}
	var out ReservationView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := lock(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.Status == "checked_in" {
			return &ConflictError{Code: "reservation_in_house", Msg: "The guest is in the house. Check them out instead."}
		}
		if cur.Status != "tentative" && cur.Status != "confirmed" {
			return ended(cur.Status)
		}
		if err := q.CancelReservation(ctx, repository.CancelReservationParams{ID: id, CancelledReason: pgtype.Text{String: why, Valid: true}, CancelledBy: pgtype.Int8{Int64: actor.UserID, Valid: true}}); err != nil {
			return err
		}
		if out, err = s.load(ctx, q, id); err != nil {
			return err
		}
		return s.record(ctx, tx, actor, m, "rooms.reservation_cancelled", "reservation", id, map[string]any{"status": cur.Status}, map[string]any{"status": "cancelled", "reason": why})
	})
	return out, err
}

// NoShow records that a confirmed guest never came. It is allowed from the arrival date on, never before.
func (s *Service) NoShow(ctx context.Context, actor *auth.Principal, id int64, m Meta) (ReservationView, error) {
	if !can(actor) {
		return ReservationView{}, ErrForbidden
	}
	var out ReservationView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := lock(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.Status != "confirmed" {
			return &ConflictError{Code: "reservation_closed", Msg: "Only a confirmed booking can be marked no-show."}
		}
		today, err := s.today(ctx, q)
		if err != nil {
			return err
		}
		if cur.CheckIn.Time.After(today) {
			return &ConflictError{Code: "too_early", Field: "check_in", Msg: "The guest is not due until " + dateStr(cur.CheckIn) + "."}
		}
		if err := q.MarkNoShow(ctx, id); err != nil {
			return err
		}
		if out, err = s.load(ctx, q, id); err != nil {
			return err
		}
		return s.record(ctx, tx, actor, m, "rooms.reservation_no_show", "reservation", id, map[string]any{"status": "confirmed"}, map[string]any{"status": "no_show"})
	})
	return out, err
}

// Availability

type AvailabilityRoom struct {
	RoomID         int64  `json:"room_id"`
	Number         string `json:"number"`
	Floor          *int   `json:"floor"`
	RoomTypeID     int64  `json:"room_type_id"`
	RoomType       string `json:"room_type"`
	RateCents      int64  `json:"base_rate_cents"`
	Capacity       int    `json:"capacity"`
	Available      bool   `json:"available"`
	Holds          int    `json:"holds"` // tentative bookings that overlap; they do not block
	BlockedBy      *int64 `json:"blocking_reservation_no"`
	BlockedFrom    string `json:"blocked_from,omitempty"`
	BlockedUntil   string `json:"blocked_until,omitempty"`
	BlockingStatus string `json:"blocking_status,omitempty"`
}

// Availability is a guide for the front desk. Booking itself is decided by the database constraint.
func (s *Service) Availability(ctx context.Context, actor *auth.Principal, from, to string, typeID int64) ([]AvailabilityRoom, error) {
	if !can(actor) {
		return nil, ErrForbidden
	}
	ci, co, err := stayWindow(from, to, time.Time{}, false)
	if err != nil {
		var v *ValidationError
		if errors.As(err, &v) {
			if v.Field == "check_in" {
				v.Field = "from"
			} else {
				v.Field = "to"
			}
		}
		return nil, err
	}
	rows, err := repository.New(s.pool).Availability(ctx, repository.AvailabilityParams{StayFrom: pgtype.Date{Time: ci, Valid: true}, StayTo: pgtype.Date{Time: co, Valid: true}, RoomTypeID: typeID})
	if err != nil {
		return nil, err
	}
	out := make([]AvailabilityRoom, 0, len(rows))
	for _, r := range rows {
		a := AvailabilityRoom{RoomID: r.ID, Number: r.Number, Floor: int4Ptr(r.Floor), RoomTypeID: r.RoomTypeID, RoomType: r.RoomType, RateCents: r.BaseRateCents,
			Capacity: int(r.Capacity), Available: r.BlockingID == 0, Holds: int(r.Holds)}
		if r.BlockingID != 0 {
			no := r.BlockingNo
			a.BlockedBy, a.BlockedFrom, a.BlockedUntil, a.BlockingStatus = &no, dateStr(r.BlockingCheckIn), dateStr(r.BlockingCheckOut), r.BlockingStatus
		}
		out = append(out, a)
	}
	return out, nil
}

func idPtr(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}
