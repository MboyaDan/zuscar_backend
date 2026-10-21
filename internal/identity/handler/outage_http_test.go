package handler_test

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Stage 16: outage reconciliation (AC-17, ASSUMPTIONS.md S-66 to S-70).

type outageSale struct {
	ID           int64  `json:"id"`
	ReceiptNo    int64  `json:"receipt_no"`
	OrderID      *int64 `json:"order_id"`
	BusinessDate string `json:"business_date"`
	TaxCents     int64  `json:"tax_cents"`
	TotalCents   int64  `json:"total_cents"`
	PaidCents    int64  `json:"paid_cents"`
	Status       string `json:"status"`
	Source       string `json:"source"`
	ShiftID      *int64 `json:"shift_id"`
	Payments     []struct {
		Status string `json:"status"`
		Method string `json:"method"`
	} `json:"payments"`
	Items []struct {
		ID             int64 `json:"id"`
		UnitPriceCents int64 `json:"unit_price_cents"`
	} `json:"items"`
	ManualReceipt *struct {
		ManualReceiptNo string `json:"manual_receipt_no"`
		PaperTotalCents int64  `json:"paper_total_cents"`
		DiscrepancyNote string `json:"discrepancy_note"`
		EnteredByName   string `json:"entered_by_name"`
	} `json:"manual_receipt"`
}

type outageResp struct {
	Sale outageSale `json:"sale"`
}

type outageList struct {
	Entries []struct {
		ManualReceiptNo string `json:"manual_receipt_no"`
		SaleID          int64  `json:"sale_id"`
		PaperTotalCents int64  `json:"paper_total_cents"`
		SaleTotalCents  int64  `json:"sale_total_cents"`
	} `json:"entries"`
	NextCursor string `json:"next_cursor"`
}

// paper is a valid body: two cups of tea at 80.00 each, paid in cash, written at 'no' a few hours ago.
func (k *kitchenEnv) paper(no string, mutate ...func(map[string]any)) map[string]any {
	b := map[string]any{
		"manual_receipt_no": no,
		"occurred_at":       time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339),
		"lines":             []map[string]any{{"product_id": k.tea, "qty": 2, "unit_price_cents": 8000}},
		"payments":          []map[string]any{{"method": "cash", "amount_cents": 16000}},
		"paper_total_cents": 16000,
	}
	for _, m := range mutate {
		m(b)
	}
	return b
}

func (k *kitchenEnv) enter(j jar, key string, body map[string]any) *httptest.ResponseRecorder {
	k.t.Helper()
	return k.e.do(req{method: "POST", path: "/api/v1/reconciliation/manual-receipts", jar: j, header: map[string]string{"Idempotency-Key": key}, body: body})
}

func (k *kitchenEnv) mustEnter(body map[string]any) outageSale {
	k.t.Helper()
	w := k.enter(k.mgr, newKey(), body)
	expect(k.t, w, 201, "")
	return decodeInto[outageResp](k.t, w).Sale
}

func TestAC17_DuplicateManualReceiptRejected(t *testing.T) {
	k := newKitchenEnv(t)
	first := k.mustEnter(k.paper("A-0412"))
	if first.Source != "outage_reconciliation" || first.ManualReceipt == nil || first.ManualReceipt.ManualReceiptNo != "A-0412" {
		t.Fatalf("not an outage sale with its paper number: %+v", first)
	}

	// The same paper receipt again, however it is typed, is refused and writes nothing.
	for _, again := range []string{"A-0412", "a-0412", "  a-0412 ", "A -0412"} {
		w := k.enter(k.mgr, newKey(), k.paper(again))
		expect(t, w, 409, "duplicate_manual_receipt")
	}
	if n := k.e.count(`SELECT count(*) FROM sales WHERE source = 'outage_reconciliation'`); n != 1 {
		t.Fatalf("%d outage sales, want 1", n)
	}
	if n := k.e.count(`SELECT count(*) FROM manual_receipt_reconciliations`); n != 1 {
		t.Fatalf("%d entries, want 1", n)
	}
	if n := k.e.count(`SELECT count(*) FROM payments`); n != 1 {
		t.Fatalf("%d payments, want 1: a refused duplicate must not record money", n)
	}

	// Refused attempts do not use up receipt numbers: the next ordinary sale follows the outage sale directly.
	next := k.sell(k.cash, nil, nil, [2]int64{k.tea, 1})
	var no int64
	if err := k.e.pool.QueryRow(t.Context(), `SELECT receipt_no FROM sales WHERE id = $1`, next.ID).Scan(&no); err != nil || no != first.ReceiptNo+1 {
		t.Fatalf("next receipt %d (err %v), want %d", no, err, first.ReceiptNo+1)
	}

	// A different paper receipt is fine.
	k.mustEnter(k.paper("A-0413"))
}

