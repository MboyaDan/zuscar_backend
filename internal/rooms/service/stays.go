package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/platform/idempotency"
	"zuscar/backend/internal/rooms/repository"
)

// Check-in, the stay, and room status (ASSUMPTIONS.md S-97 to S-102). Every booked night is posted to the guest's folio
// at check-in at the booking's rate; extending a stay posts the added nights; nothing is credited automatically.

func canFolios(a *auth.Principal) bool { return a != nil && a.Has("folios.manage") }

// taxPart is the tax inside a tax-inclusive amount: round half up of amount x rate / (10000 + rate) (S-48).
func taxPart(amount int64, rateBP int) int64 {
	if amount <= 0 || rateBP <= 0 {
		return 0
	}
	den := int64(10000 + rateBP)
	return (amount*int64(rateBP)*2 + den) / (2 * den)
}

func nightText(room string, d time.Time) string {
	return fmt.Sprintf("Room %s, night of %s", room, d.Format("Mon 2 Jan 2006"))
}

func roomNotReady(number, status string) error {
	why := map[string]string{
		"occupied":       "has a guest in it",
		"dirty":          "is dirty and needs cleaning",
		"cleaning":       "is being cleaned",
		"out_of_service": "is out of service",
	}[status]
	if why == "" {
		why = "is not ready (" + status + ")"
	}
	return &ConflictError{Code: "room_not_ready", Field: "room_id", Msg: "Room " + number + " " + why + "."}
}

// setStatus moves a room's physical status and writes the history row. The caller holds the room lock.
func (s *Service) setStatus(ctx context.Context, tx pgx.Tx, q *repository.Queries, actor *auth.Principal, roomID int64, from, to, reason string) error {
	if from == to {
		return nil
	}
	if err := q.SetRoomStatus(ctx, repository.SetRoomStatusParams{ID: roomID, PhysicalStatus: to}); err != nil {
		return err
	}
	txt := pgtype.Text{String: reason, Valid: reason != ""}
	return q.InsertRoomStatusHistory(ctx, repository.InsertRoomStatusHistoryParams{RoomID: roomID, FromStatus: from, ToStatus: to, ChangedBy: actor.UserID, Reason: txt})
}

func (s *Service) postNights(ctx context.Context, q *repository.Queries, actor *auth.Principal, folioID int64, room string, rateBP int, rate int64, from, to time.Time) error {
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		err := q.InsertRoomNight(ctx, repository.InsertRoomNightParams{FolioID: folioID, Description: nightText(room, d), AmountCents: rate, TaxRateBp: int32(rateBP),
			TaxCents: taxPart(rate, rateBP), NightDate: pgtype.Date{Time: d, Valid: true}, PostedBy: actor.UserID})
		if err != nil {
			return err
		}
	}
	return nil
}

type CheckInResult struct {
	Reservation ReservationView `json:"reservation"`
	StayID      int64           `json:"stay_id"`
	FolioID     int64           `json:"folio_id"`
}

