package handler_test

import (
	"net/http/httptest"
	"sync"
	"testing"
)

// Stage 15: discounts, voids and refunds (AC-05, ASSUMPTIONS.md S-18 and S-63).

type payoutResp struct {
	Method      string `json:"method"`
	AmountCents int64  `json:"amount_cents"`
	ShiftID     *int64 `json:"shift_id"`
}

type refundRec struct {
	ID          int64        `json:"id"`
	RefundNo    int64        `json:"refund_no"`
	Kind        string       `json:"kind"`
	Reason      string       `json:"reason"`
	AmountCents int64        `json:"amount_cents"`
	Payouts     []payoutResp `json:"payouts"`
}

type corrSale struct {
	ID            int64  `json:"id"`
	Status        string `json:"status"`
	TotalCents    int64  `json:"total_cents"`
	PaidCents     int64  `json:"paid_cents"`
	RefundedCents int64  `json:"refunded_cents"`
	Voidable      bool   `json:"voidable"`
	Items         []struct {
		ID             int64 `json:"id"`
		Qty            int32 `json:"qty"`
		RefundedQty    int32 `json:"refunded_qty"`
		LineTotalCents int64 `json:"line_total_cents"`
	} `json:"items"`
	Refunds []refundRec `json:"refunds"`
}

type corrResp struct {
	Sale   corrSale  `json:"sale"`
	Refund refundRec `json:"refund"`
}

type quoteResp struct {
	Quote struct {
		AmountCents int64 `json:"amount_cents"`
		Methods     []struct {
			Method          string `json:"method"`
			ReturnableCents int64  `json:"returnable_cents"`
		} `json:"methods"`
	} `json:"quote"`
}

type cashSummary struct {
	Summary struct {
		Cash struct {
			CashPaid    int64 `json:"cash_payments_cents"`
			CashRefunds int64 `json:"cash_refunds_cents"`
			Expected    int64 `json:"expected_cash_cents"`
		} `json:"cash"`
	} `json:"summary"`
}

// sell puts one order through finalise as the cashier. lines are (product, qty); pay is the payments body.
func (k *kitchenEnv) sell(j jar, discounts []map[string]any, pay []map[string]any, lines ...[2]int64) corrSale {
	k.t.Helper()
	o := k.open()
	for _, l := range lines {
		k.add(o, l[0], int(l[1]), "")
	}
	body := map[string]any{}
	if len(discounts) > 0 {
		for _, d := range discounts {
			if d["order_item_id"] == nil {
				d["order_item_id"] = k.firstItem(o)
			}
		}
		body["discounts"] = discounts
	}
	if len(pay) > 0 {
		body["payments"] = pay
	}
	w := k.e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(o) + "/finalise", jar: j, header: map[string]string{"Idempotency-Key": newKey()}, body: body})
	expect(k.t, w, 201, "")
	return decodeInto[corrResp](k.t, w).Sale
}

func (k *kitchenEnv) firstItem(order int64) int64 {
	k.t.Helper()
	w := k.e.do(req{method: "GET", path: "/api/v1/orders/" + itoa(order), jar: k.wait})
	expect(k.t, w, 200, "")
	return decodeInto[struct {
		Order kOrder `json:"order"`
	}](k.t, w).Order.Items[0].ID
}

func cash(amount int64) []map[string]any {
	return []map[string]any{{"method": "cash", "amount_cents": amount}}
}

func (k *kitchenEnv) void(j jar, sale int64, reason string, want int, errCode string) corrResp {
	k.t.Helper()
	w := k.e.do(req{method: "POST", path: "/api/v1/sales/" + itoa(sale) + "/void", jar: j, header: map[string]string{"Idempotency-Key": newKey()}, body: map[string]any{"reason": reason}})
	expect(k.t, w, want, errCode)
	if want >= 400 {
		return corrResp{}
	}
	return decodeInto[corrResp](k.t, w)
}

func (k *kitchenEnv) refund(j jar, sale int64, body map[string]any, want int, errCode string) corrResp {
	k.t.Helper()
	return k.refundKey(j, sale, newKey(), body, want, errCode)
}

func (k *kitchenEnv) refundKey(j jar, sale int64, key string, body map[string]any, want int, errCode string) corrResp {
	k.t.Helper()
	w := k.e.do(req{method: "POST", path: "/api/v1/sales/" + itoa(sale) + "/refunds", jar: j, header: map[string]string{"Idempotency-Key": key}, body: body})
	expect(k.t, w, want, errCode)
	if want >= 400 {
		return corrResp{}
	}
	return decodeInto[corrResp](k.t, w)
}

