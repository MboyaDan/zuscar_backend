package handler_test

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
)

// Stage 23: check-in, the stay, the guest folio, check-out and room status (AC-08; ASSUMPTIONS.md S-97 to S-102).

type folioItem struct {
	ID         int64  `json:"id"`
	Kind       string `json:"kind"`
	Amount     int64  `json:"amount_cents"`
	Tax        int64  `json:"tax_cents"`
	NightDate  string `json:"night_date"`
	ReversesID *int64 `json:"reverses_item_id"`
	Reversed   bool   `json:"reversed"`
}

type folioOut struct {
	Folio struct {
		ID         int64  `json:"id"`
		FolioNo    int64  `json:"folio_no"`
		Status     string `json:"status"`
		StayStatus string `json:"stay_status"`
		RoomNumber string `json:"room_number"`
		Items      []folioItem
		Payments   []struct {
			ID      int64  `json:"id"`
			Status  string `json:"status"`
			Method  string `json:"method"`
			Purpose string `json:"purpose"`
			Amount  int64  `json:"amount_cents"`
		} `json:"payments"`
		Totals struct {
			Items    int64 `json:"items_cents"`
			Paid     int64 `json:"paid_cents"`
			Pending  int64 `json:"pending_cents"`
			WriteOff int64 `json:"write_off_cents"`
			Balance  int64 `json:"balance_cents"`
		} `json:"totals"`
	} `json:"folio"`
}

type checkInOut struct {
	Reservation struct {
		ID      int64  `json:"id"`
		Status  string `json:"status"`
		StayID  *int64 `json:"stay_id"`
		FolioID *int64 `json:"folio_id"`
	} `json:"reservation"`
	StayID  int64 `json:"stay_id"`
	FolioID int64 `json:"folio_id"`
}

func (r *roomsEnv) post(j jar, path string, body any, want int, errCode string) *httptest.ResponseRecorder {
	r.t.Helper()
	w := r.e.do(req{method: "POST", path: path, jar: j, body: body, header: map[string]string{"Idempotency-Key": newKey()}})
	expect(r.t, w, want, errCode)
	return w
}

func (r *roomsEnv) checkIn(resID int64, want int, errCode string) checkInOut {
	r.t.Helper()
	w := r.post(r.recep, "/api/v1/reservations/"+itoa(resID)+"/check-in", nil, want, errCode)
	if want >= 400 {
		return checkInOut{}
	}
	return decodeInto[checkInOut](r.t, w)
}

func (r *roomsEnv) folio(id int64) folioOut {
	r.t.Helper()
	return decodeInto[folioOut](r.t, r.call(r.recep, "GET", "/api/v1/folios/"+itoa(id), nil, 200, ""))
}

func (r *roomsEnv) status(room int64) string {
	r.t.Helper()
	list := decodeInto[struct {
		Rooms []struct {
			ID     int64  `json:"id"`
			Status string `json:"physical_status"`
		} `json:"rooms"`
	}](r.t, r.call(r.recep, "GET", "/api/v1/rooms", nil, 200, ""))
	for _, x := range list.Rooms {
		if x.ID == room {
			return x.Status
		}
	}
	r.t.Fatal("room not listed")
	return ""
}

// stay books a room from today for the given nights and checks the guest in.
func (r *roomsEnv) stay(room int64, nights int) checkInOut {
	r.t.Helper()
	b := r.book(room, r.day(0), r.day(nights), "", 201, "")
	return r.checkIn(b.Reservation.ID, 201, "")
}

func (r *roomsEnv) pay(j jar, folio int64, body map[string]any, want int, errCode string) folioOut {
	r.t.Helper()
	w := r.post(j, "/api/v1/folios/"+itoa(folio)+"/payments", body, want, errCode)
	if want >= 400 {
		return folioOut{}
	}
	return decodeInto[folioOut](r.t, w)
}