// CheckIn turns a confirmed booking into a stay: the room becomes occupied, a folio is opened and every booked night
// is posted to it. The room must be clean and ready. Guests arriving before the booked date need the booking moved first.
func (s *Service) CheckIn(ctx context.Context, actor *auth.Principal, reservationID int64, idem *idempotency.Key, m Meta) (CheckInResult, *idempotency.Stored, error) {
	if !can(actor) {
		return CheckInResult{}, nil, ErrForbidden
	}
	var out CheckInResult
	var replay *idempotency.Stored
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
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
		cur, err := lock(ctx, q, reservationID)
		if err != nil {
			return err
		}
		switch cur.Status {
		case "confirmed":
		case "tentative":
			return &ConflictError{Code: "not_confirmed", Msg: "This booking is only on hold. Confirm it before checking the guest in."}
		case "checked_in":
			return &ConflictError{Code: "already_checked_in", Msg: "This guest is already checked in."}
		default:
			return ended(cur.Status)
		}
		today, err := s.today(ctx, q)
		if err != nil {
			return err
		}
		if cur.CheckIn.Time.After(today) {
			return &ConflictError{Code: "too_early", Msg: "The guest is not due until " + dateStr(cur.CheckIn) + ". Change the booking dates first to check in earlier."}
		}
		if !cur.CheckOut.Time.After(today) {
			return &ConflictError{Code: "reservation_past", Msg: "The booked nights are over. Cancel or change the booking."}
		}
		room, err := q.LockRoom(ctx, cur.RoomID)
		if err != nil {
			return err
		}
		if room.PhysicalStatus != "vacant_ready" && room.PhysicalStatus != "reserved" {
			return roomNotReady(room.Number, room.PhysicalStatus)
		}
		rateBP, err := q.RoomTaxRate(ctx, room.ID)
		if err != nil {
			return err
		}
		stayID, err := q.InsertStay(ctx, repository.InsertStayParams{ReservationID: cur.ID, RoomID: room.ID, GuestID: cur.GuestID, ExpectedOut: cur.CheckOut, CheckedInBy: actor.UserID})
		if err != nil {
			return err
		}
		no, err := q.NextCounterValue(ctx, "folio")
		if err != nil {
			return err
		}
		folioID, err := q.InsertFolio(ctx, repository.InsertFolioParams{FolioNo: no, StayID: pgtype.Int8{Int64: stayID, Valid: true}, GuestID: cur.GuestID})
		if err != nil {
			return err
		}
		if err := s.postNights(ctx, q, actor, folioID, room.Number, int(rateBP), cur.NightlyRateCents, cur.CheckIn.Time, cur.CheckOut.Time); err != nil {
			return err
		}
		if err := q.CheckInReservation(ctx, cur.ID); err != nil {
			return err
		}
		if err := s.setStatus(ctx, tx, q, actor, room.ID, room.PhysicalStatus, "occupied", "Check-in, booking #"+itoa(cur.ReservationNo)); err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "rooms.checked_in", "stay", stayID, map[string]any{"status": "confirmed"},
			map[string]any{"reservation_id": cur.ID, "room_id": room.ID, "folio_id": folioID, "folio_no": no, "nights": int(cur.CheckOut.Time.Sub(cur.CheckIn.Time).Hours() / 24)}); err != nil {
			return err
		}
		rv, err := s.load(ctx, q, cur.ID)
		if err != nil {
			return err
		}
		out = CheckInResult{Reservation: rv, StayID: stayID, FolioID: folioID}
		if idem != nil {
			b, err := json.Marshal(out)
			if err != nil {
				return err
			}
			return idempotency.Complete(ctx, tx, idem, 201, b)
		}
		return nil
	})
	return out, replay, err
}

type StayChange struct {
	Reservation ReservationView `json:"reservation"`
	StayID      int64           `json:"stay_id"`
	FolioID     int64           `json:"folio_id"`
}

// lockStay locks a stay that is still in the house, then its reservation and folio, in that order everywhere.
func lockStay(ctx context.Context, q *repository.Queries, id int64) (repository.LockStayRow, repository.LockReservationRow, repository.LockFolioRow, error) {
	var none1 repository.LockReservationRow
	var none2 repository.LockFolioRow
	st, err := q.LockStay(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, none1, none2, &NotFoundError{What: "stay"}
	}
	if err != nil {
		return st, none1, none2, err
	}
	if st.Status != "in_house" {
		return st, none1, none2, &ConflictError{Code: "stay_ended", Msg: "This guest has already checked out."}
	}
	res, err := q.LockReservation(ctx, st.ReservationID)
	if err != nil {
		return st, res, none2, err
	}
	fid, err := q.FolioIDForStay(ctx, pgtype.Int8{Int64: id, Valid: true})
	if err != nil {
		return st, res, none2, err
	}
	fol, err := q.LockFolio(ctx, fid)
	return st, res, fol, err
}

