package handler_test

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
)

type saleResp struct {
	Sale struct {
		ID            int64  `json:"id"`
		ReceiptNo     int64  `json:"receipt_no"`
		OrderID       int64  `json:"order_id"`
		SubtotalCents int64  `json:"subtotal_cents"`
		DiscountCents int64  `json:"discount_cents"`
		TaxCents      int64  `json:"tax_cents"`
		TotalCents    int64  `json:"total_cents"`
		PaidCents     int64  `json:"paid_cents"`
		PendingCents  int64  `json:"pending_cents"`
		BalanceCents  int64  `json:"balance_cents"`
		PaymentStatus string `json:"payment_status"`
		TaxStatus     string `json:"tax_status"`
		Items         []struct {
			Name           string `json:"name"`
			Qty            int32  `json:"qty"`
			UnitPriceCents int64  `json:"unit_price_cents"`
			ModifiersCents int64  `json:"modifiers_cents"`
			DiscountCents  int64  `json:"discount_cents"`
			DiscountReason string `json:"discount_reason"`
			TaxCents       int64  `json:"tax_cents"`
			LineTotalCents int64  `json:"line_total_cents"`
		} `json:"items"`
		Payments []struct {
			Method      string `json:"method"`
			Status      string `json:"status"`
			AmountCents int64  `json:"amount_cents"`
			Reference   string `json:"reference"`
		} `json:"payments"`
	} `json:"sale"`
}

type billResp struct {
	Bill struct {
		TotalCents int64 `json:"total_cents"`
		TaxCents   int64 `json:"tax_cents"`
		Lines      []struct {
			OrderItemID    int64 `json:"order_item_id"`
			LineTotalCents int64 `json:"line_total_cents"`
			TaxCents       int64 `json:"tax_cents"`
		} `json:"lines"`
	} `json:"bill"`
}

