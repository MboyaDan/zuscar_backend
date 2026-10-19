package handler_test

import (
	"sync"
	"testing"
)

type payResp struct {
	Payment struct {
		ID          int64  `json:"id"`
		SaleID      int64  `json:"sale_id"`
		Method      string `json:"method"`
		Status      string `json:"status"`
		AmountCents int64  `json:"amount_cents"`
		Reference   string `json:"reference"`
		ConfirmedBy int64  `json:"confirmed_by"`
		FailedBy    int64  `json:"failed_by"`
		Note        string `json:"note"`
	} `json:"payment"`
}

type pageResp struct {
	Payments []struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
		Method string `json:"method"`
	} `json:"payments"`
	Sales []struct {
		ID            int64  `json:"id"`
		PaymentStatus string `json:"payment_status"`
		PaidCents     int64  `json:"paid_cents"`
		PendingCents  int64  `json:"pending_cents"`
	} `json:"sales"`
	NextCursor string `json:"next_cursor"`
}

// unpaidSale finalises a one-line order with no payment and returns the sale id, its total and a manager login.
func (l *liteSales) unpaidSale(t *testing.T, key string) (int64, int64) {
	t.Helper()
	o := l.order(t)
	r := l.finalise(o, key, map[string]any{})
	expect(t, r.ResponseRecorder, 201, "")
	s := decodeInto[saleResp](t, r.ResponseRecorder).Sale
	if s.PaymentStatus != "unpaid" {
		t.Fatalf("sale = %+v", s)
	}
	return s.ID, s.TotalCents
}

func (l *liteSales) pay(sale int64, j jar, key string, body map[string]any) *sendResp {
	h := map[string]string{}
	if key != "" {
		h["Idempotency-Key"] = key
	}
	return &sendResp{l.e.do(req{method: "POST", path: "/api/v1/sales/" + itoa(sale) + "/payments", jar: j, header: h, body: body})}
}

func (l *liteSales) sale(t *testing.T, id int64, j jar) saleResp {
	t.Helper()
	w := l.e.do(req{method: "GET", path: "/api/v1/sales/" + itoa(id), jar: j})
	expect(t, w, 200, "")
	return decodeInto[saleResp](t, w)
}

func TestAC02_RetryPaymentNotDoubleRecorded(t *testing.T) {
	l := newLiteSales(t)
	sale, total := l.unpaidSale(t, "pay-sale-0001")
	half := total / 2
	body := map[string]any{"method": "cash", "amount_cents": half}

	expect(t, l.pay(sale, l.till, "", body).ResponseRecorder, 400, "validation_failed") // the key is required
	r := l.pay(sale, l.till, "pay-key-000001", body)
	expect(t, r.ResponseRecorder, 201, "")
	again := l.pay(sale, l.till, "pay-key-000001", body)
	expect(t, again.ResponseRecorder, 201, "")
	a, b := decodeInto[payResp](t, r.ResponseRecorder).Payment, decodeInto[payResp](t, again.ResponseRecorder).Payment
	if again.Header().Get("Idempotent-Replay") != "true" || a.ID == 0 || a.ID != b.ID || a.AmountCents != b.AmountCents {
		t.Fatalf("a retry must return the stored answer: %s vs %s", again.Body.String(), r.Body.String())
	}
	if n := l.e.count(`SELECT count(*) FROM payments WHERE sale_id = $1`, sale); n != 1 {
		t.Fatalf("payments = %d, want 1", n)
	}
	if s := l.sale(t, sale, l.till).Sale; s.PaidCents != half || s.PaymentStatus != "partially_paid" {
		t.Fatalf("sale = %+v", s)
	}
	// The same key for a different request is refused, not replayed.
	expect(t, l.pay(sale, l.till, "pay-key-000001", map[string]any{"method": "cash", "amount_cents": half + 1}).ResponseRecorder, 422, "idempotency_key_reused")

	// Many tills sending the same retry at once still record one payment.
	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = l.pay(sale, l.till, "pay-key-000002", map[string]any{"method": "cash", "amount_cents": 1000}).Code
		}()
	}
	wg.Wait()
	for _, c := range codes {
		if c != 201 && c != 409 {
			t.Fatalf("codes = %v", codes)
		}
	}
	if n := l.e.count(`SELECT count(*) FROM payments WHERE sale_id = $1`, sale); n != 2 {
		t.Fatalf("payments = %d after concurrent retries, want 2", n)
	}

	// Two different payments racing for the last of the balance: one wins, the other is refused.
	rest := total - half - 1000
	codes = make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = l.pay(sale, l.till, []string{"pay-race-00001", "pay-race-00002"}[i], map[string]any{"method": "cash", "amount_cents": rest}).Code
		}()
	}
	wg.Wait()
	// The loser finds the sale already paid (409) once the winner has settled it.
	if !(codes[0] == 201 && codes[1] == 409) && !(codes[0] == 409 && codes[1] == 201) {
		t.Fatalf("race codes = %v, want one 201 and one 409", codes)
	}
	if s := l.sale(t, sale, l.till).Sale; s.PaidCents != total || s.PaymentStatus != "paid" || s.BalanceCents != 0 {
		t.Fatalf("sale = %+v", s)
	}
	// A paid sale takes no more.
	expect(t, l.pay(sale, l.till, "pay-key-000003", map[string]any{"method": "cash", "amount_cents": 100}).ResponseRecorder, 409, "sale_not_payable")
}