// ExtendStay moves the leaving date later and posts the added nights. The extra nights are held to the same overlap
// rule as any booking, so a room that is booked after the guest cannot be extended into.
func (s *Service) ExtendStay(ctx context.Context, actor *auth.Principal, stayID int64, newOut string, m Meta) (StayChange, error) {
	if !can(actor) {
		return StayChange{}, ErrForbidden
	}
	var out StayChange
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		st, res, fol, err := lockStay(ctx, q, stayID)
		if err != nil {
			return err
		}
		co, err := parseDate(newOut, "check_out")
		if err != nil {
			return err
		}
		if !co.After(res.CheckOut.Time) {
			return &ValidationError{Field: "check_out", Msg: "must be after the current leaving date (" + dateStr(res.CheckOut) + ")"}
		}
		if int(co.Sub(res.CheckIn.Time).Hours()/24) > maxNights {
			return &ValidationError{Field: "check_out", Msg: "a stay is at most 90 nights"}
		}
		if fol.Status != "open" {
			return &ConflictError{Code: "folio_closed", Msg: "The folio is closed. Reopen it first."}
		}
		room, err := q.LockRoom(ctx, st.RoomID)
		if err != nil {
			return err
		}
		if err := q.ExtendReservation(ctx, repository.ExtendReservationParams{ID: res.ID, CheckOut: pgtype.Date{Time: co, Valid: true}}); isOverlap(err) {
			return &ConflictError{Code: "room_unavailable", Field: "check_out", Msg: "Room " + room.Number + " is booked by someone else after " + dateStr(res.CheckOut) + ", so the stay cannot be extended."}
		} else if err != nil {
			return err
		}
		if err := q.SetStayExpectedOut(ctx, repository.SetStayExpectedOutParams{ID: stayID, ExpectedOut: pgtype.Date{Time: co, Valid: true}}); err != nil {
			return err
		}
		rateBP, err := q.RoomTaxRate(ctx, room.ID)
		if err != nil {
			return err
		}
		if err := s.postNights(ctx, q, actor, fol.ID, room.Number, int(rateBP), res.NightlyRateCents, res.CheckOut.Time, co); err != nil {
			return err
		}
		rv, err := s.load(ctx, q, res.ID)
		if err != nil {
			return err
		}
		out = StayChange{Reservation: rv, StayID: stayID, FolioID: fol.ID}
		return s.record(ctx, tx, actor, m, "rooms.stay_extended", "stay", stayID, map[string]any{"check_out": dateStr(res.CheckOut)}, map[string]any{"check_out": newOut})
	})
	return out, err
}

