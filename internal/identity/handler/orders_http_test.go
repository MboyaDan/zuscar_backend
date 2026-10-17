package handler_test

import (
	"context"
	"sort"
	"sync"
	"testing"
)

type tableItem struct {
	ID          int64  `json:"id"`
	Label       string `json:"label"`
	Active      bool   `json:"active"`
	OpenOrderID *int64 `json:"open_order_id"`
}

type orderLine struct {
	ID             int64  `json:"id"`
	Qty            int32  `json:"qty"`
	UnitPriceCents int64  `json:"unit_price_cents"`
	LineTotalCents int64  `json:"line_total_cents"`
	Status         string `json:"status"`
	Note           string `json:"note"`
}

type orderResp struct {
	Order struct {
		ID            int64       `json:"id"`
		OrderNo       int64       `json:"order_no"`
		OrderType     string      `json:"order_type"`
		Status        string      `json:"status"`
		Version       int32       `json:"version"`
		SubtotalCents int64       `json:"subtotal_cents"`
		ItemCount     int32       `json:"item_count"`
		Items         []orderLine `json:"items"`
	} `json:"order"`
}

type orderList struct {
	Items []struct {
		ID      int64 `json:"id"`
		OrderNo int64 `json:"order_no"`
	} `json:"items"`
	NextCursor string `json:"next_cursor"`
}

// menuFixture adds a category, two products and a modifier through the API and returns their ids.
func menuFixture(t *testing.T, e *testEnv, mj jar) (tea, steak, extra int64) {
	t.Helper()
	var standard int64
	for _, r := range e.taxRates(mj) {
		if r.Treatment == "standard" {
			standard = r.ID
		}
	}
	w := e.do(req{method: "POST", path: "/api/v1/categories", jar: mj, body: map[string]any{"module": "restaurant", "name": "Mains"}})
	expect(t, w, 201, "")
	cat := decodeInto[struct {
		Category catItem `json:"category"`
	}](t, w).Category
	mk := func(name string, cents int64, mods ...int64) int64 {
		w := e.do(req{method: "POST", path: "/api/v1/products", jar: mj, body: map[string]any{"category_id": cat.ID, "name": name, "price_cents": cents, "tax_rate_id": standard, "kitchen_routed": true, "modifier_ids": mods}})
		expect(t, w, 201, "")
		return decodeInto[struct {
			Product catProduct `json:"product"`
		}](t, w).Product.ID
	}
	w = e.do(req{method: "POST", path: "/api/v1/modifiers", jar: mj, body: map[string]any{"name": "Extra chilli", "price_delta_cents": 5000}})
	expect(t, w, 201, "")
	extra = decodeInto[struct {
		Modifier struct {
			ID int64 `json:"id"`
		} `json:"modifier"`
	}](t, w).Modifier.ID
	tea = mk("Chai", 8000)
	steak = mk("Steak", 85000, extra)
	return tea, steak, extra
}

