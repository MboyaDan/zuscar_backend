package handler_test

import (
	"sync"
	"testing"
)

// Stage 24: charging a POS order to a guest's room, and housekeeping tasks (AC-08; ASSUMPTIONS.md S-103 to S-106).

type chargeOut struct {
	Charge struct {
		SaleID     int64  `json:"sale_id"`
		ReceiptNo  int64  `json:"receipt_no"`
		TotalCents int64  `json:"total_cents"`
		FolioID    int64  `json:"folio_id"`
		ItemID     int64  `json:"item_id"`
		RoomNumber string `json:"room_number"`
		GuestName  string `json:"guest_name"`
	} `json:"charge"`
}

type taskOut struct {
	Task struct {
		ID         int64  `json:"id"`
		RoomID     int64  `json:"room_id"`
		RoomNumber string `json:"room_number"`
		Kind       string `json:"kind"`
		Status     string `json:"status"`
		Note       string `json:"note"`
		AssignedTo *int64 `json:"assigned_to"`
	} `json:"task"`
}

type tasksOut struct {
	Tasks []struct {
		ID     int64  `json:"id"`
		RoomID int64  `json:"room_id"`
		Kind   string `json:"kind"`
		Status string `json:"status"`
	} `json:"tasks"`
}

func (r *roomsEnv) order(j jar, product int64, qty int) int64 {
	r.t.Helper()
	w := r.e.do(req{method: "POST", path: "/api/v1/orders", jar: j, body: map[string]any{"order_type": "takeaway"}})
	expect(r.t, w, 201, "")
	id := decodeInto[orderResp](r.t, w).Order.ID
	w = r.e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(id) + "/items", jar: j, body: map[string]any{"product_id": product, "qty": qty, "modifier_ids": []int64{}}})
	expect(r.t, w, 201, "")
	return id
}

func (r *roomsEnv) charge(j jar, folio int64, body map[string]any, key string, want int, code string) chargeOut {
	r.t.Helper()
	w := r.e.do(req{method: "POST", path: "/api/v1/folios/" + itoa(folio) + "/charges", jar: j, body: body, header: map[string]string{"Idempotency-Key": key}})
	expect(r.t, w, want, code)
	if want >= 400 {
		return chargeOut{}
	}
	return decodeInto[chargeOut](r.t, w)
}

