package handler_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Stage 22: room types, rooms, guests, availability and reservations (AC-07; ASSUMPTIONS.md S-93 to S-96).

type roomsEnv struct {
	t      *testing.T
	e      *testEnv
	mgr    jar
	recep  jar
	cash   jar
	tax    int64
	types  map[string]int64
	rooms  map[string]int64
	guest  int64
	nextNo int
}

type resOut struct {
	Reservation struct {
		ID               int64  `json:"id"`
		ReservationNo    int64  `json:"reservation_no"`
		Status           string `json:"status"`
		CheckIn          string `json:"check_in"`
		CheckOut         string `json:"check_out"`
		Nights           int    `json:"nights"`
		NightlyRateCents int64  `json:"nightly_rate_cents"`
		TotalCents       int64  `json:"total_cents"`
		RoomNumber       string `json:"room_number"`
		GuestName        string `json:"guest_name"`
	} `json:"reservation"`
}

func newRoomsEnv(t *testing.T) *roomsEnv {
	t.Helper()
	e := newEnv(t)
	e.addUser("mgr", false, "business_manager")
	e.addUser("recep", false, "receptionist")
	e.addUser("cash", false, "cashier")
	r := &roomsEnv{t: t, e: e, mgr: e.mustLogin("mgr"), recep: e.mustLogin("recep"), cash: e.mustLogin("cash"), types: map[string]int64{}, rooms: map[string]int64{}}
	for _, x := range e.taxRates(r.mgr) {
		if x.Treatment == "standard" {
			r.tax = x.ID
		}
	}
	r.guest = decodeInto[struct {
		Guest struct {
			ID int64 `json:"id"`
		} `json:"guest"`
	}](t, r.call(r.recep, "POST", "/api/v1/guests", map[string]any{"full_name": "Amina Otieno", "phone": "+254700111222"}, 201, "")).Guest.ID
	return r
}

func (r *roomsEnv) call(j jar, method, path string, body any, want int, errCode string) *httptest.ResponseRecorder {
	r.t.Helper()
	w := r.e.do(req{method: method, path: path, jar: j, body: body})
	expect(r.t, w, want, errCode)
	return w
}

func (r *roomsEnv) day(n int) string {
	var d time.Time
	if err := r.e.pool.QueryRow(context.Background(), `SELECT (now() AT TIME ZONE 'Africa/Nairobi')::date`).Scan(&d); err != nil {
		r.t.Fatal(err)
	}
	return d.AddDate(0, 0, n).Format("2006-01-02")
}

func (r *roomsEnv) roomType(name string, rate int64, capacity int) int64 {
	r.t.Helper()
	id := decodeInto[struct {
		RoomType struct {
			ID int64 `json:"id"`
		} `json:"room_type"`
	}](r.t, r.call(r.mgr, "POST", "/api/v1/room-types", map[string]any{"name": name, "base_rate_cents": rate, "capacity": capacity, "tax_rate_id": r.tax}, 201, "")).RoomType.ID
	r.types[name] = id
	return id
}

func (r *roomsEnv) room(number string, typeID int64) int64 {
	r.t.Helper()
	id := decodeInto[struct {
		Room struct {
			ID int64 `json:"id"`
		} `json:"room"`
	}](r.t, r.call(r.mgr, "POST", "/api/v1/rooms", map[string]any{"number": number, "room_type_id": typeID, "floor": 1}, 201, "")).Room.ID
	r.rooms[number] = id
	return id
}

func (r *roomsEnv) book(room int64, in, out, status string, want int, errCode string) resOut {
	r.t.Helper()
	body := map[string]any{"guest_id": r.guest, "room_id": room, "check_in": in, "check_out": out, "adults": 1}
	if status != "" {
		body["status"] = status
	}
	w := r.call(r.recep, "POST", "/api/v1/reservations", body, want, errCode)
	if want >= 400 {
		return resOut{}
	}
	return decodeInto[resOut](r.t, w)
}

func (r *roomsEnv) act(id int64, action string, body any, want int, errCode string) resOut {
	r.t.Helper()
	w := r.call(r.recep, "POST", "/api/v1/reservations/"+itoa(id)+"/"+action, body, want, errCode)
	if want >= 400 {
		return resOut{}
	}
	return decodeInto[resOut](r.t, w)
}