func TestAC02_DuplicateReferenceRejected(t *testing.T) {
	l := newLiteSales(t)
	l.e.addUser("mgr2", false, "business_manager")
	mj := l.e.mustLogin("mgr2")
	s1, total := l.unpaidSale(t, "dup-sale-0001")
	s2, _ := l.unpaidSale(t, "dup-sale-0002")
	m := map[string]any{"method": "mpesa_manual", "amount_cents": total / 2, "reference": " qwe123abc "}

	r := l.pay(s1, l.till, "dup-key-000001", m)
	expect(t, r.ResponseRecorder, 201, "")
	pid := decodeInto[struct {
		Payment struct{ ID int64 } `json:"payment"`
	}](t, r.ResponseRecorder).Payment.ID
	// The same code in different letters, on the same or another sale, is the same code.
	expect(t, l.pay(s1, l.till, "dup-key-000002", map[string]any{"method": "mpesa_manual", "amount_cents": 100, "reference": "QWE123ABC"}).ResponseRecorder, 409, "duplicate_reference")
	expect(t, l.pay(s2, l.till, "dup-key-000003", map[string]any{"method": "mpesa_manual", "amount_cents": 100, "reference": "qwe123abc"}).ResponseRecorder, 409, "duplicate_reference")
	// Another method may use the same text; reference is required for non-cash.
	expect(t, l.pay(s2, l.till, "dup-key-000004", map[string]any{"method": "card_manual", "amount_cents": 100, "reference": "QWE123ABC"}).ResponseRecorder, 201, "")
	expect(t, l.pay(s2, l.till, "dup-key-000005", map[string]any{"method": "bank_manual", "amount_cents": 100}).ResponseRecorder, 400, "validation_failed")
	// Once confirmed the code stays used.
	expect(t, l.e.do(req{method: "POST", path: "/api/v1/payments/" + itoa(pid) + "/confirm", jar: mj, header: map[string]string{"Idempotency-Key": "dup-confirm-0001"}, body: map[string]any{}}), 200, "")
	expect(t, l.pay(s2, l.till, "dup-key-000006", map[string]any{"method": "mpesa_manual", "amount_cents": 100, "reference": "QWE123ABC"}).ResponseRecorder, 409, "duplicate_reference")
	// A failed payment frees its code.
	r = l.pay(s1, l.till, "dup-key-000007", map[string]any{"method": "mpesa_manual", "amount_cents": 100, "reference": "FAILME01"})
	expect(t, r.ResponseRecorder, 201, "")
	fid := decodeInto[payResp](t, r.ResponseRecorder).Payment.ID
	expect(t, l.e.do(req{method: "POST", path: "/api/v1/payments/" + itoa(fid) + "/fail", jar: mj, body: map[string]any{"note": "not on the statement"}}), 200, "")
	expect(t, l.pay(s2, l.till, "dup-key-000008", map[string]any{"method": "mpesa_manual", "amount_cents": 100, "reference": "FAILME01"}).ResponseRecorder, 201, "")
	// The database refuses it too, whatever the code does.
	if _, err := l.e.pool.Exec(t.Context(), `INSERT INTO payments (sale_id, purpose, method, status, amount_cents, reference, created_by) SELECT $1, 'sale_payment', 'mpesa_manual', 'pending_confirmation', 100, 'QWE123ABC', id FROM users LIMIT 1`, s2); err == nil {
		t.Fatal("the database must refuse a duplicate (method, reference)")
	}
}