func (k *kitchenEnv) quote(j jar, sale int64, lines string) quoteResp {
	k.t.Helper()
	w := k.e.do(req{method: "GET", path: "/api/v1/sales/" + itoa(sale) + "/refund-quote?lines=" + lines, jar: j})
	expect(k.t, w, 200, "")
	return decodeInto[quoteResp](k.t, w)
}

func (k *kitchenEnv) getSale(j jar, id int64) corrSale {
	k.t.Helper()
	w := k.e.do(req{method: "GET", path: "/api/v1/sales/" + itoa(id), jar: j})
	expect(k.t, w, 200, "")
	return decodeInto[corrResp](k.t, w).Sale
}

func (k *kitchenEnv) cashSummary(j jar, shift int64) cashSummary {
	k.t.Helper()
	w := k.e.do(req{method: "GET", path: "/api/v1/shifts/" + itoa(shift) + "/summary", jar: j})
	expect(k.t, w, 200, "")
	return decodeInto[cashSummary](k.t, w)
}

// backdate moves a sale to yesterday's business date. The table refuses this normally, so the guard is switched off for the one statement.
func (k *kitchenEnv) backdate(sale int64) {
	k.t.Helper()
	k.e.exec(`ALTER TABLE sales DISABLE TRIGGER sales_guard`)
	k.e.exec(`UPDATE sales SET business_date = business_date - 1 WHERE id = $1`, sale)
	k.e.exec(`ALTER TABLE sales ENABLE TRIGGER sales_guard`)
}

func (k *kitchenEnv) lineRef(s corrSale, i int) int64 { return s.Items[i].ID }

// AC-05: a discount needs sales.discount and a reason, and the approver is recorded with an audit row.
func TestAC05_DiscountNeedsPermissionAndReason(t *testing.T) {
	k := newKitchenEnv(t)
	k.e.openShift(t, k.cash, 0)

	// The cashier cannot give a discount at all.
	o := k.open()
	item := k.add(o, k.tea, 2, "")
	w := k.e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(o) + "/finalise", jar: k.cash, header: map[string]string{"Idempotency-Key": newKey()},
		body: map[string]any{"discounts": []map[string]any{{"order_item_id": item, "amount_cents": 1000, "reason": "regular"}}}})
	expect(t, w, 403, "discount_not_permitted")
	// The manager needs a reason.
	for _, reason := range []string{"", "   "} {
		w = k.e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(o) + "/finalise", jar: k.mgr, header: map[string]string{"Idempotency-Key": newKey()},
			body: map[string]any{"discounts": []map[string]any{{"order_item_id": item, "amount_cents": 1000, "reason": reason}}}})
		expect(t, w, 400, "validation_failed")
	}
	if k.e.count(`SELECT count(*) FROM sales`) != 0 {
		t.Fatal("a refused discount still made a sale")
	}
	w = k.e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(o) + "/finalise", jar: k.mgr, header: map[string]string{"Idempotency-Key": newKey()},
		body: map[string]any{"discounts": []map[string]any{{"order_item_id": item, "amount_cents": 1000, "reason": "regular customer"}}}})
	expect(t, w, 201, "")
	sale := decodeInto[corrResp](t, w).Sale
	if sale.TotalCents != 16000-1000 {
		t.Fatalf("total %d, want 15000", sale.TotalCents)
	}
	if n := k.e.count(`SELECT count(*) FROM sale_items WHERE sale_id = $1 AND discount_cents = 1000 AND discount_reason = 'regular customer' AND discount_approved_by IS NOT NULL`, sale.ID); n != 1 {
		t.Fatalf("discount line not documented: %d", n)
	}
	if k.e.auditCount("sale.finalised", sale.ID) != 1 {
		t.Fatal("finalising with a discount wrote no audit row")
	}
}