func TestTablesAndOrdersLifecycle(t *testing.T) {
	e := newEnv(t)
	e.addUser("mgr", false, "business_manager")
	e.addUser("waitera", false, "waiter")
	e.addUser("waiterb", false, "waiter")
	e.addUser("cook", false, "kitchen_staff")
	e.addUser("auditor", false, "accountant_auditor")
	mj, wa, wb, kj, aj := e.mustLogin("mgr"), e.mustLogin("waitera"), e.mustLogin("waiterb"), e.mustLogin("cook"), e.mustLogin("auditor")
	tea, steak, extra := menuFixture(t, e, mj)

	// Permissions: tables are managed with catalogue.manage, orders need orders.manage.
	expect(t, e.do(req{method: "GET", path: "/api/v1/tables", jar: kj}), 403, "forbidden")
	expect(t, e.do(req{method: "GET", path: "/api/v1/orders", jar: aj}), 403, "forbidden")
	expect(t, e.do(req{method: "POST", path: "/api/v1/tables", jar: wa, body: map[string]any{"label": "T1", "seats": 4}}), 403, "forbidden")

	// Tables.
	expect(t, e.do(req{method: "POST", path: "/api/v1/tables", jar: mj, body: map[string]any{"label": "T1"}}), 400, "validation_failed")
	expect(t, e.do(req{method: "POST", path: "/api/v1/tables", jar: mj, body: map[string]any{"label": "T1", "seats": 0}}), 400, "validation_failed")
	w := e.do(req{method: "POST", path: "/api/v1/tables", jar: mj, body: map[string]any{"label": "T1", "seats": 4}})
	expect(t, w, 201, "")
	t1 := decodeInto[struct {
		Table tableItem `json:"table"`
	}](t, w).Table
	expect(t, e.do(req{method: "POST", path: "/api/v1/tables", jar: mj, body: map[string]any{"label": "T1", "seats": 2}}), 409, "conflict")
	w = e.do(req{method: "POST", path: "/api/v1/tables", jar: mj, body: map[string]any{"label": "T2", "seats": 2}})
	t2 := decodeInto[struct {
		Table tableItem `json:"table"`
	}](t, w).Table
	expect(t, e.do(req{method: "PATCH", path: "/api/v1/tables/" + itoa(t2.ID), jar: mj, body: map[string]any{"active": false}}), 200, "")

	// Opening orders: the table rules and the gapless order number.
	post := func(j jar, body map[string]any) *orderResp {
		w := e.do(req{method: "POST", path: "/api/v1/orders", jar: j, body: body})
		expect(t, w, 201, "")
		r := decodeInto[orderResp](t, w)
		return &r
	}
	expect(t, e.do(req{method: "POST", path: "/api/v1/orders", jar: wa, body: map[string]any{"order_type": "dine_in"}}), 400, "validation_failed")
	expect(t, e.do(req{method: "POST", path: "/api/v1/orders", jar: wa, body: map[string]any{"order_type": "takeaway", "table_id": t1.ID}}), 400, "validation_failed")
	expect(t, e.do(req{method: "POST", path: "/api/v1/orders", jar: wa, body: map[string]any{"order_type": "dine_in", "table_id": t2.ID}}), 400, "validation_failed") // inactive table
	expect(t, e.do(req{method: "POST", path: "/api/v1/orders", jar: wa, body: map[string]any{"order_type": "eat_in"}}), 400, "validation_failed")
	first := post(wa, map[string]any{"order_type": "dine_in", "table_id": t1.ID})
	second := post(wb, map[string]any{"order_type": "takeaway"})
	if first.Order.OrderNo != 1 || second.Order.OrderNo != 2 || first.Order.Status != "open" || first.Order.Version != 1 {
		t.Fatalf("orders = %+v / %+v", first.Order, second.Order)
	}
	w = e.do(req{method: "GET", path: "/api/v1/tables", jar: wa})
	expect(t, w, 200, "")
	for _, tb := range decodeInto[struct {
		Items []tableItem `json:"items"`
	}](t, w).Items {
		if tb.ID == t1.ID && (tb.OpenOrderID == nil || *tb.OpenOrderID != first.Order.ID) {
			t.Fatalf("table T1 should show its open order: %+v", tb)
		}
	}

	// Many tills opening orders at once still get distinct, gapless numbers.
	var wg sync.WaitGroup
	nums := make(chan int64, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := e.do(req{method: "POST", path: "/api/v1/orders", jar: wa, body: map[string]any{"order_type": "takeaway"}})
			if w.Code == 201 {
				nums <- decodeInto[orderResp](t, w).Order.OrderNo
			}
		}()
	}
	wg.Wait()
	close(nums)
	var got []int64
	for n := range nums {
		got = append(got, n)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != 10 {
		t.Fatalf("only %d of 10 concurrent orders were created", len(got))
	}
	for i, n := range got {
		if n != int64(i+3) {
			t.Fatalf("order numbers = %v, want 3..12 with no gaps or repeats", got)
		}
	}

	// Items: the server sets the price; a client-supplied price is refused.
	base := "/api/v1/orders/" + itoa(first.Order.ID)
	expect(t, e.do(req{method: "POST", path: base + "/items", jar: wa, body: map[string]any{"product_id": tea, "qty": 1, "unit_price_cents": 1}}), 400, "validation_failed")
	expect(t, e.do(req{method: "POST", path: base + "/items", jar: wa, body: map[string]any{"product_id": tea}}), 400, "validation_failed")
	expect(t, e.do(req{method: "POST", path: base + "/items", jar: wa, body: map[string]any{"product_id": tea, "qty": 0}}), 400, "validation_failed")
	expect(t, e.do(req{method: "POST", path: base + "/items", jar: wa, body: map[string]any{"product_id": 99999, "qty": 1}}), 400, "validation_failed")
	expect(t, e.do(req{method: "POST", path: base + "/items", jar: wa, body: map[string]any{"product_id": tea, "qty": 1, "modifier_ids": []int64{extra}}}), 400, "validation_failed") // chai does not offer it
	w = e.do(req{method: "POST", path: base + "/items", jar: wa, body: map[string]any{"product_id": steak, "qty": 2, "note": "medium", "modifier_ids": []int64{extra}}})
	expect(t, w, 201, "")
	o := decodeInto[orderResp](t, w).Order
	if len(o.Items) != 1 || o.Items[0].UnitPriceCents != 85000 || o.Items[0].LineTotalCents != (85000+5000)*2 || o.SubtotalCents != 180000 || o.Version != 2 {
		t.Fatalf("after add: %+v", o)
	}
	steakLine := o.Items[0].ID

	// The price on the line is fixed when it was added.
	expect(t, e.do(req{method: "PUT", path: "/api/v1/products/" + itoa(steak) + "/price", jar: mj, body: map[string]any{"price_cents": 99000}}), 200, "")
	w = e.do(req{method: "GET", path: base, jar: wa})
	expect(t, w, 200, "")
	if o := decodeInto[orderResp](t, w).Order; o.Items[0].UnitPriceCents != 85000 || o.SubtotalCents != 180000 {
		t.Fatalf("a price change must not alter an existing line: %+v", o)
	}

	// An unavailable product cannot be added.
	expect(t, e.do(req{method: "PUT", path: "/api/v1/products/" + itoa(tea) + "/availability", jar: mj, body: map[string]any{"is_available": false}}), 200, "")
	expect(t, e.do(req{method: "POST", path: base + "/items", jar: wa, body: map[string]any{"product_id": tea, "qty": 1}}), 409, "product_unavailable")
	expect(t, e.do(req{method: "PUT", path: "/api/v1/products/" + itoa(tea) + "/availability", jar: mj, body: map[string]any{"is_available": true}}), 200, "")

	// Optimistic version: a stale view is refused, the current one is accepted.
	expect(t, e.do(req{method: "PATCH", path: base + "/items/" + itoa(steakLine), jar: wa, body: map[string]any{"qty": 3, "expected_version": 1}}), 409, "order_changed")
	w = e.do(req{method: "PATCH", path: base + "/items/" + itoa(steakLine), jar: wa, body: map[string]any{"qty": 3, "expected_version": 2}})
	expect(t, w, 200, "")
	if o := decodeInto[orderResp](t, w).Order; o.SubtotalCents != 270000 || o.Items[0].Qty != 3 {
		t.Fatalf("after qty change: %+v", o)
	}
	expect(t, e.do(req{method: "PATCH", path: base + "/items/" + itoa(steakLine), jar: wa, body: map[string]any{}}), 400, "validation_failed")
	expect(t, e.do(req{method: "PATCH", path: base + "/items/99999", jar: wa, body: map[string]any{"qty": 1}}), 404, "not_found")

	// Who sees what: another waiter cannot see, list or change this order; the manager can see it.
	expect(t, e.do(req{method: "GET", path: base, jar: wb}), 404, "not_found")
	expect(t, e.do(req{method: "POST", path: base + "/items", jar: wb, body: map[string]any{"product_id": tea, "qty": 1}}), 404, "not_found")
	w = e.do(req{method: "GET", path: "/api/v1/orders?limit=100", jar: wb})
	expect(t, w, 200, "")
	for _, x := range decodeInto[orderList](t, w).Items {
		if x.ID == first.Order.ID {
			t.Fatal("a waiter's list must not include another waiter's order")
		}
	}
	w = e.do(req{method: "GET", path: "/api/v1/orders?limit=100", jar: mj})
	if l := decodeInto[orderList](t, w); len(l.Items) != 12 {
		t.Fatalf("manager sees %d orders, want 12", len(l.Items))
	}
	w = e.do(req{method: "GET", path: "/api/v1/orders?limit=5", jar: mj})
	if l := decodeInto[orderList](t, w); len(l.Items) != 5 || l.NextCursor == "" {
		t.Fatalf("paging: %+v", l)
	}
	expect(t, e.do(req{method: "GET", path: "/api/v1/orders?status=paid", jar: mj}), 400, "validation_failed")
	expect(t, e.do(req{method: "GET", path: base, jar: mj}), 200, "")

	// Cancelling a line needs a reason and drops it from the total.
	expect(t, e.do(req{method: "POST", path: base + "/items/" + itoa(steakLine) + "/cancel", jar: wa, body: map[string]any{}}), 400, "validation_failed")
	w = e.do(req{method: "POST", path: base + "/items/" + itoa(steakLine) + "/cancel", jar: wa, body: map[string]any{"reason": "guest changed mind"}})
	expect(t, w, 200, "")
	if o := decodeInto[orderResp](t, w).Order; o.SubtotalCents != 0 || o.ItemCount != 0 || o.Items[0].Status != "cancelled" {
		t.Fatalf("after cancel: %+v", o)
	}
	expect(t, e.do(req{method: "POST", path: base + "/items/" + itoa(steakLine) + "/cancel", jar: wa, body: map[string]any{"reason": "again"}}), 409, "item_cancelled")
	expect(t, e.do(req{method: "PATCH", path: base + "/items/" + itoa(steakLine), jar: wa, body: map[string]any{"qty": 1}}), 409, "item_cancelled")

	// Cancelling the order, and what a closed order refuses.
	expect(t, e.do(req{method: "POST", path: base + "/cancel", jar: wa, body: map[string]any{}}), 400, "validation_failed")
	w = e.do(req{method: "POST", path: base + "/cancel", jar: wa, body: map[string]any{"reason": "party left"}})
	expect(t, w, 200, "")
	if o := decodeInto[orderResp](t, w).Order; o.Status != "cancelled" {
		t.Fatalf("status = %s", o.Status)
	}
	expect(t, e.do(req{method: "POST", path: base + "/items", jar: wa, body: map[string]any{"product_id": tea, "qty": 1}}), 409, "order_closed")
	expect(t, e.do(req{method: "POST", path: base + "/cancel", jar: wa, body: map[string]any{"reason": "again"}}), 409, "order_closed")
	// The database refuses the same things even if the service were bypassed.
	if _, err := e.pool.Exec(ctx(), `UPDATE orders SET note = 'x' WHERE id = $1`, first.Order.ID); err == nil {
		t.Fatal("a cancelled order must not accept updates")
	}
	if _, err := e.pool.Exec(ctx(), `INSERT INTO order_items (order_id, product_id, qty, unit_price_cents) VALUES ($1, $2, 1, 100)`, first.Order.ID, tea); err == nil {
		t.Fatal("a cancelled order must not accept new items")
	}
	if _, err := e.pool.Exec(ctx(), `DELETE FROM orders WHERE id = $1`, second.Order.ID); err == nil {
		t.Fatal("orders must never be deleted")
	}
	if _, err := e.pool.Exec(ctx(), `UPDATE order_items SET unit_price_cents = 1 WHERE order_id = $1`, first.Order.ID); err == nil {
		t.Fatal("an item price must never change")
	}
	for _, a := range []string{"table.created", "table.updated", "order.created", "order.item_cancelled", "order.cancelled"} {
		if e.auditN(a) == 0 {
			t.Errorf("no audit row for %s", a)
		}
	}
}

func ctx() context.Context { return context.Background() }