func TestStage23_CheckInPostsNightsAndOccupiesTheRoom(t *testing.T) {
	r := newRoomsEnv(t)
	room := r.room("401", r.roomType("Standard", 650000, 2))
	dirty := r.room("402", r.types["Standard"])
	r.call(r.recep, "POST", "/api/v1/rooms/"+itoa(dirty)+"/status", map[string]any{"status": "dirty"}, 200, "")

	// Not before the booked date, not while on hold, not into a room that is not ready.
	future := r.book(room, r.day(4), r.day(5), "", 201, "")
	r.checkIn(future.Reservation.ID, 409, "too_early")
	hold := r.book(room, r.day(6), r.day(7), "tentative", 201, "")
	r.checkIn(hold.Reservation.ID, 409, "not_confirmed")
	notReady := r.book(dirty, r.day(0), r.day(1), "", 201, "")
	r.checkIn(notReady.Reservation.ID, 409, "room_not_ready")

	b := r.book(room, r.day(0), r.day(3), "", 201, "")
	key := newKey()
	first := r.e.do(req{method: "POST", path: "/api/v1/reservations/" + itoa(b.Reservation.ID) + "/check-in", jar: r.recep, header: map[string]string{"Idempotency-Key": key}})
	expect(t, first, 201, "")
	again := r.e.do(req{method: "POST", path: "/api/v1/reservations/" + itoa(b.Reservation.ID) + "/check-in", jar: r.recep, header: map[string]string{"Idempotency-Key": key}})
	expect(t, again, 201, "")
	if again.Header().Get("Idempotent-Replayed") != "true" || r.e.count(`SELECT count(*) FROM stays`) != 1 || r.e.count(`SELECT count(*) FROM guest_folios`) != 1 {
		t.Fatal("a retried check-in made a second stay or folio")
	}
	ci := decodeInto[checkInOut](t, first)
	if ci.Reservation.Status != "checked_in" || ci.Reservation.StayID == nil || ci.Reservation.FolioID == nil || *ci.Reservation.FolioID != ci.FolioID {
		t.Fatalf("check-in result: %+v", ci)
	}
	r.checkIn(b.Reservation.ID, 409, "already_checked_in")
	if r.status(room) != "occupied" {
		t.Fatalf("room is %s after check-in, want occupied", r.status(room))
	}
	f := r.folio(ci.FolioID)
	if f.Folio.FolioNo != 1 || f.Folio.Status != "open" || len(f.Folio.Items) != 3 || f.Folio.Totals.Items != 1950000 || f.Folio.Totals.Balance != 1950000 || f.Folio.RoomNumber != "401" {
		t.Fatalf("folio after check-in: %+v", f.Folio)
	}
	var bp int
	if err := r.e.pool.QueryRow(context.Background(), `SELECT x.rate_bp FROM room_types t JOIN tax_rates x ON x.id = t.tax_rate_id WHERE t.id = $1`, r.types["Standard"]).Scan(&bp); err != nil {
		t.Fatal(err)
	}
	for _, it := range f.Folio.Items {
		if it.Kind != "room_night" || it.Amount != 650000 || it.Tax != (650000*int64(bp)*2+int64(10000+bp))/(2*int64(10000+bp)) {
			t.Fatalf("night item: %+v (rate %d bp)", it, bp)
		}
	}
	// The room is held by the guest: no second stay, and the room cannot be booked over the stay.
	r.book(room, r.day(1), r.day(2), "", 409, "room_unavailable")
	// The folio is a ledger: rows never change, and nothing is a payment yet.
	if _, err := r.e.pool.Exec(context.Background(), `UPDATE folio_items SET amount_cents = 1 WHERE folio_id = $1`, ci.FolioID); err == nil {
		t.Fatal("a folio row could be edited")
	}
	if _, err := r.e.pool.Exec(context.Background(), `DELETE FROM folio_items WHERE folio_id = $1`, ci.FolioID); err == nil {
		t.Fatal("a folio row could be deleted")
	}
	if r.e.count(`SELECT count(*) FROM payments WHERE folio_id = $1`, ci.FolioID) != 0 {
		t.Fatal("posting charges created a payment")
	}
}

