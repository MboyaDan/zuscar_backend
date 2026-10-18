package handler_test

import (
	"context"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"
)

// Stage 10b-1: kitchen tickets (AC-04). These run against a real PostgreSQL through the HTTP routes.

type kLine struct {
	OrderItemID int64  `json:"order_item_id"`
	Name        string `json:"name"`
	Qty         int32  `json:"qty"`
	ChangeType  string `json:"change_type"`
	Modifiers   string `json:"modifiers"`
	Note        string `json:"note"`
}

type kTicket struct {
	ID             int64   `json:"id"`
	OrderID        int64   `json:"order_id"`
	OrderNo        int64   `json:"order_no"`
	BusinessDate   string  `json:"business_date"`
	TicketNo       int64   `json:"ticket_no"`
	Kind           string  `json:"kind"`
	ParentTicketID *int64  `json:"parent_ticket_id"`
	Status         string  `json:"status"`
	Lines          []kLine `json:"lines"`
	History        []struct {
		From string `json:"from_status"`
		To   string `json:"to_status"`
	} `json:"history"`
}

type kOrder struct {
	ID      int64  `json:"id"`
	Status  string `json:"status"`
	Version int32  `json:"version"`
	Items   []struct {
		ID            int64  `json:"id"`
		Qty           int32  `json:"qty"`
		Status        string `json:"status"`
		SentToKitchen bool   `json:"sent_to_kitchen"`
	} `json:"items"`
}

type kitchenEnv struct {
	t                 *testing.T
	e                 *testEnv
	mgr, wait, cook   jar
	cash              jar
	tea, steak, extra int64
	bar               int64 // a product that is not routed to the kitchen
}

func newKitchenEnv(t *testing.T) *kitchenEnv {
	t.Helper()
	e := newEnv(t)
	e.addUser("mgr", false, "business_manager")
	e.addUser("waitera", false, "waiter")
	e.addUser("cook", false, "kitchen_staff")
	e.addUser("cash", false, "cashier")
	k := &kitchenEnv{t: t, e: e, mgr: e.mustLogin("mgr"), wait: e.mustLogin("waitera"), cook: e.mustLogin("cook"), cash: e.mustLogin("cash")}
	k.tea, k.steak, k.extra = menuFixture(t, e, k.mgr)
	var standard int64
	for _, r := range e.taxRates(k.mgr) {
		if r.Treatment == "standard" {
			standard = r.ID
		}
	}
	w := e.do(req{method: "POST", path: "/api/v1/categories", jar: k.mgr, body: map[string]any{"module": "bar", "name": "Soft drinks"}})
	expect(t, w, 201, "")
	cat := decodeInto[struct {
		Category catItem `json:"category"`
	}](t, w).Category
	w = e.do(req{method: "POST", path: "/api/v1/products", jar: k.mgr, body: map[string]any{"category_id": cat.ID, "name": "Soda", "price_cents": 12000, "tax_rate_id": standard, "kitchen_routed": false}})
	expect(t, w, 201, "")
	k.bar = decodeInto[struct {
		Product catProduct `json:"product"`
	}](t, w).Product.ID
	return k
}

func (k *kitchenEnv) open() int64 {
	k.t.Helper()
	w := k.e.do(req{method: "POST", path: "/api/v1/orders", jar: k.wait, body: map[string]any{"order_type": "takeaway"}})
	expect(k.t, w, 201, "")
	return decodeInto[struct {
		Order kOrder `json:"order"`
	}](k.t, w).Order.ID
}

// add puts a line on the order and returns the new line's id.
func (k *kitchenEnv) add(order, product int64, qty int, note string, mods ...int64) int64 {
	k.t.Helper()
	body := map[string]any{"product_id": product, "qty": qty, "note": note}
	if len(mods) > 0 {
		body["modifier_ids"] = mods
	}
	w := k.e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(order) + "/items", jar: k.wait, body: body})
	expect(k.t, w, 201, "")
	o := decodeInto[struct {
		Order kOrder `json:"order"`
	}](k.t, w).Order
	return o.Items[len(o.Items)-1].ID
}