func TestAC01_FinaliseSnapshotsCorrect(t *testing.T) {
	e := newEnv(t)
	for u, role := range map[string]string{"mgr": "business_manager", "till": "cashier", "waitera": "waiter", "waiterb": "waiter", "cook": "kitchen_staff", "barman": "bartender"} {
		e.addUser(u, false, role)
	}
	mj, tj, wa, wb, kj, bj := e.mustLogin("mgr"), e.mustLogin("till"), e.mustLogin("waitera"), e.mustLogin("waiterb"), e.mustLogin("cook"), e.mustLogin("barman")
	tea, steak, extra := menuFixture(t, e, mj)
	e.openShift(t, tj, 0)

	var standard, zero int64
	for _, r := range e.taxRates(mj) {
		switch r.Treatment {
		case "standard":
			standard = r.ID
		case "zero_rated":
			zero = r.ID
		}
	}
	e.exec(`UPDATE tax_rates SET rate_bp = 1600 WHERE id = $1`, standard) // the figures below assume 16.00%

	// A bar product and a zero-rated restaurant product.
	w := e.do(req{method: "POST", path: "/api/v1/categories", jar: mj, body: map[string]any{"module": "bar", "name": "Beer"}})
	expect(t, w, 201, "")
	barCat := decodeInto[struct {
		Category catItem `json:"category"`
	}](t, w).Category.ID
	mk := func(cat int64, name string, cents, tax int64) int64 {
		w := e.do(req{method: "POST", path: "/api/v1/products", jar: mj, body: map[string]any{"category_id": cat, "name": name, "price_cents": cents, "tax_rate_id": tax, "kitchen_routed": false}})
		expect(t, w, 201, "")
		return decodeInto[struct {
			Product catProduct `json:"product"`
		}](t, w).Product.ID
	}
	lager := mk(barCat, "Lager", 35000, standard)
	water := mk(int64(e.rows(`SELECT id FROM categories WHERE module = 'restaurant' LIMIT 1`)), "Bottled water", 6000, zero)

	newOrder := func(j jar) int64 {
		w := e.do(req{method: "POST", path: "/api/v1/orders", jar: j, body: map[string]any{"order_type": "takeaway"}})
		expect(t, w, 201, "")
		return decodeInto[orderResp](t, w).Order.ID
	}
	add := func(j jar, order, product int64, qty int, mods ...int64) int64 {
		w := e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(order) + "/items", jar: j, body: map[string]any{"product_id": product, "qty": qty, "modifier_ids": mods}})
		expect(t, w, 201, "")
		items := decodeInto[orderResp](t, w).Order.Items
		return items[len(items)-1].ID
	}
	finalise := func(j jar, order int64, key string, body map[string]any) *sendResp {
		h := map[string]string{}
		if key != "" {
			h["Idempotency-Key"] = key
		}
		return &sendResp{e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(order) + "/finalise", jar: j, header: h, body: body})}
	}
	nextReceipt := func() int64 {
		var n int64
		if err := e.pool.QueryRow(context.Background(), `SELECT next_value FROM document_counters WHERE doc_type = 'receipt' AND scope = ''`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// One order: 2 steaks with extra chilli (2 x 90 000), 1 chai (8 000), 1 water (6 000, zero-rated). It goes to the kitchen first.
	a := newOrder(wa)
	steakLine := add(wa, a, steak, 2, extra)
	teaLine := add(wa, a, tea, 1)
	waterLine := add(wa, a, water, 1)
	expect(t, e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(a) + "/send-to-kitchen", jar: wa, header: map[string]string{"Idempotency-Key": "send-for-sale-1"}, body: map[string]any{}}), 201, "")

	// The bill comes from the server's prices.
	expect(t, e.do(req{method: "GET", path: "/api/v1/orders/" + itoa(a) + "/bill", jar: wa}), 403, "forbidden") // a waiter does not take payment
	w = e.do(req{method: "GET", path: "/api/v1/orders/" + itoa(a) + "/bill", jar: tj})
	expect(t, w, 200, "")
	bill := decodeInto[billResp](t, w).Bill
	// 18 0000 at 16% -> 24 827.586 -> 24 828; 8 000 -> 1 103.448 -> 1 103; water zero-rated -> 0
	if bill.TotalCents != 194000 || bill.TaxCents != 24828+1103 || len(bill.Lines) != 3 {
		t.Fatalf("bill = %+v", bill)
	}

	// Who may finalise, and what a request must carry.
	good := map[string]any{"payments": []map[string]any{{"method": "cash", "amount_cents": 100000}, {"method": "mpesa_manual", "amount_cents": 94000, "reference": "qwe123abc"}}}
	expect(t, finalise(tj, a, "", good).ResponseRecorder, 400, "validation_failed") // the key is required
	expect(t, finalise(wa, a, "sale-key-0001", good).ResponseRecorder, 403, "forbidden")
	expect(t, finalise(kj, a, "sale-key-0001", good).ResponseRecorder, 403, "forbidden")
	disc := map[string]any{"discounts": []map[string]any{{"order_item_id": teaLine, "amount_cents": 1000, "reason": "regular"}}}
	expect(t, finalise(tj, a, "sale-key-0001", disc).ResponseRecorder, 403, "discount_not_permitted")
	// Payment rules.
	over := map[string]any{"payments": []map[string]any{{"method": "cash", "amount_cents": 194001}}}
	expect(t, finalise(tj, a, "sale-key-0002", over).ResponseRecorder, 400, "validation_failed")
	noRef := map[string]any{"payments": []map[string]any{{"method": "mpesa_manual", "amount_cents": 1000}}}
	expect(t, finalise(tj, a, "sale-key-0003", noRef).ResponseRecorder, 400, "validation_failed")
	badMethod := map[string]any{"payments": []map[string]any{{"method": "room_charge", "amount_cents": 1000}}}
	expect(t, finalise(tj, a, "sale-key-0004", badMethod).ResponseRecorder, 400, "validation_failed")
	expect(t, finalise(tj, a, "sale-key-0005", map[string]any{"total_cents": 1}).ResponseRecorder, 400, "validation_failed") // totals are never taken from the client
	if e.count(`SELECT count(*) FROM sales`) != 0 {
		t.Fatal("refused requests must not create a sale")
	}

	// The sale: split payment, snapshots, tax, receipt number, order closed.
	first := nextReceipt()
	r := finalise(tj, a, "sale-key-0006", good)
	expect(t, r.ResponseRecorder, 201, "")
	sale := decodeInto[saleResp](t, r.ResponseRecorder).Sale
	if sale.ReceiptNo != first || sale.TotalCents != 194000 || sale.SubtotalCents != 194000 || sale.DiscountCents != 0 || sale.TaxCents != 24828+1103 ||
		sale.PaidCents != 100000 || sale.PendingCents != 94000 || sale.BalanceCents != 94000 || sale.PaymentStatus != "partially_paid" || sale.TaxStatus != "provisional_unverified" {
		t.Fatalf("sale = %+v", sale)
	}
	if len(sale.Items) != 3 || sale.Items[0].UnitPriceCents != 85000 || sale.Items[0].ModifiersCents != 5000 || sale.Items[0].LineTotalCents != 180000 || sale.Items[0].TaxCents != 24828 ||
		sale.Items[2].TaxCents != 0 {
		t.Fatalf("items = %+v", sale.Items)
	}
	if len(sale.Payments) != 2 || sale.Payments[0].Status != "confirmed" || sale.Payments[1].Status != "pending_confirmation" || sale.Payments[1].Reference != "QWE123ABC" {
		t.Fatalf("payments = %+v", sale.Payments)
	}
	if e.count(`SELECT count(*) FROM orders WHERE id = $1 AND status = 'finalised'`, a) != 1 {
		t.Fatal("finalising must close the order")
	}
	_ = steakLine
	_ = waterLine

	// A retry with the same key returns the stored sale and uses no new receipt number.
	r = finalise(tj, a, "sale-key-0006", good)
	expect(t, r.ResponseRecorder, 201, "")
	if r.Header().Get("Idempotent-Replay") != "true" || decodeInto[saleResp](t, r.ResponseRecorder).Sale.ID != sale.ID || e.count(`SELECT count(*) FROM sales`) != 1 || nextReceipt() != first+1 {
		t.Fatal("a replay must return the stored sale and create nothing")
	}
	expect(t, finalise(tj, a, "sale-key-0006", map[string]any{"payments": []map[string]any{{"method": "cash", "amount_cents": 1}}}).ResponseRecorder, 422, "idempotency_key_reused")
	// A second finalisation of the same order with a new key is refused.
	expect(t, finalise(tj, a, "sale-key-0007", good).ResponseRecorder, 409, "order_already_finalised")
	if e.count(`SELECT count(*) FROM sales`) != 1 {
		t.Fatal("an order can be sold once")
	}

	// The order is now immutable, and later price changes never touch the sale (AC-01 snapshots).
	expect(t, e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(a) + "/items", jar: wa, body: map[string]any{"product_id": tea, "qty": 1}}), 409, "order_closed")
	expect(t, e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(a) + "/cancel", jar: wa, body: map[string]any{"reason": "oops"}}), 409, "order_closed")
	expect(t, e.do(req{method: "PUT", path: "/api/v1/products/" + itoa(steak) + "/price", jar: mj, body: map[string]any{"price_cents": 99000}}), 200, "")
	w = e.do(req{method: "GET", path: "/api/v1/sales/" + itoa(sale.ID), jar: tj})
	expect(t, w, 200, "")
	if got := decodeInto[saleResp](t, w).Sale; got.TotalCents != 194000 || got.Items[0].UnitPriceCents != 85000 || got.TaxCents != sale.TaxCents {
		t.Fatalf("a price change altered a finished sale: %+v", got)
	}
	// Kitchen status is independent of payment (AC-04): the ticket is untouched and still moves.
	if e.count(`SELECT count(*) FROM kitchen_tickets WHERE order_id = $1 AND status = 'submitted'`, a) != 1 {
		t.Fatal("finalising must not change a kitchen ticket")
	}
	tid := e.rows(`SELECT id FROM kitchen_tickets WHERE order_id = $1`, a)
	expect(t, e.do(req{method: "POST", path: "/api/v1/kitchen/tickets/" + itoa(int64(tid)) + "/status", jar: kj, body: map[string]any{"status": "ready"}}), 200, "")

	// The database refuses edits to a finished sale even if the service were bypassed.
	ctx := context.Background()
	for name, sql := range map[string]string{
		"sale total":      `UPDATE sales SET total_cents = 1, subtotal_cents = 1`,
		"sale receipt":    `UPDATE sales SET receipt_no = 999999`,
		"sale delete":     `DELETE FROM sales`,
		"sale line":       `UPDATE sale_items SET qty = 9`,
		"sale line del":   `DELETE FROM sale_items`,
		"payment amount":  `UPDATE payments SET amount_cents = 1`,
		"payment delete":  `DELETE FROM payments`,
		"payment reverse": `UPDATE payments SET status = 'pending_confirmation' WHERE status = 'confirmed'`,
	} {
		if _, err := e.pool.Exec(ctx, sql); err == nil {
			t.Errorf("%s: the database accepted a change it must refuse", name)
		}
	}
	if _, err := e.pool.Exec(ctx, `UPDATE sales SET payment_status = 'paid' WHERE id = $1`, sale.ID); err != nil {
		t.Fatalf("a sale's payment status must remain changeable: %v", err)
	}
	e.exec(`UPDATE sales SET payment_status = 'partially_paid' WHERE id = $1`, sale.ID)

	// Duplicate typed reference: refused, and the failed attempt leaves no receipt number used and the order open.
	b := newOrder(wa)
	add(wa, b, tea, 1)
	before := nextReceipt()
	dup := map[string]any{"payments": []map[string]any{{"method": "mpesa_manual", "amount_cents": 8000, "reference": "QWE123abc"}}}
	expect(t, finalise(tj, b, "sale-key-0008", dup).ResponseRecorder, 409, "duplicate_reference")
	if nextReceipt() != before || e.count(`SELECT count(*) FROM orders WHERE id = $1 AND status = 'open'`, b) != 1 || e.count(`SELECT count(*) FROM idempotency_records WHERE idem_key = 'sale-key-0008'`) != 0 {
		t.Fatal("a refused sale must leave no receipt number used, an open order and no stored key")
	}
	// Failure injection: if saving the lines fails the whole sale rolls back and the number is not lost.
	e.exec(`CREATE FUNCTION boom() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure'; END $$`)
	e.exec(`CREATE TRIGGER boom BEFORE INSERT ON sale_items FOR EACH ROW EXECUTE FUNCTION boom()`)
	expect(t, finalise(tj, b, "sale-key-0009", map[string]any{}).ResponseRecorder, 500, "internal_error")
	if nextReceipt() != before || e.count(`SELECT count(*) FROM sales`) != 1 || e.count(`SELECT count(*) FROM orders WHERE id = $1 AND status = 'open'`, b) != 1 {
		t.Fatal("a failed finalisation must leave no sale, no used number and an open order")
	}
	e.exec(`DROP TRIGGER boom ON sale_items`)
	e.exec(`DROP FUNCTION boom()`)
	r = finalise(tj, b, "sale-key-0009", map[string]any{}) // the same key works again; unpaid, a due balance is left
	expect(t, r.ResponseRecorder, 201, "")
	if got := decodeInto[saleResp](t, r.ResponseRecorder).Sale; got.ReceiptNo != before || got.PaymentStatus != "unpaid" || got.BalanceCents != 8000 {
		t.Fatalf("no gap and a due balance expected: %+v", got)
	}

	// Many identical requests at once make one sale; many different keys for one order also make one.
	c := newOrder(wa)
	add(wa, c, tea, 1)
	var wg sync.WaitGroup
	ids := make(chan int64, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := finalise(tj, c, "sale-key-race-1", map[string]any{}); r.Code == 201 {
				ids <- decodeInto[saleResp](t, r.ResponseRecorder).Sale.ID
			}
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[int64]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 || e.count(`SELECT count(*) FROM sales WHERE order_id = $1`, c) != 1 {
		t.Fatalf("simultaneous retries made %d sales (%v)", e.count(`SELECT count(*) FROM sales WHERE order_id = $1`, c), seen)
	}
	d := newOrder(wa)
	add(wa, d, tea, 1)
	codes := make(chan int, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes <- finalise(tj, d, "sale-key-diff-"+itoa(int64(i)), map[string]any{}).Code
		}(i)
	}
	wg.Wait()
	close(codes)
	created := 0
	for c := range codes {
		if c == 201 {
			created++
		} else if c != 409 {
			t.Errorf("unexpected status %d", c)
		}
	}
	if created != 1 || e.count(`SELECT count(*) FROM sales WHERE order_id = $1`, d) != 1 {
		t.Fatalf("different keys on one order made %d sales, want exactly 1", created)
	}

	// Discounts: only with sales.discount, with a reason, never more than the line, and recorded with who approved.
	f := newOrder(wa)
	fl := add(wa, f, steak, 1)
	ft := add(wa, f, tea, 1)
	d1 := func(item int64, amount int, reason string) map[string]any {
		return map[string]any{"discounts": []map[string]any{{"order_item_id": item, "amount_cents": amount, "reason": reason}}}
	}
	expect(t, finalise(mj, f, "sale-key-0010", d1(fl, 500, "")).ResponseRecorder, 400, "validation_failed")
	expect(t, finalise(mj, f, "sale-key-0011", d1(fl, 99999999, "too much")).ResponseRecorder, 400, "validation_failed")
	expect(t, finalise(mj, f, "sale-key-0012", d1(999999, 500, "ghost line")).ResponseRecorder, 400, "validation_failed")
	r = finalise(mj, f, "sale-key-0013", d1(ft, 1000, "regular guest"))
	expect(t, r.ResponseRecorder, 201, "")
	ds := decodeInto[saleResp](t, r.ResponseRecorder).Sale
	// steak is now 99 000 (the price change), chai 8 000 less 1 000 discount -> 7 000.
	if ds.DiscountCents != 1000 || ds.TotalCents != ds.SubtotalCents-1000 || ds.Items[1].DiscountCents != 1000 || ds.Items[1].DiscountReason != "regular guest" || ds.Items[1].LineTotalCents != 7000 {
		t.Fatalf("discounted sale = %+v", ds)
	}
	if e.count(`SELECT count(*) FROM sale_items WHERE discount_cents > 0 AND discount_approved_by IS NOT NULL AND discount_reason IS NOT NULL`) != 1 {
		t.Fatal("a discount must record its reason and approver")
	}

	// An order with nothing to sell, and the tax setting the sale does not support yet.
	g := newOrder(wa)
	expect(t, finalise(tj, g, "sale-key-0014", map[string]any{}).ResponseRecorder, 409, "order_empty")
	gl := add(wa, g, tea, 1)
	expect(t, e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(g) + "/items/" + itoa(gl) + "/cancel", jar: wa, body: map[string]any{"reason": "no"}}), 200, "")
	expect(t, finalise(tj, g, "sale-key-0015", map[string]any{}).ResponseRecorder, 409, "order_empty") // a cancelled line is not sold
	add(wa, g, tea, 1)
	e.exec(`UPDATE system_settings SET value = 'false'::jsonb WHERE key = 'tax_inclusive'`)
	expect(t, finalise(tj, g, "sale-key-0016", map[string]any{}).ResponseRecorder, 409, "tax_exclusive_unsupported")
	e.exec(`UPDATE system_settings SET value = 'true'::jsonb WHERE key = 'tax_inclusive'`)
	expect(t, finalise(tj, g, "sale-key-0017", map[string]any{}).ResponseRecorder, 201, "")

	// A role limited to bar products: bar-only orders yes, restaurant products no, other people's orders not visible.
	h := newOrder(bj)
	add(bj, h, lager, 2)
	expect(t, finalise(bj, h, "sale-key-0018", map[string]any{}).ResponseRecorder, 201, "")
	i := newOrder(bj)
	add(bj, i, tea, 1)
	expect(t, e.do(req{method: "GET", path: "/api/v1/orders/" + itoa(i) + "/bill", jar: bj}), 403, "bar_only")
	expect(t, finalise(bj, i, "sale-key-0019", map[string]any{}).ResponseRecorder, 403, "bar_only")
	expect(t, finalise(bj, a, "sale-key-0020", map[string]any{}).ResponseRecorder, 404, "not_found") // waitera's order

	// Reading a sale: everyone with sales.view in full, own records for waiter and bartender, 403 without it.
	expect(t, e.do(req{method: "GET", path: "/api/v1/sales/" + itoa(sale.ID), jar: wa}), 200, "") // the waiter took the order
	expect(t, e.do(req{method: "GET", path: "/api/v1/sales/" + itoa(sale.ID), jar: wb}), 404, "not_found")
	expect(t, e.do(req{method: "GET", path: "/api/v1/sales/" + itoa(sale.ID), jar: kj}), 403, "forbidden")
	expect(t, e.do(req{method: "GET", path: "/api/v1/sales/99999", jar: mj}), 404, "not_found")
	for _, act := range []string{"sale.finalised", "payment.recorded"} {
		if e.auditN(act) == 0 {
			t.Errorf("no audit row for %s", act)
		}
	}
}