func TestAC08_FolioBalanceFromItemsPaymentsAdjustments(t *testing.T) {
	r := newRoomsEnv(t)
	room := r.room("411", r.roomType("Standard", 650000, 2))
	ci := r.stay(room, 3)
	r.e.openShift(t, r.mgr, 0) // the manager takes cash at the desk in these tests
	f := r.folio(ci.FolioID)
	night := f.Folio.Items[0].ID

	// Cash counts at once. M-Pesa waits for confirmation and is not in the balance until then.
	f = r.pay(r.mgr, ci.FolioID, map[string]any{"method": "cash", "amount_cents": 500000}, 201, "")
	if f.Folio.Totals.Paid != 500000 || f.Folio.Totals.Balance != 1450000 {
		t.Fatalf("after cash: %+v", f.Folio.Totals)
	}
	f = r.pay(r.mgr, ci.FolioID, map[string]any{"method": "mpesa_manual", "amount_cents": 300000, "reference": "QWE123ABC1"}, 201, "")
	if f.Folio.Totals.Pending != 300000 || f.Folio.Totals.Paid != 500000 || f.Folio.Totals.Balance != 1450000 {
		t.Fatalf("with M-Pesa waiting: %+v", f.Folio.Totals)
	}
	mpesa := f.Folio.Payments[len(f.Folio.Payments)-1].ID
	// The same M-Pesa code cannot be recorded twice; promised money counts against what can still be taken.
	r.pay(r.mgr, ci.FolioID, map[string]any{"method": "mpesa_manual", "amount_cents": 100000, "reference": "qwe123abc1"}, 409, "duplicate_reference")
	r.pay(r.mgr, ci.FolioID, map[string]any{"method": "cash", "amount_cents": 1200000}, 400, "validation_failed")
	r.pay(r.mgr, ci.FolioID, map[string]any{"method": "cash", "amount_cents": 0}, 400, "validation_failed")
	// The payments list shows it for the reconciler, who confirms it.
	r.post(r.mgr, "/api/v1/payments/"+itoa(mpesa)+"/confirm", map[string]any{}, 200, "")
	f = r.folio(ci.FolioID)
	if f.Folio.Totals.Paid != 800000 || f.Folio.Totals.Pending != 0 || f.Folio.Totals.Balance != 1150000 {
		t.Fatalf("after confirming: %+v", f.Folio.Totals)
	}

	// Adjustments: a charge by the desk, a credit only with a manager; a night is credited back once, in full.
	adj := func(j jar, body map[string]any, want int, code string) folioOut {
		w := r.post(j, "/api/v1/folios/"+itoa(ci.FolioID)+"/adjustments", body, want, code)
		if want >= 400 {
			return folioOut{}
		}
		return decodeInto[folioOut](t, w)
	}
	adj(r.recep, map[string]any{"description": "Broken glass", "amount_cents": 100000}, 400, "validation_failed") // a reason is required
	f = adj(r.recep, map[string]any{"description": "Broken glass", "amount_cents": 100000, "reason": "Guest broke a glass"}, 201, "")
	if f.Folio.Totals.Balance != 1250000 {
		t.Fatalf("after a charge: %+v", f.Folio.Totals)
	}
	adj(r.recep, map[string]any{"description": "Discount", "amount_cents": -50000, "reason": "Goodwill"}, 409, "manager_required")
	adj(r.recep, map[string]any{"reverses_item_id": night, "reason": "Slept elsewhere"}, 409, "manager_required")
	f = adj(r.mgr, map[string]any{"reverses_item_id": night, "reason": "Slept elsewhere"}, 201, "")
	if f.Folio.Totals.Balance != 600000 || f.Folio.Totals.Items != 1950000+100000-650000 {
		t.Fatalf("after crediting a night: %+v", f.Folio.Totals)
	}
	adj(r.mgr, map[string]any{"reverses_item_id": night, "reason": "again"}, 409, "already_reversed")
	adj(r.mgr, map[string]any{"reverses_item_id": 999999, "reason": "x"}, 400, "validation_failed")
	var reversed int
	for _, it := range f.Folio.Items {
		if it.ID == night && it.Reversed {
			reversed++
		}
	}
	if reversed != 1 {
		t.Fatal("the credited night is not marked reversed")
	}
	// None of it is a payment (NN-10): only the two payments exist.
	if r.e.count(`SELECT count(*) FROM payments WHERE folio_id = $1`, ci.FolioID) != 2 {
		t.Fatal("charges or adjustments created payments")
	}
	// The balance is items - confirmed payments, worked out each time (AC-08).
	var want int64
	if err := r.e.pool.QueryRow(context.Background(), `SELECT (SELECT sum(amount_cents) FROM folio_items WHERE folio_id = $1) - (SELECT sum(amount_cents) FROM payments WHERE folio_id = $1 AND status = 'confirmed')`, ci.FolioID).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if got := r.folio(ci.FolioID).Folio.Totals.Balance; got != want {
		t.Fatalf("balance %d, but items minus confirmed payments is %d", got, want)
	}
	// A reader without folios.view cannot see it; the desk can.
	r.call(r.cash, "GET", "/api/v1/folios/"+itoa(ci.FolioID), nil, 403, "forbidden")
	r.call(r.recep, "GET", "/api/v1/folios/999999", nil, 404, "not_found")
}