func (k *kitchenEnv) send(order int64) *httptest.ResponseRecorder {
	return k.e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(order) + "/send-to-kitchen", jar: k.wait})
}

func (k *kitchenEnv) mustSend(order int64) kTicket {
	k.t.Helper()
	w := k.send(order)
	expect(k.t, w, 201, "")
	return decodeInto[struct {
		Ticket kTicket `json:"ticket"`
	}](k.t, w).Ticket
}

func (k *kitchenEnv) order(order int64) kOrder {
	k.t.Helper()
	w := k.e.do(req{method: "GET", path: "/api/v1/orders/" + itoa(order), jar: k.wait})
	expect(k.t, w, 200, "")
	return decodeInto[struct {
		Order kOrder `json:"order"`
	}](k.t, w).Order
}

func (k *kitchenEnv) tickets(query string) []kTicket {
	k.t.Helper()
	w := k.e.do(req{method: "GET", path: "/api/v1/kitchen/tickets" + query, jar: k.cook})
	expect(k.t, w, 200, "")
	return decodeInto[struct {
		Items []kTicket `json:"items"`
	}](k.t, w).Items
}

func (k *kitchenEnv) ticket(id int64) kTicket {
	k.t.Helper()
	w := k.e.do(req{method: "GET", path: "/api/v1/kitchen/tickets/" + itoa(id), jar: k.cook})
	expect(k.t, w, 200, "")
	return decodeInto[struct {
		Ticket kTicket `json:"ticket"`
	}](k.t, w).Ticket
}

func (k *kitchenEnv) setStatus(j jar, id int64, status string) *httptest.ResponseRecorder {
	return k.e.do(req{method: "POST", path: "/api/v1/kitchen/tickets/" + itoa(id) + "/status", jar: j, body: map[string]any{"status": status}})
}

func (k *kitchenEnv) count(sql string, args ...any) int {
	k.t.Helper()
	var n int
	if err := k.e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		k.t.Fatal(err)
	}
	return n
}