func TestAC02_DuplicateProviderEventIgnored(t *testing.T) {
	e := newEnv(t)
	ins := `INSERT INTO payment_attempts (provider, provider_event_id) VALUES ('mpesa', 'EVT-1') ON CONFLICT (provider, provider_event_id) DO NOTHING`
	for i := 0; i < 3; i++ {
		if _, err := e.pool.Exec(t.Context(), ins); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.count(`SELECT count(*) FROM payment_attempts`); n != 1 {
		t.Fatalf("attempts = %d, want 1", n)
	}
	if _, err := e.pool.Exec(t.Context(), `INSERT INTO payment_attempts (provider, provider_event_id) VALUES ('mpesa', 'EVT-1')`); err == nil {
		t.Fatal("the same provider event must not be recorded twice")
	}
	if _, err := e.pool.Exec(t.Context(), `INSERT INTO payment_attempts (provider, provider_event_id) VALUES ('other', 'EVT-1')`); err != nil {
		t.Fatal("another provider may reuse an event id:", err)
	}
	if _, err := e.pool.Exec(t.Context(), `DELETE FROM payment_attempts`); err == nil {
		t.Fatal("attempts must not be deletable")
	}
}

func TestStage11b_ConfirmAndFailRules(t *testing.T) {
	l := newLiteSales(t)
	l.e.addUser("mgr2", false, "business_manager")
	l.e.addUser("auditor", false, "accountant_auditor")
	mj, aj := l.e.mustLogin("mgr2"), l.e.mustLogin("auditor")
	sale, total := l.unpaidSale(t, "cf-sale-00001")
	r := l.pay(sale, l.till, "cf-key-0000001", map[string]any{"method": "mpesa_manual", "amount_cents": total, "reference": "abc999"})
	expect(t, r.ResponseRecorder, 201, "")
	pid := decodeInto[payResp](t, r.ResponseRecorder).Payment.ID
	// Entered but not confirmed: nothing is collected (AC-20).
	if s := l.sale(t, sale, l.till).Sale; s.PaidCents != 0 || s.PendingCents != total || s.PaymentStatus != "unpaid" {
		t.Fatalf("sale = %+v", s)
	}
	do := func(j jar, action, key string, body map[string]any) *sendResp {
		h := map[string]string{}
		if key != "" {
			h["Idempotency-Key"] = key
		}
		return &sendResp{l.e.do(req{method: "POST", path: "/api/v1/payments/" + itoa(pid) + "/" + action, jar: j, header: h, body: body})}
	}
	expect(t, do(l.till, "confirm", "cf-confirm-001", map[string]any{}).ResponseRecorder, 403, "forbidden") // the person who took it cannot confirm it
	expect(t, do(aj, "confirm", "cf-confirm-001", map[string]any{}).ResponseRecorder, 403, "forbidden")
	expect(t, do(mj, "confirm", "", map[string]any{}).ResponseRecorder, 400, "validation_failed")
	expect(t, do(mj, "fail", "", map[string]any{}).ResponseRecorder, 400, "validation_failed") // a reason is required
	c := do(mj, "confirm", "cf-confirm-001", map[string]any{"note": "seen on statement"})
	expect(t, c.ResponseRecorder, 200, "")
	got := decodeInto[payResp](t, c.ResponseRecorder).Payment
	if got.Status != "confirmed" || got.ConfirmedBy == 0 || got.Note != "seen on statement" {
		t.Fatalf("payment = %+v", got)
	}
	if s := l.sale(t, sale, l.till).Sale; s.PaidCents != total || s.PaymentStatus != "paid" {
		t.Fatalf("sale = %+v", s)
	}
	// A retry returns the stored answer; a new key on a confirmed payment is an invalid transition.
	again := do(mj, "confirm", "cf-confirm-001", map[string]any{"note": "seen on statement"})
	expect(t, again.ResponseRecorder, 200, "")
	if again.Header().Get("Idempotent-Replay") != "true" {
		t.Fatal("expected a replay")
	}
	expect(t, do(mj, "confirm", "cf-confirm-002", map[string]any{}).ResponseRecorder, 409, "invalid_transition")
	expect(t, do(mj, "fail", "", map[string]any{"note": "too late"}).ResponseRecorder, 409, "invalid_transition")
	expect(t, l.e.do(req{method: "POST", path: "/api/v1/payments/99999/fail", jar: mj, body: map[string]any{"note": "x"}}), 404, "not_found")

	// Failing a pending payment leaves the sale unpaid and the money uncollected.
	s2, t2 := l.unpaidSale(t, "cf-sale-00002")
	r = l.pay(s2, l.till, "cf-key-0000002", map[string]any{"method": "card_manual", "amount_cents": t2, "reference": "card-1"})
	expect(t, r.ResponseRecorder, 201, "")
	pid = decodeInto[payResp](t, r.ResponseRecorder).Payment.ID
	f := do(mj, "fail", "", map[string]any{"note": "declined"})
	expect(t, f.ResponseRecorder, 200, "")
	if p := decodeInto[payResp](t, f.ResponseRecorder).Payment; p.Status != "failed" || p.FailedBy == 0 || p.Note != "declined" {
		t.Fatalf("payment = %+v", p)
	}
	if s := l.sale(t, s2, l.till).Sale; s.PaidCents != 0 || s.PendingCents != 0 || s.PaymentStatus != "unpaid" {
		t.Fatalf("sale = %+v", s)
	}
	// ...and the balance can be paid again.
	expect(t, l.pay(s2, l.till, "cf-key-0000003", map[string]any{"method": "cash", "amount_cents": t2}).ResponseRecorder, 201, "")
	for _, a := range []string{"payment.recorded", "payment.confirmed", "payment.failed"} {
		if l.e.auditN(a) == 0 {
			t.Errorf("no audit row for %s", a)
		}
	}
}

func TestStage11b_AddPaymentRules(t *testing.T) {
	l := newLiteSales(t)
	sale, total := l.unpaidSale(t, "ap-sale-00001")
	for name, b := range map[string]map[string]any{
		"zero":      {"method": "cash", "amount_cents": 0},
		"negative":  {"method": "cash", "amount_cents": -5},
		"method":    {"method": "room_charge", "amount_cents": 10},
		"too much":  {"method": "cash", "amount_cents": total + 1},
		"unknown":   {"method": "cash", "amount_cents": 10, "sale_id": 4},
		"mpesa/ref": {"method": "mpesa_manual", "amount_cents": 10},
	} {
		if w := l.pay(sale, l.till, "ap-key-"+name+"-0001", b); w.Code != 400 {
			t.Errorf("%s: status %d: %s", name, w.Code, w.Body.String())
		}
	}
	expect(t, l.pay(sale, l.wa, "ap-key-waiter-001", map[string]any{"method": "cash", "amount_cents": 10}).ResponseRecorder, 403, "forbidden")
	expect(t, l.pay(99999, l.till, "ap-key-missing-01", map[string]any{"method": "cash", "amount_cents": 10}).ResponseRecorder, 404, "not_found")
	// Pending money counts against the balance, so the same balance cannot be promised twice.
	expect(t, l.pay(sale, l.till, "ap-key-pend-0001", map[string]any{"method": "mpesa_manual", "amount_cents": total, "reference": "pp1"}).ResponseRecorder, 201, "")
	expect(t, l.pay(sale, l.till, "ap-key-pend-0002", map[string]any{"method": "cash", "amount_cents": 1}).ResponseRecorder, 400, "validation_failed")
}

func TestStage11b_ListsScopeFiltersPaging(t *testing.T) {
	l := newLiteSales(t)
	l.e.addUser("barman", false, "bartender")
	l.e.addUser("auditor", false, "accountant_auditor")
	l.e.addUser("cook", false, "kitchen_staff")
	bj, aj, kj := l.e.mustLogin("barman"), l.e.mustLogin("auditor"), l.e.mustLogin("cook")
	var ids []int64
	for i := 0; i < 3; i++ {
		s, total := l.unpaidSale(t, "ls-sale-0000"+itoa(int64(i)))
		ids = append(ids, s)
		m := "cash"
		ref := ""
		if i == 1 {
			m, ref = "mpesa_manual", "LST"+itoa(int64(i))
		}
		expect(t, l.pay(s, l.till, "ls-pay-0000"+itoa(int64(i)), map[string]any{"method": m, "amount_cents": total, "reference": ref}).ResponseRecorder, 201, "")
	}
	get := func(j jar, path string) *sendResp { return &sendResp{l.e.do(req{method: "GET", path: path, jar: j})} }
	w := get(l.till, "/api/v1/sales")
	expect(t, w.ResponseRecorder, 200, "")
	all := decodeInto[pageResp](t, w.ResponseRecorder).Sales
	if len(all) != 3 || all[0].ID != ids[2] {
		t.Fatalf("sales = %+v", all)
	}
	if d := decodeInto[pageResp](t, get(l.till, "/api/v1/sales?payment_status=paid").ResponseRecorder).Sales; len(d) != 2 {
		t.Fatalf("paid sales = %+v", d)
	}
	if d := decodeInto[pageResp](t, get(aj, "/api/v1/sales?payment_status=unpaid").ResponseRecorder).Sales; len(d) != 1 || d[0].PendingCents == 0 {
		t.Fatalf("unpaid sales = %+v", d)
	}
	// Paging.
	p1 := decodeInto[pageResp](t, get(l.till, "/api/v1/sales?limit=2").ResponseRecorder)
	if len(p1.Sales) != 2 || p1.NextCursor == "" {
		t.Fatalf("page 1 = %+v", p1)
	}
	p2 := decodeInto[pageResp](t, get(l.till, "/api/v1/sales?limit=2&cursor="+p1.NextCursor).ResponseRecorder)
	if len(p2.Sales) != 1 || p2.NextCursor != "" || p2.Sales[0].ID != ids[0] {
		t.Fatalf("page 2 = %+v", p2)
	}
	expect(t, get(l.till, "/api/v1/sales?payment_status=bogus").ResponseRecorder, 400, "validation_failed")
	expect(t, get(l.till, "/api/v1/sales?business_date=yesterday").ResponseRecorder, 400, "validation_failed")
	expect(t, get(l.till, "/api/v1/sales?limit=x").ResponseRecorder, 400, "validation_failed")

	// Payments.
	pays := decodeInto[pageResp](t, get(l.till, "/api/v1/payments").ResponseRecorder).Payments
	if len(pays) != 3 {
		t.Fatalf("payments = %+v", pays)
	}
	if d := decodeInto[pageResp](t, get(aj, "/api/v1/payments?status=pending_confirmation&method=mpesa_manual").ResponseRecorder).Payments; len(d) != 1 {
		t.Fatalf("pending mpesa = %+v", d)
	}
	if d := decodeInto[pageResp](t, get(aj, "/api/v1/payments?sale_id="+itoa(ids[0])).ResponseRecorder).Payments; len(d) != 1 {
		t.Fatalf("by sale = %+v", d)
	}
	expect(t, get(aj, "/api/v1/payments?status=bogus").ResponseRecorder, 400, "validation_failed")
	// A limited role sees only its own, and a role without sales.view sees nothing.
	if d := decodeInto[pageResp](t, get(bj, "/api/v1/sales").ResponseRecorder).Sales; len(d) != 0 {
		t.Fatalf("a bartender must not see other people's sales: %+v", d)
	}
	if d := decodeInto[pageResp](t, get(bj, "/api/v1/payments").ResponseRecorder).Payments; len(d) != 0 {
		t.Fatalf("a bartender must not see other people's payments: %+v", d)
	}
	expect(t, get(kj, "/api/v1/sales").ResponseRecorder, 403, "forbidden")
	expect(t, get(kj, "/api/v1/payments").ResponseRecorder, 403, "forbidden")
}