// AC-05 and S-18: a void needs sales.void and a reason, puts the money back by the methods it came in by, and is audited.
func TestAC05_VoidNeedsPermissionAndReason(t *testing.T) {
	k := newKitchenEnv(t)
	cashShift := k.e.openShift(t, k.cash, 0)
	mgrShift := k.e.openShift(t, k.mgr, 0)
	sale := k.sell(k.cash, nil, cash(16000), [2]int64{k.tea, 2})
	if k.cashSummary(k.mgr, cashShift).Summary.Cash.CashPaid != 16000 {
		t.Fatal("the sale's cash is not on the cashier's shift")
	}
	if !k.getSale(k.mgr, sale.ID).Voidable {
		t.Fatal("a sale made today with no refund should be voidable")
	}

	k.void(k.cash, sale.ID, "mistake", 403, "forbidden") // the cashier has no sales.void
	k.void(k.wait, sale.ID, "mistake", 403, "forbidden")
	k.void(k.mgr, sale.ID, "", 400, "validation_failed")
	k.void(k.mgr, sale.ID, "   ", 400, "validation_failed")
	if k.getSale(k.mgr, sale.ID).Status != "finalised" {
		t.Fatal("a refused void changed the sale")
	}

	out := k.void(k.mgr, sale.ID, "wrong table", 201, "")
	if out.Sale.Status != "voided" || out.Refund.Kind != "void" || out.Refund.Reason != "wrong table" {
		t.Fatalf("void result: %+v", out)
	}
	if len(out.Refund.Payouts) != 1 || out.Refund.Payouts[0].Method != "cash" || out.Refund.Payouts[0].AmountCents != 16000 {
		t.Fatalf("a void must hand back what was received: %+v", out.Refund.Payouts)
	}
	if out.Sale.RefundedCents != 16000 || out.Sale.Voidable {
		t.Fatalf("sale after void: %+v", out.Sale)
	}
	// The cash left the manager's drawer, not the cashier's closed-over figure; the payment row itself is untouched.
	if r := k.cashSummary(k.mgr, mgrShift).Summary.Cash; r.CashRefunds != 16000 || r.Expected != -16000 {
		t.Fatalf("manager's drawer: %+v", r)
	}
	if n := k.e.count(`SELECT count(*) FROM payments WHERE sale_id = $1 AND status = 'confirmed' AND amount_cents = 16000`, sale.ID); n != 1 {
		t.Fatal("the payment row must stay as it was")
	}
	var no int64
	if err := k.e.pool.QueryRow(t.Context(), `SELECT receipt_no FROM sales WHERE id = $1`, sale.ID).Scan(&no); err != nil || no == 0 {
		t.Fatal("a voided sale keeps its receipt number")
	}
	if k.e.auditCount("sale.voided", sale.ID) != 1 {
		t.Fatal("the void wrote no audit row")
	}
	// A voided sale's receipt is not printed.
	w := k.e.do(req{method: "POST", path: "/api/v1/print/jobs", jar: k.cash, body: map[string]any{"doc_type": "receipt", "ref_id": sale.ID}})
	expect(t, w, 409, "sale_voided")
}

// AC-05: a refund needs sales.refund, a reason, lines and a way to pay it back, and is audited.
func TestAC05_RefundNeedsPermissionAndReason(t *testing.T) {
	k := newKitchenEnv(t)
	k.e.openShift(t, k.cash, 0)
	k.e.openShift(t, k.mgr, 0)
	sale := k.sell(k.cash, nil, cash(16000), [2]int64{k.tea, 2})
	line := k.lineRef(sale, 0)
	good := func() map[string]any {
		return map[string]any{"reason": "customer returned one", "lines": []map[string]any{{"sale_item_id": line, "qty": 1}}, "payouts": cash(8000)}
	}

	k.refund(k.cash, sale.ID, good(), 403, "forbidden")
	k.refund(k.wait, sale.ID, good(), 403, "forbidden")
	b := good()
	b["reason"] = ""
	k.refund(k.mgr, sale.ID, b, 400, "validation_failed")
	b = good()
	b["lines"] = []map[string]any{}
	k.refund(k.mgr, sale.ID, b, 400, "validation_failed")
	b = good()
	b["payouts"] = cash(7000) // does not add up to the refund
	k.refund(k.mgr, sale.ID, b, 400, "validation_failed")
	if k.e.count(`SELECT count(*) FROM refunds`) != 0 {
		t.Fatal("a refused refund left a record")
	}

	out := k.refund(k.mgr, sale.ID, good(), 201, "")
	if out.Refund.Kind != "refund" || out.Refund.AmountCents != 8000 || out.Sale.Status != "partially_refunded" || out.Sale.RefundedCents != 8000 {
		t.Fatalf("refund result: %+v", out)
	}
	if k.e.auditCount("sale.refunded", sale.ID) != 1 {
		t.Fatal("the refund wrote no audit row")
	}
	k.void(k.mgr, sale.ID, "too late", 409, "void_after_refund")
}