// The ticket is stored, with its number, lines and first history row, by the time the send call answers. Nothing in
// this stage prints, so the guarantee tested is that the stored ticket matches what the caller was given and that a
// send that fails or has nothing to send uses no number (AC-04, NN-05).
func TestAC04_TicketPersistedBeforePrint(t *testing.T) {
	k := newKitchenEnv(t)
	order := k.open()
	steakLine := k.add(order, k.steak, 2, "medium rare", k.extra)
	k.add(order, k.tea, 1, "")
	k.add(order, k.bar, 3, "") // a drink for the bar: never on a kitchen ticket

	before := time.Now().In(nairobi(t)).Format("2006-01-02")
	tk := k.mustSend(order)
	after := time.Now().In(nairobi(t)).Format("2006-01-02")
	if tk.TicketNo != 1 || tk.Kind != "original" || tk.ParentTicketID != nil || tk.Status != "submitted" || len(tk.Lines) != 2 {
		t.Fatalf("ticket = %+v", tk)
	}
	if tk.BusinessDate != before && tk.BusinessDate != after {
		t.Fatalf("business date = %s, want today in Nairobi (%s)", tk.BusinessDate, before)
	}

	// What is stored equals what was returned, with the name and modifiers copied at send time and no money.
	var stored, withMods int
	stored = k.count(`SELECT count(*) FROM kitchen_tickets WHERE id = $1 AND order_id = $2 AND ticket_no = 1 AND kind = 'original' AND status = 'submitted'`, tk.ID, order)
	withMods = k.count(`SELECT count(*) FROM kitchen_ticket_items WHERE ticket_id = $1 AND order_item_id = $2 AND name_snapshot = 'Steak' AND qty = 2 AND modifiers_text = 'Extra chilli' AND note = 'medium rare' AND change_type = 'add'`, tk.ID, steakLine)
	if stored != 1 || withMods != 1 {
		t.Fatalf("stored ticket rows = %d, steak line rows = %d", stored, withMods)
	}
	if n := k.count(`SELECT count(*) FROM information_schema.columns WHERE table_name IN ('kitchen_tickets', 'kitchen_ticket_items') AND column_name LIKE '%cents%'`); n != 0 {
		t.Fatalf("a kitchen ticket table has %d money columns", n)
	}
	if n := k.count(`SELECT count(*) FROM ticket_status_history WHERE ticket_id = $1 AND from_status IS NULL AND to_status = 'submitted'`, tk.ID); n != 1 {
		t.Fatalf("first history rows = %d", n)
	}
	if n := k.count(`SELECT count(*) FROM audit_logs WHERE action = 'order.sent_to_kitchen' AND entity_id = $1`, itoa(order)); n != 1 {
		t.Fatalf("send audit rows = %d", n)
	}
	o := k.order(order)
	if o.Status != "sent" {
		t.Fatalf("order status = %s", o.Status)
	}
	flags := map[int64]bool{}
	for _, it := range o.Items {
		flags[it.ID] = it.SentToKitchen
	}
	if !flags[steakLine] || len(flags) != 3 {
		t.Fatalf("sent flags = %v", flags)
	}

	// A second tap has nothing new to send and must not use up a ticket number.
	expect(t, k.send(order), 409, "nothing_to_send")
	k.add(order, k.tea, 1, "second chai")
	tk2 := k.mustSend(order)
	if tk2.TicketNo != 2 {
		t.Fatalf("after a refused send the next ticket is number %d, want 2", tk2.TicketNo)
	}

	// An order with only bar items has nothing for the kitchen, and a missing order is not found.
	barOnly := k.open()
	k.add(barOnly, k.bar, 1, "")
	expect(t, k.send(barOnly), 409, "nothing_to_send")
	if k.order(barOnly).Status != "open" {
		t.Fatal("a refused send must leave the order open")
	}
	expect(t, k.e.do(req{method: "POST", path: "/api/v1/orders/99999/send-to-kitchen", jar: k.wait}), 404, "not_found")
	expect(t, k.e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(order) + "/send-to-kitchen", jar: k.cook}), 403, "forbidden")

	// Two taps at the same moment: one ticket.
	dup := k.open()
	k.add(dup, k.tea, 1, "")
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- k.send(dup).Code }()
	}
	wg.Wait()
	close(codes)
	var got []int
	for c := range codes {
		got = append(got, c)
	}
	sort.Ints(got)
	if got[0] != 201 || got[1] != 409 {
		t.Fatalf("two simultaneous sends answered %v, want one 201 and one 409", got)
	}
	if n := k.count(`SELECT count(*) FROM kitchen_tickets WHERE order_id = $1`, dup); n != 1 {
		t.Fatalf("tickets for the double-tapped order = %d", n)
	}
}

func nairobi(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Africa/Nairobi")
	if err != nil {
		t.Skip("no tz database: ", err)
	}
	return loc
}