func TestAC07_ThirteenthAndFourteenthRoomViaAPI(t *testing.T) {
	r := newRoomsEnv(t)
	std := r.roomType("Standard", 650000, 2)
	dlx := r.roomType("Deluxe", 900000, 3)
	ste := r.roomType("Suite", 1500000, 4)
	for i := 1; i <= 13; i++ {
		typ := std
		if i > 6 {
			typ = dlx
		}
		if i > 11 {
			typ = ste
		}
		r.room(fmt.Sprintf("%d", 100+i), typ)
	}
	// The fourteenth room is data, not a code change, and takes bookings like any other.
	fourteen := r.room("114", ste)
	list := decodeInto[struct {
		Rooms []struct {
			Number string `json:"number"`
			Active bool   `json:"active"`
		} `json:"rooms"`
	}](t, r.call(r.recep, "GET", "/api/v1/rooms", nil, 200, ""))
	if len(list.Rooms) != 14 || list.Rooms[0].Number != "101" || list.Rooms[13].Number != "114" {
		t.Fatalf("rooms listed: %+v", list.Rooms)
	}
	b := r.book(fourteen, r.day(1), r.day(3), "", 201, "")
	if b.Reservation.Status != "confirmed" || b.Reservation.Nights != 2 || b.Reservation.NightlyRateCents != 1500000 || b.Reservation.TotalCents != 3000000 {
		t.Fatalf("booking: %+v", b.Reservation)
	}
	// A duplicate number is refused whatever its case; a room cannot be removed.
	r.call(r.mgr, "POST", "/api/v1/rooms", map[string]any{"number": "114", "room_type_id": std}, 409, "duplicate_number")
	r.call(r.mgr, "POST", "/api/v1/room-types", map[string]any{"name": "standard", "base_rate_cents": 1, "capacity": 1, "tax_rate_id": r.tax}, 409, "duplicate_name")
	r.e.exec(`UPDATE rooms SET active = true WHERE id = $1`, fourteen)
	var pgErr error
	_, pgErr = r.e.pool.Exec(context.Background(), `DELETE FROM rooms WHERE id = $1`, fourteen)
	if pgErr == nil {
		t.Fatal("a room could be deleted")
	}
}