// S-18: the void rules.
func TestS18_VoidRules(t *testing.T) {
	k := newKitchenEnv(t)
	k.e.openShift(t, k.cash, 0)

	// Cash goes back from an open drawer: the manager has none yet.
	a := k.sell(k.cash, nil, cash(8000), [2]int64{k.tea, 1})
	k.void(k.mgr, a.ID, "wrong order", 409, "shift_required")
	if k.getSale(k.mgr, a.ID).Status != "finalised" {
		t.Fatal("a failed void left a mark")
	}
	k.e.openShift(t, k.mgr, 0)
	k.void(k.mgr, a.ID, "wrong order", 201, "")
	k.void(k.mgr, a.ID, "again", 409, "sale_already_voided")
	k.refund(k.mgr, a.ID, map[string]any{"reason": "x", "lines": []map[string]any{{"sale_item_id": k.lineRef(a, 0), "qty": 1}}, "payouts": cash(8000)}, 409, "sale_not_refundable")

	// Only on the day it was made.
	b := k.sell(k.cash, nil, cash(8000), [2]int64{k.tea, 1})
	k.backdate(b.ID)
	k.void(k.mgr, b.ID, "yesterday's", 409, "void_window_closed")
	if k.getSale(k.mgr, b.ID).Voidable {
		t.Fatal("a sale from another day must not be voidable")
	}
	// ...but it can be refunded.
	k.refund(k.mgr, b.ID, map[string]any{"reason": "next-day return", "lines": []map[string]any{{"sale_item_id": k.lineRef(b, 0), "qty": 1}}, "payouts": cash(8000)}, 201, "")

	// Money waiting for confirmation must be settled first.
	c := k.sell(k.cash, nil, []map[string]any{{"method": "cash", "amount_cents": 4000}, {"method": "mpesa_manual", "amount_cents": 4000, "reference": "QWE123RTY1"}}, [2]int64{k.tea, 1})
	k.void(k.mgr, c.ID, "x", 409, "pending_payments")

	// A split sale goes back by both methods once both are confirmed.
	pay := k.e.do(req{method: "GET", path: "/api/v1/payments?sale_id=" + itoa(c.ID), jar: k.mgr})
	expect(t, pay, 200, "")
	list := decodeInto[struct {
		Payments []struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		} `json:"payments"`
	}](t, pay).Payments
	for _, p := range list {
		if p.Method == "mpesa_manual" {
			expect(t, k.e.do(req{method: "POST", path: "/api/v1/payments/" + itoa(p.ID) + "/confirm", jar: k.mgr, header: map[string]string{"Idempotency-Key": newKey()}, body: map[string]any{}}), 200, "")
		}
	}
	out := k.void(k.mgr, c.ID, "customer left", 201, "")
	got := map[string]int64{}
	for _, p := range out.Refund.Payouts {
		got[p.Method] = p.AmountCents
	}
	if got["cash"] != 4000 || got["mpesa_manual"] != 4000 || len(got) != 2 {
		t.Fatalf("a split sale must go back by each method: %+v", got)
	}
}

// Taking a line back in pieces adds up to exactly the line; the status follows; nothing can be taken twice.
func TestRefundPiecesAddUpExactly(t *testing.T) {
	k := newKitchenEnv(t)
	k.e.openShift(t, k.cash, 0)
	mgrShift := k.e.openShift(t, k.mgr, 0)
	// Three teas, 100 cents off: the line is 23900, which does not divide by three.
	sale := k.sell(k.mgr, []map[string]any{{"amount_cents": 100, "reason": "rounding"}}, cash(23900), [2]int64{k.tea, 3})
	line := k.lineRef(sale, 0)

	var sum int64
	for i := 1; i <= 3; i++ {
		q := k.quote(k.mgr, sale.ID, itoa(line)+":1")
		out := k.refund(k.mgr, sale.ID, map[string]any{"reason": "piece " + itoa(int64(i)), "lines": []map[string]any{{"sale_item_id": line, "qty": 1}}, "payouts": cash(q.Quote.AmountCents)}, 201, "")
		if out.Refund.AmountCents != q.Quote.AmountCents {
			t.Fatalf("the quote said %d, the refund was %d", q.Quote.AmountCents, out.Refund.AmountCents)
		}
		sum += out.Refund.AmountCents
		want := "partially_refunded"
		if i == 3 {
			want = "refunded"
		}
		if out.Sale.Status != want {
			t.Fatalf("after piece %d the sale is %s, want %s", i, out.Sale.Status, want)
		}
	}
	if sum != 23900 {
		t.Fatalf("the pieces add up to %d, the line was 23900", sum)
	}
	k.refund(k.mgr, sale.ID, map[string]any{"reason": "again", "lines": []map[string]any{{"sale_item_id": line, "qty": 1}}, "payouts": cash(100)}, 409, "sale_not_refundable")
	if r := k.cashSummary(k.mgr, mgrShift).Summary.Cash; r.CashRefunds != 23900 {
		t.Fatalf("cash refunds on the drawer: %d", r.CashRefunds)
	}
}