type sendResp struct{ *httptest.ResponseRecorder }

func (e *testEnv) count(sql string, args ...any) int { return e.rows(sql, args...) }

// A small setup for the focused AC-01 tests: a cashier, a waiter, one product and a helper to open an order with one line.
type liteSales struct {
	e        *testEnv
	till, wa jar
	tea      int64
	shift    int64
}

func newLiteSales(t *testing.T) *liteSales {
	t.Helper()
	e := newEnv(t)
	e.addUser("mgr", false, "business_manager")
	e.addUser("till", false, "cashier")
	e.addUser("waitera", false, "waiter")
	mj := e.mustLogin("mgr")
	tea, _, _ := menuFixture(t, e, mj)
	l := &liteSales{e: e, till: e.mustLogin("till"), wa: e.mustLogin("waitera"), tea: tea}
	l.shift = e.openShift(t, l.till, 0) // cash needs an open shift
	return l
}

// openShift opens a shift for the signed-in user and returns its id.
func (e *testEnv) openShift(t *testing.T, j jar, float int64) int64 {
	t.Helper()
	w := e.do(req{method: "POST", path: "/api/v1/shifts/open", jar: j, header: map[string]string{"Idempotency-Key": "open-" + itoa(int64(e.rows(`SELECT count(*) FROM cashier_shifts`)+1)) + "-aaaa"}, body: map[string]any{"opening_float_cents": float}})
	expect(t, w, 201, "")
	return decodeInto[struct {
		Shift struct {
			ID int64 `json:"id"`
		} `json:"shift"`
	}](t, w).Shift.ID
}