func TestAC08_CloseNeedsZeroBalanceOrWriteOff(t *testing.T) {
	r := newRoomsEnv(t)
	room := r.room("421", r.roomType("Standard", 650000, 2))
	ci := r.stay(room, 2) // KES 13,000
	r.e.openShift(t, r.mgr, 0)
	out := "/api/v1/stays/" + itoa(ci.StayID) + "/check-out"

	r.post(r.recep, out, nil, 409, "balance_due")
	r.pay(r.mgr, ci.FolioID, map[string]any{"method": "cash", "amount_cents": 1000000}, 201, "")
	// A manager gives up part of the rest; the desk cannot.
	wo := func(j jar, amount int64, reason string, want int, code string) folioOut {
		w := r.post(j, "/api/v1/folios/"+itoa(ci.FolioID)+"/write-off", map[string]any{"amount_cents": amount, "reason": reason}, want, code)
		if want >= 400 {
			return folioOut{}
		}
		return decodeInto[folioOut](t, w)
	}
	wo(r.recep, 100000, "Goodwill", 409, "manager_required")
	wo(r.mgr, 100000, "", 400, "validation_failed")
	wo(r.mgr, 400000, "Too much", 400, "validation_failed") // more than the 300,000 still owed
	f := wo(r.mgr, 100000, "Goodwill: air conditioning broke", 200, "")
	if f.Folio.Totals.WriteOff != 100000 || f.Folio.Totals.Balance != 200000 {
		t.Fatalf("after write-off: %+v", f.Folio.Totals)
	}
	wo(r.mgr, 50000, "Second", 409, "write_off_exists")
	r.post(r.recep, out, nil, 409, "balance_due")
	// A payment still waiting blocks checking out.
	r.pay(r.mgr, ci.FolioID, map[string]any{"method": "mpesa_manual", "amount_cents": 200000, "reference": "ZXC987LKJ2"}, 201, "")
	r.post(r.recep, out, nil, 409, "payments_pending")
	pending := r.folio(ci.FolioID).Folio.Payments
	r.post(r.mgr, "/api/v1/payments/"+itoa(pending[len(pending)-1].ID)+"/confirm", map[string]any{}, 200, "")

	key := newKey()
	w := r.e.do(req{method: "POST", path: out, jar: r.recep, header: map[string]string{"Idempotency-Key": key}})
	expect(t, w, 200, "")
	f = decodeInto[folioOut](t, w)
	if f.Folio.Status != "closed" || f.Folio.StayStatus != "checked_out" || f.Folio.Totals.Balance != 0 {
		t.Fatalf("after check-out: %+v", f.Folio)
	}
	again := r.e.do(req{method: "POST", path: out, jar: r.recep, header: map[string]string{"Idempotency-Key": key}})
	expect(t, again, 200, "")
	if again.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("a retried check-out was not replayed")
	}
	r.post(r.recep, out, nil, 409, "stay_ended")
	if r.status(room) != "dirty" {
		t.Fatalf("room is %s after check-out, want dirty", r.status(room))
	}
	if r.e.count(`SELECT count(*) FROM reservations WHERE status = 'checked_out'`) != 1 {
		t.Fatal("the booking did not move to checked out")
	}
	// The room is free again for later nights, and a closed folio takes nothing more.
	r.book(room, r.day(1), r.day(2), "", 201, "")
	r.pay(r.mgr, ci.FolioID, map[string]any{"method": "cash", "amount_cents": 100}, 409, "folio_closed")
	r.post(r.mgr, "/api/v1/folios/"+itoa(ci.FolioID)+"/adjustments", map[string]any{"description": "Minibar", "amount_cents": 50000, "reason": "Late"}, 409, "folio_closed")
	if _, err := r.e.pool.Exec(context.Background(), `INSERT INTO folio_items (folio_id, kind, description, amount_cents, reason, posted_by) VALUES ($1, 'adjustment', 'x', 100, 'x', (SELECT id FROM users LIMIT 1))`, ci.FolioID); err == nil {
		t.Fatal("the database accepted a charge on a closed folio")
	}
	// Late posting (S-20): reopen with a reason, post, settle, close again. The guest does not go back into the room.
	r.post(r.recep, "/api/v1/folios/"+itoa(ci.FolioID)+"/reopen", map[string]any{}, 400, "validation_failed")
	r.post(r.recep, "/api/v1/folios/"+itoa(ci.FolioID)+"/reopen", map[string]any{"reason": "Minibar found after leaving"}, 200, "")
	r.post(r.recep, "/api/v1/folios/"+itoa(ci.FolioID)+"/adjustments", map[string]any{"description": "Minibar", "amount_cents": 50000, "reason": "Found after check-out"}, 201, "")
	r.post(r.recep, "/api/v1/folios/"+itoa(ci.FolioID)+"/close", nil, 409, "balance_due")
	r.pay(r.mgr, ci.FolioID, map[string]any{"method": "cash", "amount_cents": 50000}, 201, "")
	r.post(r.recep, "/api/v1/folios/"+itoa(ci.FolioID)+"/close", nil, 200, "")
	if r.status(room) != "dirty" {
		t.Fatal("reopening a folio changed the room")
	}
	// The database refuses a write-off without an approver and a second change of one.
	if _, err := r.e.pool.Exec(context.Background(), `UPDATE guest_folios SET write_off_cents = 5 WHERE id = $1`, ci.FolioID); err == nil {
		t.Fatal("a closed folio changed")
	}
}