// Money goes back only by a method it came in by, and only as much as came in that way.
func TestRefundPayoutsAreCapped(t *testing.T) {
	k := newKitchenEnv(t)
	k.e.openShift(t, k.cash, 0)
	k.e.openShift(t, k.mgr, 0)
	sale := k.sell(k.cash, nil, cash(8000), [2]int64{k.tea, 1}, [2]int64{k.bar, 1}) // two lines (8000 and 12000), 8000 paid
	line := k.lineRef(sale, 0)
	body := func(method string, amount int64) map[string]any {
		return map[string]any{"reason": "x", "lines": []map[string]any{{"sale_item_id": line, "qty": 1}}, "payouts": []map[string]any{{"method": method, "amount_cents": amount, "reference": "REF12345"}}}
	}
	k.refund(k.mgr, sale.ID, body("mpesa_manual", 8000), 409, "payout_exceeds_received") // nothing came in by M-Pesa
	// 20000 was sold but only 8000 was paid: the tea can go back, the soda cannot.
	k.refund(k.mgr, sale.ID, body("cash", 8000), 201, "")
	other := k.lineRef(sale, 1)
	k.refund(k.mgr, sale.ID, map[string]any{"reason": "x", "lines": []map[string]any{{"sale_item_id": other, "qty": 1}}, "payouts": cash(12000)}, 409, "payout_exceeds_received")
	if k.e.count(`SELECT count(*) FROM refunds`) != 1 {
		t.Fatal("a refused payout left a record")
	}
}

// Eight managers refunding the last unit at once: one wins, and the same key replays instead of refunding twice.
func TestRefundConcurrentOnlyOneWins(t *testing.T) {
	k := newKitchenEnv(t)
	k.e.openShift(t, k.cash, 0)
	k.e.openShift(t, k.mgr, 0)
	sale := k.sell(k.cash, nil, cash(8000), [2]int64{k.tea, 1})
	line := k.lineRef(sale, 0)
	body := map[string]any{"reason": "race", "lines": []map[string]any{{"sale_item_id": line, "qty": 1}}, "payouts": cash(8000)}

	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = k.e.do(req{method: "POST", path: "/api/v1/sales/" + itoa(sale.ID) + "/refunds", jar: k.mgr, header: map[string]string{"Idempotency-Key": newKey()}, body: body})
		}()
	}
	wg.Wait()
	created := 0
	for _, r := range results {
		switch r.Code {
		case 201:
			created++
		case 409:
		default:
			t.Fatalf("unexpected status %d: %s", r.Code, r.Body.String())
		}
	}
	if created != 1 {
		t.Fatalf("%d of 8 simultaneous refunds succeeded, want exactly 1", created)
	}
	if n := k.e.count(`SELECT count(*) FROM refunds WHERE sale_id = $1`, sale.ID); n != 1 {
		t.Fatalf("%d refund rows", n)
	}

	// A retry with the same key answers with the first result and refunds nothing more.
	s2 := k.sell(k.cash, nil, cash(8000), [2]int64{k.tea, 1})
	b2 := map[string]any{"reason": "retry", "lines": []map[string]any{{"sale_item_id": k.lineRef(s2, 0), "qty": 1}}, "payouts": cash(8000)}
	first := k.refundKey(k.mgr, s2.ID, "retry-key-refund-0001", b2, 201, "")
	w := k.e.do(req{method: "POST", path: "/api/v1/sales/" + itoa(s2.ID) + "/refunds", jar: k.mgr, header: map[string]string{"Idempotency-Key": "retry-key-refund-0001"}, body: b2})
	expect(t, w, 201, "")
	if w.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("the retry was not marked as a replay")
	}
	if again := decodeInto[corrResp](t, w); again.Refund.ID != first.Refund.ID {
		t.Fatal("the retry made a second refund")
	}
	if k.e.count(`SELECT count(*) FROM refunds WHERE sale_id = $1`, s2.ID) != 1 {
		t.Fatal("a retried refund made a second record")
	}
}