func (l *liteSales) order(t *testing.T) int64 {
	t.Helper()
	w := l.e.do(req{method: "POST", path: "/api/v1/orders", jar: l.wa, body: map[string]any{"order_type": "takeaway"}})
	expect(t, w, 201, "")
	id := decodeInto[orderResp](t, w).Order.ID
	expect(t, l.e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(id) + "/items", jar: l.wa, body: map[string]any{"product_id": l.tea, "qty": 1}}), 201, "")
	return id
}

func (l *liteSales) finalise(order int64, key string, body map[string]any) *sendResp {
	return &sendResp{l.e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(order) + "/finalise", jar: l.till, header: map[string]string{"Idempotency-Key": key}, body: body})}
}

func (l *liteSales) receipts() int {
	return l.e.rows(`SELECT next_value FROM document_counters WHERE doc_type = 'receipt' AND scope = ''`)
}

func TestAC01_RetrySameKeyReturnsStoredSale(t *testing.T) {
	l := newLiteSales(t)
	o := l.order(t)
	start := l.receipts()
	first := l.finalise(o, "retry-key-0001", map[string]any{})
	expect(t, first.ResponseRecorder, 201, "")
	again := l.finalise(o, "retry-key-0001", map[string]any{})
	expect(t, again.ResponseRecorder, 201, "")
	a, b := decodeInto[saleResp](t, first.ResponseRecorder).Sale, decodeInto[saleResp](t, again.ResponseRecorder).Sale
	if a.ID != b.ID || a.ReceiptNo != b.ReceiptNo || again.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("a retry must return the stored sale: %+v vs %+v", a, b)
	}
	if l.e.rows(`SELECT count(*) FROM sales`) != 1 || l.receipts() != start+1 {
		t.Fatal("a retry must not create a second sale or use a second receipt number")
	}
	expect(t, l.finalise(o, "retry-key-0001", map[string]any{"payments": []map[string]any{{"method": "cash", "amount_cents": 100}}}).ResponseRecorder, 422, "idempotency_key_reused")
}