func TestAC08_RoomChargeIsNotPayment(t *testing.T) {
	r := newRoomsEnv(t)
	r.e.addUser("wait", false, "waiter")
	wj := r.e.mustLogin("wait")
	tea, _, _ := menuFixture(t, r.e, r.mgr)
	room := r.room("601", r.roomType("Standard", 650000, 2))
	ci := r.stay(room, 1)        // KES 6,500
	order := r.order(wj, tea, 3) // 3 x 80.00 = KES 240.00
	key := newKey()

	// A waiter charges the order to the room. No payment is made and no shift is needed.
	got := r.charge(wj, ci.FolioID, map[string]any{"order_id": order}, key, 201, "")
	if got.Charge.TotalCents != 24000 || got.Charge.RoomNumber != "601" || got.Charge.GuestName != "Amina Otieno" || got.Charge.FolioID != ci.FolioID {
		t.Fatalf("charge: %+v", got.Charge)
	}
	f := r.folio(ci.FolioID)
	if f.Folio.Totals.Items != 650000+24000 || f.Folio.Totals.Balance != 650000+24000 || f.Folio.Totals.Paid != 0 {
		t.Fatalf("folio totals after the charge: %+v", f.Folio.Totals)
	}
	var pos int
	for _, it := range f.Folio.Items {
		if it.Kind == "pos_charge" {
			pos++
			if it.Amount != 24000 || it.Tax <= 0 {
				t.Fatalf("pos_charge item: %+v", it)
			}
		}
	}
	if pos != 1 {
		t.Fatalf("%d pos_charge items, want 1", pos)
	}
	// NN-10: a charge is a posting, never a payment.
	if r.e.count(`SELECT count(*) FROM payments WHERE folio_id = $1 OR sale_id = $2`, ci.FolioID, got.Charge.SaleID) != 0 {
		t.Fatal("a room charge created a payment")
	}
	var status string
	var folioID int64
	if err := r.e.pool.QueryRow(t.Context(), `SELECT payment_status, folio_id FROM sales WHERE id = $1`, got.Charge.SaleID).Scan(&status, &folioID); err != nil || status != "charged_to_folio" || folioID != ci.FolioID {
		t.Fatalf("sale row: %q folio %d err %v", status, folioID, err)
	}
	// The sale reads as a sale with nothing owed on it; the guest owes it on the folio.
	sale := decodeInto[struct {
		Sale struct {
			PaymentStatus string `json:"payment_status"`
			FolioID       *int64 `json:"folio_id"`
			BalanceCents  int64  `json:"balance_cents"`
			TotalCents    int64  `json:"total_cents"`
		} `json:"sale"`
	}](t, r.call(r.mgr, "GET", "/api/v1/sales/"+itoa(got.Charge.SaleID), nil, 200, ""))
	if sale.Sale.PaymentStatus != "charged_to_folio" || sale.Sale.FolioID == nil || sale.Sale.BalanceCents != 0 || sale.Sale.TotalCents != 24000 {
		t.Fatalf("sale view: %+v", sale.Sale)
	}
	// The order is finalised once, whichever way it is tried again.
	r.charge(wj, ci.FolioID, map[string]any{"order_id": order}, newKey(), 409, "order_already_finalised")
	r.e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(order) + "/finalise", jar: r.mgr, body: map[string]any{}})
	if r.e.count(`SELECT count(*) FROM sales WHERE order_id = $1`, order) != 1 {
		t.Fatal("the order was sold twice")
	}
	// A retry with the same key is replayed.
	again := r.e.do(req{method: "POST", path: "/api/v1/folios/" + itoa(ci.FolioID) + "/charges", jar: wj, body: map[string]any{"order_id": order}, header: map[string]string{"Idempotency-Key": key}})
	expect(t, again, 201, "")
	if again.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("a retried charge was not replayed")
	}
	// What a charged sale is not: refundable like a paid one (it is corrected on the folio).
	r.e.do(req{method: "GET", path: "/api/v1/sales", jar: r.mgr})
	// The balance now blocks check-out until the guest settles everything, charge included.
	r.post(r.recep, "/api/v1/stays/"+itoa(ci.StayID)+"/check-out", nil, 409, "balance_due")
	r.e.openShift(t, r.mgr, 0)
	r.pay(r.mgr, ci.FolioID, map[string]any{"method": "cash", "amount_cents": 674000}, 201, "")
	r.post(r.recep, "/api/v1/stays/"+itoa(ci.StayID)+"/check-out", nil, 200, "")
	// After check-out, nothing more goes on that room.
	order2 := r.order(wj, tea, 1)
	r.charge(wj, ci.FolioID, map[string]any{"order_id": order2}, newKey(), 409, "folio_closed")
	if r.e.count(`SELECT count(*) FROM sales WHERE order_id = $1`, order2) != 0 {
		t.Fatal("a refused room charge left a sale behind")
	}
}