// The database refuses what the service already refuses (NN-08).
func TestRefundsAreProtectedInTheDatabase(t *testing.T) {
	k := newKitchenEnv(t)
	k.e.openShift(t, k.cash, 0)
	k.e.openShift(t, k.mgr, 0)
	sale := k.sell(k.cash, nil, cash(16000), [2]int64{k.tea, 2})
	k.refund(k.mgr, sale.ID, map[string]any{"reason": "one back", "lines": []map[string]any{{"sale_item_id": k.lineRef(sale, 0), "qty": 1}}, "payouts": cash(8000)}, 201, "")

	refuse := func(name, sql string, args ...any) {
		t.Helper()
		if _, err := k.e.pool.Exec(t.Context(), sql, args...); err == nil {
			t.Errorf("%s: the database accepted it", name)
		}
	}
	refuse("update refund", `UPDATE refunds SET reason = 'changed'`)
	refuse("delete refund", `DELETE FROM refunds`)
	refuse("truncate refunds", `TRUNCATE refunds CASCADE`)
	refuse("update refund item", `UPDATE refund_items SET qty = 2`)
	refuse("delete payout", `DELETE FROM refund_payouts`)
	// More taken back than was sold.
	refuse("over-refund a line", `INSERT INTO refund_items (refund_id, sale_item_id, qty, amount_cents, tax_cents) SELECT r.id, $2, 2, 1, 0 FROM refunds r WHERE r.sale_id = $1`, sale.ID, k.lineRef(sale, 0))
	// More cash handed back than came in.
	refuse("over-return cash", `INSERT INTO refund_payouts (refund_id, method, amount_cents, shift_id) SELECT r.id, 'cash', 9000, (SELECT id FROM cashier_shifts WHERE status = 'open' LIMIT 1) FROM refunds r WHERE r.sale_id = $1`, sale.ID)
	// A status that skips its record, or goes backwards.
	refuse("void without a void record", `UPDATE sales SET status = 'voided' WHERE id = $1`, sale.ID)
	refuse("back to finalised", `UPDATE sales SET status = 'finalised' WHERE id = $1`, sale.ID)
	// A void after a refund, even by direct insert.
	refuse("void after refund", `INSERT INTO refunds (refund_no, sale_id, kind, reason, approved_by, created_by, business_date, amount_cents, tax_cents) SELECT 999, $1, 'void', 'x', created_by, created_by, business_date, 0, 0 FROM refunds WHERE sale_id = $1 LIMIT 1`, sale.ID)
	if k.e.count(`SELECT count(*) FROM refunds WHERE sale_id = $1`, sale.ID) != 1 {
		t.Fatal("a refused statement left a refund behind")
	}
}

// Who may see the refund screens' data: the quote follows the same permission and scope as the refund.
func TestRefundQuotePermissionAndShape(t *testing.T) {
	k := newKitchenEnv(t)
	k.e.openShift(t, k.cash, 0)
	sale := k.sell(k.cash, nil, cash(8000), [2]int64{k.tea, 1})
	line := k.lineRef(sale, 0)
	expect(t, k.e.do(req{method: "GET", path: "/api/v1/sales/" + itoa(sale.ID) + "/refund-quote?lines=" + itoa(line) + ":1", jar: k.cash}), 403, "forbidden")
	expect(t, k.e.do(req{method: "GET", path: "/api/v1/sales/" + itoa(sale.ID) + "/refund-quote?lines=" + itoa(line) + ":2", jar: k.mgr}), 409, "refund_exceeds_sold")
	expect(t, k.e.do(req{method: "GET", path: "/api/v1/sales/" + itoa(sale.ID) + "/refund-quote?lines=abc", jar: k.mgr}), 400, "validation_failed")
	expect(t, k.e.do(req{method: "GET", path: "/api/v1/sales/999999/refund-quote?lines=1:1", jar: k.mgr}), 404, "not_found")
	q := k.quote(k.mgr, sale.ID, itoa(line)+":1")
	if q.Quote.AmountCents != 8000 || len(q.Quote.Methods) != 1 || q.Quote.Methods[0].Method != "cash" || q.Quote.Methods[0].ReturnableCents != 8000 {
		t.Fatalf("quote: %+v", q.Quote)
	}
}