// ChangeRoom moves a guest in the house to another clean room. The booked nights and the rate stay as agreed; the old
// room goes to dirty and the new one to occupied, both with a history row.
func (s *Service) ChangeRoom(ctx context.Context, actor *auth.Principal, stayID, roomID int64, reason string, m Meta) (StayChange, error) {
	if !can(actor) {
		return StayChange{}, ErrForbidden
	}
	why, err := required(reason, "reason", 200)
	if err != nil {
		return StayChange{}, err
	}
	var out StayChange
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		st, res, fol, err := lockStay(ctx, q, stayID)
		if err != nil {
			return err
		}
		if roomID == st.RoomID {
			return &ValidationError{Field: "room_id", Msg: "is the room the guest is already in"}
		}
		// Rooms are locked in id order, so two moves crossing each other cannot deadlock.
		first, second := st.RoomID, roomID
		if second < first {
			first, second = second, first
		}
		locked := map[int64]repository.LockRoomRow{}
		for _, id := range []int64{first, second} {
			r, err := q.LockRoom(ctx, id)
			if errors.Is(err, pgx.ErrNoRows) {
				return &ValidationError{Field: "room_id", Msg: "does not exist"}
			}
			if err != nil {
				return err
			}
			locked[id] = r
		}
		oldRoom, newRoom := locked[st.RoomID], locked[roomID]
		if !newRoom.Active {
			return &ValidationError{Field: "room_id", Msg: "must be a room that can take guests"}
		}
		if newRoom.PhysicalStatus != "vacant_ready" && newRoom.PhysicalStatus != "reserved" {
			return roomNotReady(newRoom.Number, newRoom.PhysicalStatus)
		}
		rooms, err := q.ListRooms(ctx, repository.ListRoomsParams{OnlyID: roomID})
		if err != nil {
			return err
		}
		if len(rooms) == 1 {
			if err := checkAdults(int(res.Adults), rooms[0]); err != nil {
				return err
			}
		}
		if err := q.MoveReservationRoom(ctx, repository.MoveReservationRoomParams{ID: res.ID, RoomID: roomID}); isOverlap(err) {
			return &ConflictError{Code: "room_unavailable", Field: "room_id", Msg: "Room " + newRoom.Number + " is booked by someone else during this stay."}
		} else if err != nil {
			return err
		}
		if err := q.MoveStayRoom(ctx, repository.MoveStayRoomParams{ID: stayID, RoomID: roomID}); err != nil {
			return err
		}
		if err := s.setStatus(ctx, tx, q, actor, oldRoom.ID, oldRoom.PhysicalStatus, "dirty", "Guest moved to room "+newRoom.Number+": "+why); err != nil {
			return err
		}
		if err := s.setStatus(ctx, tx, q, actor, newRoom.ID, newRoom.PhysicalStatus, "occupied", "Guest moved from room "+oldRoom.Number+": "+why); err != nil {
			return err
		}
		rv, err := s.load(ctx, q, res.ID)
		if err != nil {
			return err
		}
		out = StayChange{Reservation: rv, StayID: stayID, FolioID: fol.ID}
		return s.record(ctx, tx, actor, m, "rooms.room_changed", "stay", stayID, map[string]any{"room_id": oldRoom.ID}, map[string]any{"room_id": newRoom.ID, "reason": why})
	})
	return out, err
}

// CheckOut closes the folio and the stay in one step. It needs a settled folio: every confirmed payment and any
// approved write-off add up to the charges, and no payment is still waiting for confirmation.
func (s *Service) CheckOut(ctx context.Context, actor *auth.Principal, stayID int64, idem *idempotency.Key, m Meta) (FolioView, *idempotency.Stored, error) {
	if !canFolios(actor) {
		return FolioView{}, nil, ErrForbidden
	}
	var out FolioView
	var replay *idempotency.Stored
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
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
		st, res, fol, err := lockStay(ctx, q, stayID)
		if err != nil {
			return err
		}
		if err := s.settled(ctx, tx, q, fol); err != nil {
			return err
		}
		room, err := q.LockRoom(ctx, st.RoomID)
		if err != nil {
			return err
		}
		if err := q.CloseFolio(ctx, repository.CloseFolioParams{ID: fol.ID, ClosedBy: pgtype.Int8{Int64: actor.UserID, Valid: true}}); err != nil {
			return err
		}
		if err := q.EndStay(ctx, repository.EndStayParams{ID: stayID, CheckedOutBy: pgtype.Int8{Int64: actor.UserID, Valid: true}}); err != nil {
			return err
		}
		if err := q.CheckOutReservation(ctx, res.ID); err != nil {
			return err
		}
		if err := s.setStatus(ctx, tx, q, actor, room.ID, room.PhysicalStatus, "dirty", "Check-out, booking #"+itoa(res.ReservationNo)); err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "rooms.checked_out", "stay", stayID, map[string]any{"status": "in_house"}, map[string]any{"folio_id": fol.ID, "room_id": room.ID}); err != nil {
			return err
		}
		out, err = s.folioView(ctx, tx, q, fol.ID)
		if err != nil {
			return err
		}
		if idem != nil {
			b, err := json.Marshal(map[string]any{"folio": out})
			if err != nil {
				return err
			}
			return idempotency.Complete(ctx, tx, idem, 200, b)
		}
		return nil
	})
	return out, replay, err
}

// ---- who is in the house ----