func TestAC17_TwoClerksEnteringTheSameReceiptAtOnce(t *testing.T) {
	k := newKitchenEnv(t)
	codes := make([]int, 6)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = k.enter(k.mgr, newKey(), k.paper("RACE-1")).Code
		}()
	}
	wg.Wait()
	created, dup := 0, 0
	for _, c := range codes {
		switch c {
		case 201:
			created++
		case 409:
			dup++
		}
	}
	if created != 1 || dup != len(codes)-1 {
		t.Fatalf("statuses %v: want one 201 and the rest 409", codes)
	}
	if k.e.count(`SELECT count(*) FROM sales WHERE manual_receipt_no = 'RACE-1'`) != 1 || k.e.count(`SELECT count(*) FROM manual_receipt_reconciliations`) != 1 {
		t.Fatal("the race wrote more than one entry")
	}
}

func TestAC17_RetryWithSameKeyReplays(t *testing.T) {
	k := newKitchenEnv(t)
	key := newKey()
	body := k.paper("K-1") // built once: it carries the current time, so two calls could differ across a second boundary
	w1 := k.enter(k.mgr, key, body)
	expect(t, w1, 201, "")
	w2 := k.enter(k.mgr, key, body)
	expect(t, w2, 201, "")
	if w2.Header().Get("Idempotent-Replayed") != "true" || decodeInto[outageResp](t, w1).Sale.ID != decodeInto[outageResp](t, w2).Sale.ID {
		t.Fatal("a retry with the same key should return the stored sale")
	}
	if k.e.count(`SELECT count(*) FROM sales`) != 1 {
		t.Fatal("a retry made a second sale")
	}
	// The same key with a different receipt is a different request.
	expect(t, k.enter(k.mgr, key, k.paper("K-2")), 422, "idempotency_key_reused")
}

func TestAC17_OnlyReconcilersMayEnterOrList(t *testing.T) {
	k := newKitchenEnv(t)
	for _, j := range []jar{k.cash, k.wait, k.cook} {
		expect(t, k.enter(j, newKey(), k.paper("P-1")), 403, "forbidden")
		expect(t, k.e.do(req{method: "GET", path: "/api/v1/reconciliation/manual-receipts", jar: j}), 403, "forbidden")
	}
	if k.e.count(`SELECT count(*) FROM sales`) != 0 {
		t.Fatal("a refused entry made a sale")
	}
	expect(t, k.e.do(req{method: "POST", path: "/api/v1/reconciliation/manual-receipts", jar: k.mgr, body: k.paper("P-1")}), 400, "") // key is required
}