func TestAC08_PosChargePostsOnce(t *testing.T) {
	r := newRoomsEnv(t)
	r.e.addUser("wait", false, "waiter")
	wj := r.e.mustLogin("wait")
	tea, _, _ := menuFixture(t, r.e, r.mgr)
	room := r.room("611", r.roomType("Standard", 650000, 2))
	ci := r.stay(room, 1)
	order := r.order(wj, tea, 2)
	const n = 6
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := r.e.do(req{method: "POST", path: "/api/v1/folios/" + itoa(ci.FolioID) + "/charges", jar: wj, body: map[string]any{"order_id": order}, header: map[string]string{"Idempotency-Key": newKey()}})
			codes[i] = w.Code
		}()
	}
	wg.Wait()
	won := 0
	for _, c := range codes {
		if c == 201 {
			won++
		} else if c != 409 {
			t.Fatalf("unexpected status %d", c)
		}
	}
	if won != 1 || r.e.count(`SELECT count(*) FROM sales WHERE order_id = $1`, order) != 1 || r.e.count(`SELECT count(*) FROM folio_items WHERE folio_id = $1 AND kind = 'pos_charge'`, ci.FolioID) != 1 {
		t.Fatalf("%d charges went through for one order (%v)", won, codes)
	}
	// The database refuses a second charge for one sale and a charge that does not name a sale.
	if _, err := r.e.pool.Exec(t.Context(), `INSERT INTO folio_items (folio_id, kind, description, amount_cents, sale_id, posted_by) SELECT $1, 'pos_charge', 'again', 100, id, cashier_id FROM sales WHERE order_id = $2`, ci.FolioID, order); err == nil {
		t.Fatal("a sale was charged to the folio twice")
	}
	if _, err := r.e.pool.Exec(t.Context(), `INSERT INTO folio_items (folio_id, kind, description, amount_cents, posted_by) VALUES ($1, 'pos_charge', 'no sale', 100, (SELECT id FROM users LIMIT 1))`, ci.FolioID); err == nil {
		t.Fatal("a charge with no sale was accepted")
	}
}

func TestStage24_RoomChargeRules(t *testing.T) {
	r := newRoomsEnv(t)
	r.e.addUser("wait", false, "waiter")
	wj := r.e.mustLogin("wait")
	tea, _, _ := menuFixture(t, r.e, r.mgr)
	room := r.room("621", r.roomType("Standard", 650000, 2))
	ci := r.stay(room, 1)
	order := r.order(wj, tea, 1)
	// Who may: folios.post. The storekeeper has no such permission.
	r.e.addUser("store", false, "storekeeper")
	r.charge(r.e.mustLogin("store"), ci.FolioID, map[string]any{"order_id": order}, newKey(), 403, "forbidden")
	// Bad requests leave nothing behind.
	r.charge(wj, ci.FolioID, map[string]any{}, newKey(), 400, "validation_failed")
	r.charge(wj, 999999, map[string]any{"order_id": order}, newKey(), 404, "not_found")
	r.charge(wj, ci.FolioID, map[string]any{"order_id": 999999}, newKey(), 404, "not_found")
	r.charge(wj, ci.FolioID, map[string]any{"order_id": order, "payments": []any{map[string]any{"method": "cash", "amount_cents": 100}}}, newKey(), 400, "validation_failed")
	// A waiter cannot give a discount; an empty order has nothing to charge.
	r.charge(wj, ci.FolioID, map[string]any{"order_id": order, "discounts": []any{map[string]any{"order_item_id": 1, "amount_cents": 100, "reason": "x"}}}, newKey(), 403, "discount_not_permitted")
	empty := decodeInto[orderResp](t, r.e.do(req{method: "POST", path: "/api/v1/orders", jar: wj, body: map[string]any{"order_type": "takeaway"}})).Order.ID
	r.charge(wj, ci.FolioID, map[string]any{"order_id": empty}, newKey(), 409, "order_empty")
	if r.e.count(`SELECT count(*) FROM sales`) != 0 || r.e.count(`SELECT count(*) FROM folio_items WHERE kind = 'pos_charge'`) != 0 {
		t.Fatal("a refused charge left a sale or a folio row behind")
	}
	// A guest who has left cannot be charged, even if the folio is reopened for a late item.
	r.e.openShift(t, r.mgr, 0)
	r.pay(r.mgr, ci.FolioID, map[string]any{"method": "cash", "amount_cents": 650000}, 201, "")
	r.post(r.recep, "/api/v1/stays/"+itoa(ci.StayID)+"/check-out", nil, 200, "")
	r.post(r.recep, "/api/v1/folios/"+itoa(ci.FolioID)+"/reopen", map[string]any{"reason": "late item"}, 200, "")
	r.charge(wj, ci.FolioID, map[string]any{"order_id": order}, newKey(), 409, "guest_not_in_house")
}

func (r *roomsEnv) tasks(query string) tasksOut {
	r.t.Helper()
	return decodeInto[tasksOut](r.t, r.call(r.recep, "GET", "/api/v1/housekeeping/tasks"+query, nil, 200, ""))
}