type InHouseView struct {
	StayID        int64  `json:"stay_id"`
	FolioID       *int64 `json:"folio_id,omitempty"`
	RoomID        int64  `json:"room_id"`
	RoomNumber    string `json:"room_number"`
	RoomType      string `json:"room_type"`
	GuestID       int64  `json:"guest_id"`
	GuestName     string `json:"guest_name"`
	ReservationNo int64  `json:"reservation_no"`
	CheckIn       string `json:"check_in"`
	ExpectedOut   string `json:"expected_out"`
	Adults        int    `json:"adults"`
	BalanceCents  *int64 `json:"balance_cents,omitempty"` // only for people who may see folios
}

// InHouse lists the guests in the house. Anyone with rooms.view sees room and guest; the folio and the balance need
// folios.view, so a waiter picking a room to charge sees who is where and nothing about the account.
func (s *Service) InHouse(ctx context.Context, actor *auth.Principal) ([]InHouseView, error) {
	if actor == nil || !actor.Has("rooms.view") {
		return nil, ErrForbidden
	}
	showFolio := actor.Has("folios.view")
	var out []InHouseView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		rows, err := q.ListInHouse(ctx)
		if err != nil {
			return err
		}
		out = make([]InHouseView, 0, len(rows))
		for _, r := range rows {
			v := InHouseView{StayID: r.StayID, RoomID: r.RoomID, RoomNumber: r.RoomNumber, RoomType: r.RoomType, GuestID: r.GuestID, GuestName: r.GuestName,
				ReservationNo: r.ReservationNo, CheckIn: dateStr(r.CheckIn), ExpectedOut: dateStr(r.ExpectedOut), Adults: int(r.Adults)}
			if showFolio && r.FolioID != 0 {
				v.FolioID = idPtr(r.FolioID)
				paid, _, err := s.pay.FolioPaymentTotals(ctx, tx, r.FolioID)
				if err != nil {
					return err
				}
				bal := r.ItemsCents - paid - r.WriteOffCents
				v.BalanceCents = &bal
			}
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

// ---- room status ----

// manualMoves are the changes staff make by hand. Occupied is set only by check-in and a guest move, and dirty after
// check-out; reserved is not used (S-101).
var manualMoves = map[string]map[string]bool{
	"vacant_ready":   {"dirty": true, "out_of_service": true},
	"dirty":          {"cleaning": true, "out_of_service": true},
	"cleaning":       {"vacant_ready": true, "dirty": true, "out_of_service": true},
	"reserved":       {"vacant_ready": true, "dirty": true, "out_of_service": true},
	"out_of_service": {"dirty": true},
}

func (s *Service) SetRoomStatus(ctx context.Context, actor *auth.Principal, roomID int64, to, reason string, m Meta) (RoomView, error) {
	if !can(actor) {
		return RoomView{}, ErrForbidden
	}
	rs, err := optText(reason, 200, "reason")
	if err != nil {
		return RoomView{}, err
	}
	if to == "out_of_service" && !rs.Valid {
		return RoomView{}, &ValidationError{Field: "reason", Msg: "is required to take a room out of service"}
	}
	var out RoomView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		room, err := q.LockRoom(ctx, roomID)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "room"}
		}
		if err != nil {
			return err
		}
		if room.PhysicalStatus == "occupied" {
			return &ConflictError{Code: "room_occupied", Msg: "Room " + room.Number + " has a guest in it. Check the guest out first."}
		}
		if !manualMoves[room.PhysicalStatus][to] {
			return &ConflictError{Code: "invalid_transition", Field: "status", Msg: "A room that is " + room.PhysicalStatus + " cannot be set to " + to + "."}
		}
		if err := s.setStatus(ctx, tx, q, actor, roomID, room.PhysicalStatus, to, rs.String); err != nil {
			return err
		}
		rows, err := q.ListRooms(ctx, repository.ListRoomsParams{OnlyID: roomID})
		if err != nil {
			return err
		}
		out = roomView(rows[0])
		return s.record(ctx, tx, actor, m, "rooms.status_changed", "room", roomID, map[string]any{"status": room.PhysicalStatus}, map[string]any{"status": to, "reason": rs.String})
	})
	return out, err
}