func TestAC17_EntryIsValidated(t *testing.T) {
	k := newKitchenEnv(t)
	bad := map[string]func(map[string]any){
		"empty number":   func(b map[string]any) { b["manual_receipt_no"] = "  " },
		"odd characters": func(b map[string]any) { b["manual_receipt_no"] = "A#1" },
		"too long":       func(b map[string]any) { b["manual_receipt_no"] = strings.Repeat("9", 33) },
		"future":         func(b map[string]any) { b["occurred_at"] = time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339) },
		"older than 31 days": func(b map[string]any) {
			b["occurred_at"] = time.Now().Add(-32 * 24 * time.Hour).UTC().Format(time.RFC3339)
		},
		"no time":  func(b map[string]any) { delete(b, "occurred_at") },
		"no lines": func(b map[string]any) { b["lines"] = []map[string]any{} },
		"unknown product": func(b map[string]any) {
			b["lines"] = []map[string]any{{"product_id": 999999, "qty": 1, "unit_price_cents": 100}}
		},
		"zero quantity": func(b map[string]any) {
			b["lines"] = []map[string]any{{"product_id": k.tea, "qty": 0, "unit_price_cents": 100}}
		},
		"negative price": func(b map[string]any) {
			b["lines"] = []map[string]any{{"product_id": k.tea, "qty": 1, "unit_price_cents": -1}}
		},
		"overpaid": func(b map[string]any) { b["payments"] = []map[string]any{{"method": "cash", "amount_cents": 16001}} },
		"mpesa without code": func(b map[string]any) {
			b["payments"] = []map[string]any{{"method": "mpesa_manual", "amount_cents": 16000}}
		},
		"no paper total":               func(b map[string]any) { delete(b, "paper_total_cents") },
		"paper total differs, no note": func(b map[string]any) { b["paper_total_cents"] = 15000 },
	}
	for name, mut := range bad {
		w := k.enter(k.mgr, newKey(), k.paper("V-1", mut))
		if w.Code != 400 {
			t.Fatalf("%s: status %d, want 400: %s", name, w.Code, w.Body.String())
		}
	}
	if k.e.count(`SELECT count(*) FROM sales`) != 0 || k.e.count(`SELECT count(*) FROM payments`) != 0 {
		t.Fatal("a refused entry left records behind")
	}
	// A differing paper total is accepted when it is explained, and both totals are kept.
	s := k.mustEnter(k.paper("V-1", func(b map[string]any) {
		b["paper_total_cents"] = 15000
		b["discrepancy_note"] = "clerk wrote 150.00, two teas at 80.00 each"
	}))
	if s.TotalCents != 16000 || s.ManualReceipt.PaperTotalCents != 15000 || s.ManualReceipt.DiscrepancyNote == "" {
		t.Fatalf("totals not kept: %+v", s)
	}
}

func TestAC17_OutageSaleBehavesLikeASale(t *testing.T) {
	k := newKitchenEnv(t)
	mgrShift := k.e.openShift(t, k.mgr, 0)
	occurred := time.Now().Add(-50 * time.Hour)
	s := k.mustEnter(k.paper("B-77", func(b map[string]any) {
		b["occurred_at"] = occurred.UTC().Format(time.RFC3339)
		b["source_note"] = "paper book 2, page 14"
	}))
	if s.OrderID != nil || s.ShiftID != nil || s.Status != "finalised" || s.TotalCents != 16000 || s.PaidCents != 16000 || s.TaxCents != 2207 {
		t.Fatalf("unexpected sale: %+v", s)
	}
	if want := occurred.In(time.FixedZone("EAT", 3*3600)).Format("2006-01-02"); s.BusinessDate != want {
		t.Fatalf("business date %s, want %s (the day it was sold, not the day it was entered)", s.BusinessDate, want)
	}
	if len(s.Payments) != 1 || s.Payments[0].Status != "confirmed" || s.ManualReceipt.EnteredByName != "mgr" {
		t.Fatalf("payments or entry not as expected: %+v", s)
	}
	if k.e.auditCount("sale.outage_entered", s.ID) != 1 {
		t.Fatal("entering a paper receipt wrote no audit row")
	}
	// The cash was taken during the outage, not on this manager's shift, so it does not change the shift's expected cash (S-70).
	if got := k.cashSummary(k.mgr, mgrShift).Summary.Cash; got.CashPaid != 0 || got.Expected != 0 {
		t.Fatalf("the outage cash landed on a shift: %+v", got)
	}
	// No receipt is printed: the customer already holds the paper one.
	k.job(k.mgr, "POST", "/api/v1/print/jobs", map[string]any{"doc_type": "receipt", "ref_id": s.ID}, 409, "outage_entry")
	// A mistake in an old entry is corrected with a refund like any other sale; it is too old to void.
	k.void(k.mgr, s.ID, "wrong", 409, "")
	r := k.refund(k.mgr, s.ID, map[string]any{"reason": "customer returned one", "lines": []map[string]any{{"sale_item_id": s.Items[0].ID, "qty": 1}},
		"payouts": []map[string]any{{"method": "cash", "amount_cents": 8000}}}, 201, "")
	if r.Sale.Status != "partially_refunded" {
		t.Fatalf("status %s, want partially_refunded", r.Sale.Status)
	}
	// It shows in the sales list as an outage entry.
	w := k.e.do(req{method: "GET", path: "/api/v1/sales", jar: k.mgr})
	expect(t, w, 200, "")
	if !strings.Contains(w.Body.String(), `"source":"outage_reconciliation"`) {
		t.Fatal("the sales list does not show the outage source")
	}
}