func (r *roomsEnv) patchTask(id int64, body map[string]any, want int, code string) taskOut {
	r.t.Helper()
	w := r.e.do(req{method: "PATCH", path: "/api/v1/housekeeping/tasks/" + itoa(id), jar: r.recep, body: body})
	expect(r.t, w, want, code)
	if want >= 400 {
		return taskOut{}
	}
	return decodeInto[taskOut](r.t, w)
}

func TestStage24_HousekeepingFollowsRoomStatus(t *testing.T) {
	r := newRoomsEnv(t)
	room := r.room("701", r.roomType("Standard", 650000, 2))
	other := r.room("702", r.types["Standard"])
	ci := r.stay(room, 1)
	if len(r.tasks("").Tasks) != 0 {
		t.Fatal("a task appeared before anyone left")
	}
	// Checking out makes the room dirty and opens a cleaning job.
	r.e.openShift(t, r.mgr, 0)
	r.pay(r.mgr, ci.FolioID, map[string]any{"method": "cash", "amount_cents": 650000}, 201, "")
	r.post(r.recep, "/api/v1/stays/"+itoa(ci.StayID)+"/check-out", nil, 200, "")
	list := r.tasks("?status=active")
	if len(list.Tasks) != 1 || list.Tasks[0].RoomID != room || list.Tasks[0].Kind != "clean" || list.Tasks[0].Status != "open" {
		t.Fatalf("tasks after check-out: %+v", list.Tasks)
	}
	id := list.Tasks[0].ID
	// A second cleaning job for the same room is refused.
	r.call(r.recep, "POST", "/api/v1/housekeeping/tasks", map[string]any{"room_id": room, "kind": "clean"}, 409, "task_exists")
	// Starting moves the room to cleaning; finishing makes it ready and sellable.
	if tk := r.patchTask(id, map[string]any{"status": "in_progress"}, 200, ""); tk.Task.Status != "in_progress" || tk.Task.AssignedTo == nil {
		t.Fatalf("started: %+v", tk.Task)
	}
	if r.status(room) != "cleaning" {
		t.Fatalf("room is %s after the job started, want cleaning", r.status(room))
	}
	r.patchTask(id, map[string]any{"status": "in_progress"}, 409, "invalid_transition")
	if tk := r.patchTask(id, map[string]any{"status": "done"}, 200, ""); tk.Task.Status != "done" {
		t.Fatalf("finished: %+v", tk.Task)
	}
	if r.status(room) != "vacant_ready" {
		t.Fatalf("room is %s after the job finished, want vacant_ready", r.status(room))
	}
	r.patchTask(id, map[string]any{"status": "cancelled"}, 409, "task_finished")
	if _, err := r.e.pool.Exec(t.Context(), `DELETE FROM housekeeping_tasks WHERE id = $1`, id); err == nil {
		t.Fatal("a task could be deleted")
	}
	if _, err := r.e.pool.Exec(t.Context(), `UPDATE housekeeping_tasks SET status = 'open' WHERE id = $1`, id); err == nil {
		t.Fatal("a finished task could be reopened")
	}

	// Room status by hand drives the same job: dirty opens one, cleaning starts it, ready finishes it.
	r.call(r.recep, "POST", "/api/v1/rooms/"+itoa(other)+"/status", map[string]any{"status": "dirty"}, 200, "")
	t2 := r.tasks("?status=open")
	if len(t2.Tasks) != 1 || t2.Tasks[0].RoomID != other {
		t.Fatalf("tasks after marking dirty: %+v", t2.Tasks)
	}
	r.call(r.recep, "POST", "/api/v1/rooms/"+itoa(other)+"/status", map[string]any{"status": "cleaning"}, 200, "")
	if len(r.tasks("?status=in_progress").Tasks) != 1 {
		t.Fatal("starting to clean did not start the job")
	}
	r.call(r.recep, "POST", "/api/v1/rooms/"+itoa(other)+"/status", map[string]any{"status": "vacant_ready"}, 200, "")
	if len(r.tasks("?status=active").Tasks) != 0 || len(r.tasks("?status=done").Tasks) != 2 {
		t.Fatal("making the room ready did not finish the job")
	}
	// Finishing a job straight from open walks the room through cleaning to ready.
	r.call(r.recep, "POST", "/api/v1/rooms/"+itoa(other)+"/status", map[string]any{"status": "dirty"}, 200, "")
	t3 := r.tasks("?status=active")
	r.patchTask(t3.Tasks[0].ID, map[string]any{"status": "done"}, 200, "")
	if r.status(other) != "vacant_ready" {
		t.Fatalf("room is %s, want vacant_ready", r.status(other))
	}
	// Out of service cancels a pending cleaning job.
	r.call(r.recep, "POST", "/api/v1/rooms/"+itoa(other)+"/status", map[string]any{"status": "dirty"}, 200, "")
	r.call(r.recep, "POST", "/api/v1/rooms/"+itoa(other)+"/status", map[string]any{"status": "out_of_service", "reason": "Leak"}, 200, "")
	if len(r.tasks("?status=active").Tasks) != 0 || len(r.tasks("?status=cancelled").Tasks) != 1 {
		t.Fatal("taking the room out of service did not cancel its cleaning job")
	}
}