// Ticket numbers are gapless and distinct when many tills send at once, and a rolled-back send gives its number back.
func TestAC04_TicketNumbersGaplessUnderConcurrency(t *testing.T) {
	k := newKitchenEnv(t)
	const n = 8
	orders := make([]int64, n)
	for i := range orders {
		orders[i] = k.open()
		k.add(orders[i], k.tea, 1, "")
	}
	var wg sync.WaitGroup
	nums := make(chan int64, n)
	for _, o := range orders {
		wg.Add(1)
		go func(o int64) {
			defer wg.Done()
			w := k.send(o)
			if w.Code != 201 {
				t.Errorf("send = %d %s", w.Code, w.Body.String())
				return
			}
			nums <- decodeInto[struct {
				Ticket kTicket `json:"ticket"`
			}](t, w).Ticket.TicketNo
		}(o)
	}
	wg.Wait()
	close(nums)
	var got []int64
	for v := range nums {
		got = append(got, v)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	for i, v := range got {
		if v != int64(i+1) {
			t.Fatalf("ticket numbers = %v, want 1..%d with no gap or repeat", got, n)
		}
	}
}

// Ticket status is the kitchen's own: it moves forward only, skipping is allowed (S-17), and it never reads the order's
// payment state (AC-04). The sale and payment tables arrive in Stage 11; the order here is still unpaid at every step,
// and Stage 11 repeats this against a finalised and a paid sale.
func TestAC04_TicketStatusIndependentOfPayment(t *testing.T) {
	k := newKitchenEnv(t)
	order := k.open()
	k.add(order, k.tea, 1, "")
	tk := k.mustSend(order)
	version := k.order(order).Version

	// Permissions: the cashier may look but not move a ticket; the waiter and the kitchen may.
	expect(t, k.e.do(req{method: "GET", path: "/api/v1/kitchen/tickets", jar: k.cash}), 200, "")
	expect(t, k.setStatus(k.cash, tk.ID, "preparing"), 403, "forbidden")
	expect(t, k.setStatus(k.mgr, 99999, "preparing"), 404, "not_found")

	// What may be asked for.
	expect(t, k.setStatus(k.cook, tk.ID, ""), 400, "validation_failed")
	expect(t, k.setStatus(k.cook, tk.ID, "burnt"), 400, "validation_failed")
	expect(t, k.setStatus(k.cook, tk.ID, "cancelled"), 400, "validation_failed") // only by cancelling the order
	expect(t, k.setStatus(k.cook, tk.ID, "submitted"), 400, "validation_failed")

	expect(t, k.setStatus(k.cook, tk.ID, "preparing"), 200, "")
	expect(t, k.setStatus(k.cook, tk.ID, "preparing"), 409, "invalid_transition") // not the same status twice
	expect(t, k.setStatus(k.wait, tk.ID, "ready"), 200, "")
	expect(t, k.setStatus(k.cook, tk.ID, "preparing"), 409, "invalid_transition") // never backwards
	expect(t, k.setStatus(k.cook, tk.ID, "served"), 200, "")
	expect(t, k.setStatus(k.cook, tk.ID, "ready"), 409, "invalid_transition") // served is final

	got := k.ticket(tk.ID)
	if got.Status != "served" || len(got.History) != 4 || got.History[0].To != "submitted" || got.History[3].From != "ready" || got.History[3].To != "served" {
		t.Fatalf("ticket history = %+v", got)
	}
	// The order itself did not move: ticket changes neither finalise, close nor re-version it.
	if o := k.order(order); o.Status != "sent" || o.Version != version {
		t.Fatalf("order after ticket changes = %+v (version was %d)", o, version)
	}

	// Skipping forward is allowed for a paper kitchen.
	o2 := k.open()
	k.add(o2, k.tea, 1, "")
	skip := k.mustSend(o2)
	expect(t, k.setStatus(k.cook, skip.ID, "served"), 200, "")

	// The database refuses what the service would: going backwards, editing a number, deleting, editing lines.
	ctx := context.Background()
	for _, sql := range []string{
		`UPDATE kitchen_tickets SET status = 'preparing' WHERE id = ` + itoa(tk.ID),
		`UPDATE kitchen_tickets SET ticket_no = 99 WHERE id = ` + itoa(tk.ID),
		`DELETE FROM kitchen_tickets WHERE id = ` + itoa(tk.ID),
		`UPDATE kitchen_ticket_items SET qty = 5 WHERE ticket_id = ` + itoa(tk.ID),
		`DELETE FROM kitchen_ticket_items WHERE ticket_id = ` + itoa(tk.ID),
		`UPDATE ticket_status_history SET to_status = 'ready' WHERE ticket_id = ` + itoa(tk.ID),
		`DELETE FROM ticket_status_history WHERE ticket_id = ` + itoa(tk.ID),
		`TRUNCATE kitchen_tickets CASCADE`,
	} {
		if _, err := k.e.pool.Exec(ctx, sql); err == nil {
			t.Errorf("the database accepted: %s", sql)
		}
	}
}

// A change to an order the kitchen already has never edits the old ticket: it goes out as a new update ticket that
// points at the original.
func TestAC04_ChangesCreateLinkedUpdateTickets(t *testing.T) {
	k := newKitchenEnv(t)
	order := k.open()
	steakLine := k.add(order, k.steak, 2, "", k.extra)
	teaLine := k.add(order, k.tea, 1, "")
	orig := k.mustSend(order)

	// A line added later goes out as an update ticket holding only that line.
	late := k.add(order, k.tea, 4, "for the kids")
	upd := k.mustSend(order)
	if upd.Kind != "update" || upd.ParentTicketID == nil || *upd.ParentTicketID != orig.ID || upd.TicketNo != 2 || len(upd.Lines) != 1 ||
		upd.Lines[0].OrderItemID != late || upd.Lines[0].ChangeType != "add" || upd.Lines[0].Qty != 4 || upd.Lines[0].Note != "for the kids" {
		t.Fatalf("update ticket = %+v", upd)
	}
	if k.order(order).Status != "sent" {
		t.Fatal("order should stay sent")
	}

	// Changing a line the kitchen holds raises a modify ticket with the new quantity; the old ticket is untouched.
	w := k.e.do(req{method: "PATCH", path: "/api/v1/orders/" + itoa(order) + "/items/" + itoa(steakLine), jar: k.wait, body: map[string]any{"qty": 3, "note": "no onions"}})
	expect(t, w, 200, "")
	all := k.tickets("?order_id=" + itoa(order))
	if len(all) != 3 {
		t.Fatalf("tickets after a change = %d, want 3", len(all))
	}
	mod := all[2]
	if mod.Kind != "update" || mod.ParentTicketID == nil || *mod.ParentTicketID != orig.ID || len(mod.Lines) != 1 ||
		mod.Lines[0].OrderItemID != steakLine || mod.Lines[0].ChangeType != "modify" || mod.Lines[0].Qty != 3 || mod.Lines[0].Note != "no onions" || mod.Lines[0].Modifiers != "Extra chilli" {
		t.Fatalf("modify ticket = %+v", mod)
	}
	parent := k.ticket(orig.ID)
	if len(parent.Lines) != 2 || parent.Lines[0].Qty != 2 || parent.Lines[0].ChangeType != "add" || parent.Status != "submitted" {
		t.Fatalf("the original ticket was edited: %+v", parent)
	}

	// Changing nothing, or changing a line that was never sent, raises no ticket.
	expect(t, k.e.do(req{method: "PATCH", path: "/api/v1/orders/" + itoa(order) + "/items/" + itoa(steakLine), jar: k.wait, body: map[string]any{"qty": 3}}), 200, "")
	unsent := k.add(order, k.tea, 1, "")
	expect(t, k.e.do(req{method: "PATCH", path: "/api/v1/orders/" + itoa(order) + "/items/" + itoa(unsent), jar: k.wait, body: map[string]any{"qty": 2}}), 200, "")
	if n := len(k.tickets("?order_id=" + itoa(order))); n != 3 {
		t.Fatalf("tickets after no-op and unsent changes = %d, want 3", n)
	}

	// The database keeps the links honest: an update needs a parent, a parent must be of the same order, and a line
	// goes to the kitchen once.
	ctx := context.Background()
	var uid int64 // a real user, so a refusal below comes from the ticket rules and not from a missing foreign key
	if err := k.e.pool.QueryRow(ctx, `SELECT id FROM users ORDER BY id LIMIT 1`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	other := k.open()
	k.add(other, k.tea, 1, "")
	otherTicket := k.mustSend(other)
	for _, sql := range []string{
		`INSERT INTO kitchen_tickets (order_id, business_date, ticket_no, kind, created_by) VALUES (` + itoa(order) + `, '2001-01-01', 1, 'update', ` + itoa(uid) + `)`,
		`INSERT INTO kitchen_tickets (order_id, business_date, ticket_no, kind, parent_ticket_id, created_by) VALUES (` + itoa(order) + `, '2001-01-01', 2, 'update', ` + itoa(otherTicket.ID) + `, ` + itoa(uid) + `)`,
		`INSERT INTO kitchen_tickets (order_id, business_date, ticket_no, kind, parent_ticket_id, created_by) VALUES (` + itoa(order) + `, '2001-01-01', 3, 'original', ` + itoa(orig.ID) + `, ` + itoa(uid) + `)`,
		`INSERT INTO kitchen_tickets (order_id, business_date, ticket_no, kind, created_by) VALUES (` + itoa(order) + `, '2001-01-01', 4, 'original', ` + itoa(uid) + `)`,     // a second original
		`INSERT INTO kitchen_ticket_items (ticket_id, order_item_id, name_snapshot, qty, change_type) VALUES (` + itoa(upd.ID) + `, ` + itoa(teaLine) + `, 'Chai', 1, 'add')`, // already added on the original
	} {
		if _, err := k.e.pool.Exec(ctx, sql); err == nil {
			t.Errorf("the database accepted: %s", sql)
		}
	}
}

// Cancelling after send tells the kitchen with a cancel ticket, and cancelling a whole sent order closes what the
// kitchen still had to do.
func TestAC04_CancelAfterSendCreatesCancelTicket(t *testing.T) {
	k := newKitchenEnv(t)
	order := k.open()
	steakLine := k.add(order, k.steak, 2, "", k.extra)
	k.add(order, k.tea, 1, "")
	orig := k.mustSend(order)

	// One sent line cancelled: a cancel ticket for that line only.
	cancelLine := func(line int64, reason string) *httptest.ResponseRecorder {
		return k.e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(order) + "/items/" + itoa(line) + "/cancel", jar: k.wait, body: map[string]any{"reason": reason}})
	}
	expect(t, cancelLine(steakLine, ""), 400, "validation_failed")
	expect(t, cancelLine(steakLine, "guest changed their mind"), 200, "")
	all := k.tickets("?order_id=" + itoa(order))
	if len(all) != 2 {
		t.Fatalf("tickets after cancelling a sent line = %d, want 2", len(all))
	}
	c := all[1]
	if c.Kind != "update" || c.ParentTicketID == nil || *c.ParentTicketID != orig.ID || len(c.Lines) != 1 ||
		c.Lines[0].OrderItemID != steakLine || c.Lines[0].ChangeType != "cancel" || c.Lines[0].Qty != 2 || c.Lines[0].Name != "Steak" {
		t.Fatalf("cancel ticket = %+v", c)
	}
	expect(t, cancelLine(steakLine, "again"), 409, "item_cancelled")
	if n := len(k.tickets("?order_id=" + itoa(order))); n != 2 {
		t.Fatalf("a refused second cancel raised a ticket: %d", n)
	}

	// A line that was never sent is cancelled with no ticket.
	unsent := k.add(order, k.tea, 1, "")
	expect(t, cancelLine(unsent, "typo"), 200, "")
	if n := len(k.tickets("?order_id=" + itoa(order))); n != 2 {
		t.Fatalf("cancelling an unsent line raised a ticket: %d", n)
	}

	// Cancelling the whole sent order: the kitchen gets a cancel ticket for what it still holds (the chai), the
	// tickets still to do are cancelled, and a served ticket is left as it was.
	expect(t, k.setStatus(k.cook, orig.ID, "preparing"), 200, "")
	k.add(order, k.tea, 2, "")
	more := k.mustSend(order)
	expect(t, k.setStatus(k.cook, more.ID, "served"), 200, "")
	w := k.e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(order) + "/cancel", jar: k.wait, body: map[string]any{"reason": "party left"}})
	expect(t, w, 200, "")
	if o := k.order(order); o.Status != "cancelled" {
		t.Fatalf("order status = %s", o.Status)
	}
	all = k.tickets("?status=all&order_id=" + itoa(order))
	status := map[int64]string{}
	var last kTicket
	for _, tk := range all {
		status[tk.ID] = tk.Status
		last = tk
	}
	if status[orig.ID] != "cancelled" || status[more.ID] != "served" {
		t.Fatalf("ticket statuses after cancelling the order = %v", status)
	}
	if last.Kind != "update" || last.Status != "submitted" || len(last.Lines) != 2 || last.Lines[0].ChangeType != "cancel" || last.Lines[1].ChangeType != "cancel" {
		t.Fatalf("order cancel ticket = %+v", last)
	}
	hist := k.ticket(orig.ID).History
	if len(hist) != 3 || hist[2].From != "preparing" || hist[2].To != "cancelled" {
		t.Fatalf("history of the ticket closed by the cancellation = %+v", hist)
	}
	if n := k.count(`SELECT count(*) FROM audit_logs WHERE action = 'order.cancelled' AND entity_id = $1`, itoa(order)); n != 1 {
		t.Fatalf("order cancel audit rows = %d", n)
	}
	// A cancelled order accepts nothing more, and a cancelled ticket cannot be moved.
	expect(t, k.send(order), 409, "order_closed")
	expect(t, k.setStatus(k.cook, orig.ID, "served"), 409, "invalid_transition")

	// An open order that never reached the kitchen cancels with no ticket at all.
	plain := k.open()
	k.add(plain, k.tea, 1, "")
	expect(t, k.e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(plain) + "/cancel", jar: k.wait, body: map[string]any{"reason": "walked away"}}), 200, "")
	if n := k.count(`SELECT count(*) FROM kitchen_tickets WHERE order_id = $1`, plain); n != 0 {
		t.Fatalf("an unsent order cancelled with %d tickets", n)
	}
}