func TestAC17_ListFindsEntriesByNumber(t *testing.T) {
	k := newKitchenEnv(t)
	for _, n := range []string{"L-1", "L-2", "L-3"} {
		k.mustEnter(k.paper(n))
	}
	get := func(q string) outageList {
		w := k.e.do(req{method: "GET", path: "/api/v1/reconciliation/manual-receipts" + q, jar: k.mgr})
		expect(t, w, 200, "")
		return decodeInto[outageList](t, w)
	}
	if l := get("?manual_receipt_no=l-2"); len(l.Entries) != 1 || l.Entries[0].ManualReceiptNo != "L-2" {
		t.Fatalf("lookup by number: %+v", l)
	}
	if l := get("?manual_receipt_no=nope"); len(l.Entries) != 0 {
		t.Fatalf("lookup of a number not entered: %+v", l)
	}
	p1 := get("?limit=2")
	if len(p1.Entries) != 2 || p1.Entries[0].ManualReceiptNo != "L-3" || p1.NextCursor == "" {
		t.Fatalf("first page: %+v", p1)
	}
	if p2 := get("?limit=2&cursor=" + p1.NextCursor); len(p2.Entries) != 1 || p2.Entries[0].ManualReceiptNo != "L-1" || p2.NextCursor != "" {
		t.Fatalf("second page: %+v", p2)
	}
}

func TestAC17_DatabaseRefusesHalfEntries(t *testing.T) {
	k := newKitchenEnv(t)
	s := k.mustEnter(k.paper("D-1"))
	ctx := t.Context()

	// An outage sale cannot be committed without its entry.
	tx, err := k.e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sales (receipt_no, business_date, subtotal_cents, tax_cents, total_cents, source, manual_receipt_no, cashier_id, tax_status)
		SELECT 9000001, current_date, 100, 0, 100, 'outage_reconciliation', 'RAW-1', id, 'verified' FROM users LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err == nil {
		t.Fatal("an outage sale without an entry was committed")
	}
	// An entry must carry its sale's paper number.
	if _, err := k.e.pool.Exec(ctx, `INSERT INTO manual_receipt_reconciliations (manual_receipt_no, sale_id, original_occurred_at, paper_total_cents, entered_by)
		SELECT 'OTHER', $1, now(), 16000, id FROM users LIMIT 1`, s.ID); err == nil {
		t.Fatal("an entry with a different paper number was accepted")
	}
	// Entries are never edited or removed.
	if _, err := k.e.pool.Exec(ctx, `UPDATE manual_receipt_reconciliations SET source_note = 'x'`); err == nil {
		t.Fatal("an entry was edited")
	}
	if _, err := k.e.pool.Exec(ctx, `DELETE FROM manual_receipt_reconciliations`); err == nil {
		t.Fatal("an entry was deleted")
	}
}