func TestStage24_HousekeepingTaskRules(t *testing.T) {
	r := newRoomsEnv(t)
	room := r.room("711", r.roomType("Standard", 650000, 2))
	r.call(r.cash, "GET", "/api/v1/housekeeping/tasks", nil, 403, "forbidden")
	r.call(r.cash, "POST", "/api/v1/housekeeping/tasks", map[string]any{"room_id": room, "kind": "maintenance", "note": "x"}, 403, "forbidden")
	r.call(r.recep, "POST", "/api/v1/housekeeping/tasks", map[string]any{"room_id": room, "kind": "paint"}, 400, "validation_failed")
	r.call(r.recep, "POST", "/api/v1/housekeeping/tasks", map[string]any{"room_id": room, "kind": "maintenance"}, 400, "validation_failed") // say what is wrong
	r.call(r.recep, "POST", "/api/v1/housekeeping/tasks", map[string]any{"room_id": 999999, "kind": "clean"}, 400, "validation_failed")
	r.call(r.recep, "POST", "/api/v1/housekeeping/tasks", map[string]any{"room_id": room, "kind": "maintenance", "note": "Tap drips", "assigned_to": 999999}, 400, "validation_failed")
	w := r.call(r.recep, "POST", "/api/v1/housekeeping/tasks", map[string]any{"room_id": room, "kind": "maintenance", "note": "Tap drips"}, 201, "")
	mt := decodeInto[taskOut](t, w).Task
	if mt.Status != "open" || mt.Kind != "maintenance" || mt.Note != "Tap drips" {
		t.Fatalf("maintenance task: %+v", mt)
	}
	// A repair note does not touch the room, and can be reworded and given to someone.
	var staff int64
	if err := r.e.pool.QueryRow(t.Context(), `SELECT id FROM users WHERE username = 'cash'`).Scan(&staff); err != nil {
		t.Fatal(err)
	}
	up := r.patchTask(mt.ID, map[string]any{"note": "Tap drips in the bathroom", "assigned_to": staff}, 200, "")
	if up.Task.Note != "Tap drips in the bathroom" || up.Task.AssignedTo == nil || *up.Task.AssignedTo != staff {
		t.Fatalf("updated: %+v", up.Task)
	}
	if r.status(room) != "vacant_ready" {
		t.Fatal("a maintenance task changed the room")
	}
	r.patchTask(mt.ID, map[string]any{"status": "paused"}, 400, "validation_failed")
	r.patchTask(999999, map[string]any{"note": "x"}, 404, "not_found")
	r.patchTask(mt.ID, map[string]any{"status": "done"}, 200, "")
	if r.status(room) != "vacant_ready" {
		t.Fatal("finishing a maintenance task changed the room")
	}
	r.patchTask(mt.ID, map[string]any{"note": "again"}, 409, "task_finished")
}