// Reading tickets: the kitchen sees what is still to do, oldest first, and can page and filter.
func TestKitchenTicketList(t *testing.T) {
	k := newKitchenEnv(t)
	var ids []int64
	for i := 0; i < 3; i++ {
		o := k.open()
		k.add(o, k.tea, 1, "")
		ids = append(ids, k.mustSend(o).ID)
	}
	expect(t, k.setStatus(k.cook, ids[0], "served"), 200, "")

	active := k.tickets("")
	if len(active) != 2 || active[0].ID != ids[1] || active[1].ID != ids[2] {
		t.Fatalf("active tickets = %+v", active)
	}
	if n := len(k.tickets("?status=all")); n != 3 {
		t.Fatalf("all tickets = %d", n)
	}
	if got := k.tickets("?status=served"); len(got) != 1 || got[0].ID != ids[0] {
		t.Fatalf("served tickets = %+v", got)
	}
	// Paging: limit 1 gives a cursor that continues after the last ticket seen.
	w := k.e.do(req{method: "GET", path: "/api/v1/kitchen/tickets?status=all&limit=1", jar: k.cook})
	expect(t, w, 200, "")
	page := decodeInto[struct {
		Items      []kTicket `json:"items"`
		NextCursor string    `json:"next_cursor"`
	}](t, w)
	if len(page.Items) != 1 || page.Items[0].ID != ids[0] || page.NextCursor != itoa(ids[0]) {
		t.Fatalf("first page = %+v", page)
	}
	w = k.e.do(req{method: "GET", path: "/api/v1/kitchen/tickets?status=all&limit=2&cursor=" + page.NextCursor, jar: k.cook})
	expect(t, w, 200, "")
	rest := decodeInto[struct {
		Items      []kTicket `json:"items"`
		NextCursor string    `json:"next_cursor"`
	}](t, w)
	if len(rest.Items) != 2 || rest.Items[0].ID != ids[1] || rest.NextCursor != "" {
		t.Fatalf("second page = %+v", rest)
	}
	expect(t, k.e.do(req{method: "GET", path: "/api/v1/kitchen/tickets?status=burnt", jar: k.cook}), 400, "validation_failed")
	expect(t, k.e.do(req{method: "GET", path: "/api/v1/kitchen/tickets?order_id=abc", jar: k.cook}), 400, "validation_failed")
	expect(t, k.e.do(req{method: "GET", path: "/api/v1/kitchen/tickets/abc", jar: k.cook}), 404, "not_found")
	expect(t, k.e.do(req{method: "GET", path: "/api/v1/kitchen/tickets", jar: k.mgr}), 200, "")
	// A role without kitchen.view (the auditor) cannot read tickets.
	k.e.addUser("auditor", false, "accountant_auditor")
	expect(t, k.e.do(req{method: "GET", path: "/api/v1/kitchen/tickets", jar: k.e.mustLogin("auditor")}), 403, "forbidden")
}