func TestAC01_ConcurrentSameKeyCreatesOneSale(t *testing.T) {
	l := newLiteSales(t)
	o := l.order(t)
	var wg sync.WaitGroup
	ids := make(chan int64, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := l.finalise(o, "race-key-00001", map[string]any{}); r.Code == 201 {
				ids <- decodeInto[saleResp](t, r.ResponseRecorder).Sale.ID
			}
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[int64]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 || l.e.rows(`SELECT count(*) FROM sales`) != 1 {
		t.Fatalf("simultaneous identical requests made %d sales (%v), want exactly 1", l.e.rows(`SELECT count(*) FROM sales`), seen)
	}
}

func TestAC01_SecondFinaliseOfSameOrderRejected(t *testing.T) {
	l := newLiteSales(t)
	o := l.order(t)
	expect(t, l.finalise(o, "second-key-0001", map[string]any{}).ResponseRecorder, 201, "")
	used := l.receipts()
	expect(t, l.finalise(o, "second-key-0002", map[string]any{}).ResponseRecorder, 409, "order_already_finalised")
	if l.e.rows(`SELECT count(*) FROM sales WHERE order_id = $1`, o) != 1 || l.receipts() != used {
		t.Fatal("a second finalisation must create no sale and use no receipt number")
	}
	// the database also refuses a second sale for one order
	if _, err := l.e.pool.Exec(context.Background(), `INSERT INTO sales (receipt_no, order_id, business_date, subtotal_cents, tax_cents, total_cents, cashier_id, tax_status)
		SELECT 99999, order_id, business_date, subtotal_cents, tax_cents, total_cents, cashier_id, tax_status FROM sales LIMIT 1`); err == nil {
		t.Fatal("two sales for one order must be refused by the database")
	}
}