func TestAC07_ConcurrentBookingsOnlyOneWins(t *testing.T) {
	r := newRoomsEnv(t)
	room := r.room("201", r.roomType("Standard", 650000, 2))
	const n = 8
	codes := make([]int, n)
	errs := make([]string, n)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := r.e.do(req{method: "POST", path: "/api/v1/reservations", jar: r.recep, header: map[string]string{"Idempotency-Key": newKey()},
				body: map[string]any{"guest_id": r.guest, "room_id": room, "check_in": r.day(2), "check_out": r.day(4), "adults": 1}})
			codes[i] = w.Code
			if w.Code != 201 {
				errs[i] = code(t, w)
			}
		}()
	}
	wg.Wait()
	won := 0
	for i, c := range codes {
		switch c {
		case 201:
			won++
		case 409:
			if errs[i] != "room_unavailable" {
				t.Fatalf("a losing booking answered %q, want room_unavailable", errs[i])
			}
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if won != 1 {
		t.Fatalf("%d bookings won the same room and nights, want exactly 1", won)
	}
	if r.e.count(`SELECT count(*) FROM reservations WHERE room_id = $1`, room) != 1 {
		t.Fatal("a losing booking left a row behind")
	}
	// Losers give their numbers back: the next booking is number 2, with no gap.
	next := r.book(room, r.day(10), r.day(11), "", 201, "")
	if next.Reservation.ReservationNo != 2 {
		t.Fatalf("next reservation number = %d, want 2", next.Reservation.ReservationNo)
	}
}

func TestAC07_BackToBackAllowed(t *testing.T) {
	r := newRoomsEnv(t)
	room := r.room("202", r.roomType("Standard", 650000, 2))
	r.book(room, r.day(1), r.day(3), "", 201, "")
	r.book(room, r.day(3), r.day(5), "", 201, "") // arrives the day the other leaves
	r.book(room, r.day(0), r.day(1), "", 201, "") // leaves the day the first arrives
	r.book(room, r.day(2), r.day(4), "", 409, "room_unavailable")
	r.book(room, r.day(0), r.day(2), "", 409, "room_unavailable")
	r.book(room, r.day(4), r.day(6), "", 409, "room_unavailable")
	r.book(room, r.day(5), r.day(6), "", 201, "")
	// The database refuses the same thing without the API's help.
	_, err := r.e.pool.Exec(context.Background(), `INSERT INTO reservations (reservation_no, guest_id, room_id, check_in, check_out, status, nightly_rate_cents, created_by)
		VALUES (9001, $1, $2, $3::date, $4::date, 'confirmed', 100, (SELECT id FROM users LIMIT 1))`, r.guest, room, r.day(1), r.day(2))
	if err == nil {
		t.Fatal("the database accepted an overlapping booking")
	}
}

func TestAC07_TentativeDoesNotBlock(t *testing.T) {
	r := newRoomsEnv(t)
	room := r.room("203", r.roomType("Standard", 650000, 2))
	a := r.book(room, r.day(1), r.day(3), "tentative", 201, "")
	b := r.book(room, r.day(2), r.day(4), "tentative", 201, "") // holds do not block each other
	avail := func() (bool, int) {
		o := decodeInto[struct {
			Rooms []struct {
				Available bool `json:"available"`
				Holds     int  `json:"holds"`
			} `json:"rooms"`
		}](t, r.call(r.recep, "GET", "/api/v1/availability?from="+r.day(1)+"&to="+r.day(3), nil, 200, ""))
		return o.Rooms[0].Available, o.Rooms[0].Holds
	}
	if ok, holds := avail(); !ok || holds != 2 {
		t.Fatalf("with two holds: available %v holds %d, want true and 2", ok, holds)
	}
	r.act(a.Reservation.ID, "confirm", nil, 200, "")
	if ok, _ := avail(); ok {
		t.Fatal("a confirmed booking does not block the room")
	}
	r.act(b.Reservation.ID, "confirm", nil, 409, "room_unavailable") // someone got there first
	// Cancelling frees the room and the second hold can now be confirmed.
	r.act(a.Reservation.ID, "cancel", map[string]any{"reason": "guest changed plans"}, 200, "")
	r.act(b.Reservation.ID, "confirm", nil, 200, "")
	r.act(b.Reservation.ID, "confirm", nil, 409, "already_confirmed")
}

func TestStage22_BookingRules(t *testing.T) {
	r := newRoomsEnv(t)
	typ := r.roomType("Standard", 650000, 2)
	room := r.room("301", typ)
	other := r.room("302", typ)

	// Who may use it: reception and the manager; the cashier and signed-out callers may not.
	r.call(r.cash, "GET", "/api/v1/reservations", nil, 403, "forbidden")
	r.call(nil, "GET", "/api/v1/guests", nil, 401, "")

	// Dates and numbers.
	r.book(room, r.day(-1), r.day(1), "", 400, "validation_failed")
	r.book(room, r.day(2), r.day(2), "", 400, "validation_failed")
	r.book(room, r.day(2), r.day(100), "", 400, "validation_failed")
	r.call(r.recep, "POST", "/api/v1/reservations", map[string]any{"guest_id": r.guest, "room_id": room, "check_in": r.day(1), "check_out": r.day(2), "adults": 3}, 400, "validation_failed")
	r.call(r.recep, "POST", "/api/v1/reservations", map[string]any{"guest_id": r.guest, "room_id": room, "check_in": "next friday", "check_out": r.day(2)}, 400, "validation_failed")
	r.call(r.recep, "POST", "/api/v1/reservations", map[string]any{"guest_id": 99999, "room_id": room, "check_in": r.day(1), "check_out": r.day(2)}, 400, "validation_failed")
	r.call(r.recep, "POST", "/api/v1/reservations", map[string]any{"room_id": room, "check_in": r.day(1), "check_out": r.day(2)}, 400, "validation_failed")

	// A new guest can be given with the booking, in one step; the rate is copied and later rate changes do not touch it.
	w := r.call(r.recep, "POST", "/api/v1/reservations", map[string]any{"guest": map[string]any{"full_name": "Peter Mwangi", "phone": "0712345678"}, "room_id": room,
		"check_in": r.day(5), "check_out": r.day(7), "adults": 2, "nightly_rate_cents": 600000}, 201, "")
	b := decodeInto[resOut](t, w)
	if b.Reservation.GuestName != "Peter Mwangi" || b.Reservation.NightlyRateCents != 600000 || b.Reservation.TotalCents != 1200000 {
		t.Fatalf("booking: %+v", b.Reservation)
	}
	r.call(r.mgr, "PATCH", "/api/v1/room-types/"+itoa(typ), map[string]any{"base_rate_cents": 800000}, 200, "")
	got := decodeInto[resOut](t, r.call(r.recep, "GET", "/api/v1/reservations/"+itoa(b.Reservation.ID), nil, 200, ""))
	if got.Reservation.NightlyRateCents != 600000 {
		t.Fatal("a booking followed the room type's new rate")
	}
	if r.book(other, r.day(5), r.day(6), "", 201, "").Reservation.NightlyRateCents != 800000 {
		t.Fatal("a new booking did not take the new room-type rate")
	}

	// Moving a booking is held to the same rule.
	r.book(room, r.day(8), r.day(10), "", 201, "")
	r.call(r.recep, "PATCH", "/api/v1/reservations/"+itoa(b.Reservation.ID), map[string]any{"check_out": r.day(9)}, 409, "room_unavailable")
	moved := decodeInto[resOut](t, r.call(r.recep, "PATCH", "/api/v1/reservations/"+itoa(b.Reservation.ID), map[string]any{"room_id": other, "check_in": r.day(6), "check_out": r.day(8), "notes": "late arrival"}, 200, ""))
	if moved.Reservation.RoomNumber != "302" || moved.Reservation.Nights != 2 {
		t.Fatalf("moved booking: %+v", moved.Reservation)
	}

	// Same key, same booking; a different body with that key is refused.
	key := newKey()
	body := map[string]any{"guest_id": r.guest, "room_id": other, "check_in": r.day(20), "check_out": r.day(21)}
	first := r.e.do(req{method: "POST", path: "/api/v1/reservations", jar: r.recep, header: map[string]string{"Idempotency-Key": key}, body: body})
	expect(t, first, 201, "")
	again := r.e.do(req{method: "POST", path: "/api/v1/reservations", jar: r.recep, header: map[string]string{"Idempotency-Key": key}, body: body})
	expect(t, again, 201, "")
	if again.Header().Get("Idempotent-Replayed") != "true" || r.e.count(`SELECT count(*) FROM reservations WHERE check_in = $1::date`, r.day(20)) != 1 {
		t.Fatal("a retried booking was made twice")
	}
	body["check_out"] = r.day(22)
	expect(t, r.e.do(req{method: "POST", path: "/api/v1/reservations", jar: r.recep, header: map[string]string{"Idempotency-Key": key}, body: body}), 422, "idempotency_key_reused")

	// Cancelling needs a reason; an ended booking stays ended.
	c := r.book(room, r.day(30), r.day(31), "", 201, "")
	r.act(c.Reservation.ID, "cancel", map[string]any{}, 400, "validation_failed")
	r.act(c.Reservation.ID, "cancel", map[string]any{"reason": "  "}, 400, "validation_failed")
	r.act(c.Reservation.ID, "cancel", map[string]any{"reason": "double booked by mistake"}, 200, "")
	r.act(c.Reservation.ID, "cancel", map[string]any{"reason": "again"}, 409, "reservation_closed")
	r.act(c.Reservation.ID, "confirm", nil, 409, "reservation_closed")
	r.call(r.recep, "PATCH", "/api/v1/reservations/"+itoa(c.Reservation.ID), map[string]any{"notes": "x"}, 409, "reservation_closed")
	if _, err := r.e.pool.Exec(context.Background(), `UPDATE reservations SET status = 'confirmed', cancelled_reason = NULL, cancelled_by = NULL, cancelled_at = NULL WHERE id = $1`, c.Reservation.ID); err == nil {
		t.Fatal("the database let a cancelled booking come back")
	}

	// No-show: not before the guest is due.
	soon := r.book(room, r.day(40), r.day(41), "", 201, "")
	r.act(soon.Reservation.ID, "no-show", nil, 409, "too_early")
	due := r.book(room, r.day(0), r.day(1), "", 201, "")
	if r.act(due.Reservation.ID, "no-show", nil, 200, "").Reservation.Status != "no_show" {
		t.Fatal("no-show did not stick")
	}
	r.book(room, r.day(0), r.day(1), "", 201, "") // a no-show frees the room

	// A room with bookings cannot be switched off; a room that is off cannot be booked.
	r.call(r.mgr, "PATCH", "/api/v1/rooms/"+itoa(room), map[string]any{"active": false}, 409, "room_in_use")
	free := r.room("303", typ)
	r.call(r.mgr, "PATCH", "/api/v1/rooms/"+itoa(free), map[string]any{"active": false}, 200, "")
	r.book(free, r.day(1), r.day(2), "", 400, "validation_failed")
	r.call(r.mgr, "PATCH", "/api/v1/room-types/"+itoa(typ), map[string]any{"active": false}, 409, "room_type_in_use")

	// Guests: search, edit, bad input.
	r.call(r.recep, "POST", "/api/v1/guests", map[string]any{"full_name": "X", "email": "not-an-email"}, 400, "validation_failed")
	r.call(r.recep, "PATCH", "/api/v1/guests/"+itoa(r.guest), map[string]any{"email": "amina@example.com"}, 200, "")
	gl := decodeInto[struct {
		Guests []struct {
			FullName string `json:"full_name"`
			Email    string `json:"email"`
		} `json:"guests"`
	}](t, r.call(r.recep, "GET", "/api/v1/guests?q=amina", nil, 200, ""))
	if len(gl.Guests) != 1 || gl.Guests[0].Email != "amina@example.com" {
		t.Fatalf("guest search: %+v", gl.Guests)
	}
	list := decodeInto[struct {
		Reservations []struct {
			Status string `json:"status"`
		} `json:"reservations"`
	}](t, r.call(r.recep, "GET", "/api/v1/reservations?status=cancelled", nil, 200, ""))
	if len(list.Reservations) != 1 {
		t.Fatalf("cancelled list: %d", len(list.Reservations))
	}
	r.call(r.recep, "GET", "/api/v1/reservations?status=bogus", nil, 400, "validation_failed")
	r.call(r.recep, "GET", "/api/v1/availability?from="+r.day(3), nil, 400, "validation_failed")
}