func TestAC08_ConcurrentPaymentsNeverTakeMoreThanIsOwed(t *testing.T) {
	r := newRoomsEnv(t)
	room := r.room("431", r.roomType("Standard", 650000, 2))
	ci := r.stay(room, 2) // KES 13,000
	r.e.openShift(t, r.mgr, 0)
	r.pay(r.mgr, ci.FolioID, map[string]any{"method": "cash", "amount_cents": 700000}, 201, "") // 6,000 left
	const n = 6
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := r.e.do(req{method: "POST", path: "/api/v1/folios/" + itoa(ci.FolioID) + "/payments", jar: r.mgr, header: map[string]string{"Idempotency-Key": newKey()}, body: map[string]any{"method": "cash", "amount_cents": 250000}})
			codes[i] = w.Code
		}()
	}
	wg.Wait()
	won := 0
	for _, c := range codes {
		if c == 201 {
			won++
		} else if c != 400 {
			t.Fatalf("unexpected status %d", c)
		}
	}
	if won != 2 {
		t.Fatalf("%d payments of 2,500 went through against 6,000 owed, want exactly 2", won)
	}
	if b := r.folio(ci.FolioID).Folio.Totals.Balance; b != 100000 {
		t.Fatalf("balance %d, want 100000", b)
	}
}

func TestStage23_ExtendAndChangeRoom(t *testing.T) {
	r := newRoomsEnv(t)
	typ := r.roomType("Standard", 650000, 2)
	a, b, c := r.room("501", typ), r.room("502", typ), r.room("503", typ)
	ci := r.stay(a, 2)
	stay := "/api/v1/stays/" + itoa(ci.StayID)

	// Extend: later only, nights are posted, and a room booked after the guest cannot be extended into.
	r.book(a, r.day(4), r.day(5), "", 201, "")
	r.post(r.recep, stay+"/extend", map[string]any{"check_out": r.day(1)}, 400, "validation_failed")
	r.post(r.recep, stay+"/extend", map[string]any{"check_out": r.day(5)}, 409, "room_unavailable")
	r.post(r.recep, stay+"/extend", map[string]any{"check_out": "soon"}, 400, "validation_failed")
	r.post(r.recep, stay+"/extend", map[string]any{"check_out": r.day(4)}, 200, "")
	f := r.folio(ci.FolioID)
	if len(f.Folio.Items) != 4 || f.Folio.Totals.Items != 2600000 {
		t.Fatalf("after extending: %d items, %d", len(f.Folio.Items), f.Folio.Totals.Items)
	}

	// Change room: a reason, a clean room, and the old room needs cleaning.
	r.e.exec(`UPDATE rooms SET physical_status = 'dirty' WHERE id = $1`, b)
	r.post(r.recep, stay+"/change-room", map[string]any{"room_id": b, "reason": "Noise"}, 409, "room_not_ready")
	r.post(r.recep, stay+"/change-room", map[string]any{"room_id": c}, 400, "validation_failed")
	r.post(r.recep, stay+"/change-room", map[string]any{"room_id": a, "reason": "Same"}, 400, "validation_failed")
	r.book(c, r.day(2), r.day(3), "", 201, "") // someone else is due in 503 during the stay
	r.post(r.recep, stay+"/change-room", map[string]any{"room_id": c, "reason": "Noise"}, 409, "room_unavailable")
	r.e.exec(`UPDATE rooms SET physical_status = 'vacant_ready' WHERE id = $1`, b)
	r.post(r.recep, stay+"/change-room", map[string]any{"room_id": b, "reason": "Noise next door"}, 200, "")
	if r.status(a) != "dirty" || r.status(b) != "occupied" {
		t.Fatalf("after the move: old room %s, new room %s", r.status(a), r.status(b))
	}
	if r.folio(ci.FolioID).Folio.RoomNumber != "502" || r.folio(ci.FolioID).Folio.Totals.Items != 2600000 {
		t.Fatal("the folio did not follow the guest or the price changed")
	}
	if n := r.e.count(`SELECT count(*) FROM room_status_history WHERE room_id IN ($1, $2)`, a, b); n != 3 {
		t.Fatalf("room status history has %d rows for the move", n)
	}
	// The vacated room can be booked for the nights the guest no longer holds it.
	r.book(a, r.day(0), r.day(1), "", 201, "")
}

func TestStage23_RoomStatusAndInHouse(t *testing.T) {
	r := newRoomsEnv(t)
	room := r.room("511", r.roomType("Standard", 650000, 2))
	other := r.room("512", r.types["Standard"])
	set := func(id int64, status, reason string, want int, code string) {
		r.t.Helper()
		r.call(r.recep, "POST", "/api/v1/rooms/"+itoa(id)+"/status", map[string]any{"status": status, "reason": reason}, want, code)
	}
	set(room, "cleaning", "", 409, "invalid_transition")
	set(room, "occupied", "", 409, "invalid_transition")
	set(room, "out_of_service", "", 400, "validation_failed") // a reason is required
	set(room, "out_of_service", "Leaking tap", 200, "")
	r.book(room, r.day(1), r.day(2), "", 400, "validation_failed") // cannot be booked while out of service
	set(room, "dirty", "Back in service", 200, "")
	set(room, "cleaning", "", 200, "")
	set(room, "vacant_ready", "Inspected", 200, "")
	if r.e.count(`SELECT count(*) FROM room_status_history WHERE room_id = $1`, room) != 4 {
		t.Fatal("each change should write one history row")
	}
	// A guest in the room: it cannot be changed by hand.
	ci := r.stay(room, 1)
	set(room, "dirty", "", 409, "room_occupied")
	// Who is in the house: the desk sees the balance, a waiter sees room and guest only.
	r.e.addUser("waiter2", false, "waiter")
	wj := r.e.mustLogin("waiter2")
	got := decodeInto[struct {
		Stays []struct {
			RoomNumber string `json:"room_number"`
			GuestName  string `json:"guest_name"`
			FolioID    *int64 `json:"folio_id"`
			Balance    *int64 `json:"balance_cents"`
		} `json:"stays"`
	}](t, r.call(r.recep, "GET", "/api/v1/rooms/in-house", nil, 200, ""))
	if len(got.Stays) != 1 || got.Stays[0].RoomNumber != "511" || got.Stays[0].Balance == nil || *got.Stays[0].Balance != 650000 || got.Stays[0].FolioID == nil || *got.Stays[0].FolioID != ci.FolioID {
		t.Fatalf("in-house for the desk: %+v", got.Stays)
	}
	lim := decodeInto[struct {
		Stays []struct {
			RoomNumber string `json:"room_number"`
			GuestName  string `json:"guest_name"`
			FolioID    *int64 `json:"folio_id"`
			Balance    *int64 `json:"balance_cents"`
		} `json:"stays"`
	}](t, r.call(wj, "GET", "/api/v1/rooms/in-house", nil, 200, ""))
	if len(lim.Stays) != 1 || lim.Stays[0].GuestName != "Amina Otieno" || lim.Stays[0].Balance != nil || lim.Stays[0].FolioID == nil || *lim.Stays[0].FolioID != ci.FolioID {
		t.Fatalf("in-house for a waiter: %+v", lim.Stays)
	}
	_ = other
}
