package handler_test

import (
	"fmt"
	"sync"
	"testing"
)

type sumResp struct {
	Summary struct {
		Shift struct {
			ID                int64  `json:"id"`
			UserID            int64  `json:"user_id"`
			Status            string `json:"status"`
			OpeningFloatCents int64  `json:"opening_float_cents"`
			DeclaredCashCents *int64 `json:"declared_cash_cents"`
			ExpectedCashCents *int64 `json:"expected_cash_cents"`
			VarianceCents     *int64 `json:"variance_cents"`
		} `json:"shift"`
		Cash struct {
			Opening  int64 `json:"opening_float_cents"`
			CashPaid int64 `json:"cash_payments_cents"`
			PaidIn   int64 `json:"paid_in_cents"`
			PaidOut  int64 `json:"paid_out_cents"`
			Expenses int64 `json:"expenses_cents"`
			Expected int64 `json:"expected_cash_cents"`
		} `json:"cash"`
		Pending []struct {
			Method      string `json:"method"`
			AmountCents int64  `json:"amount_cents"`
		} `json:"pending_not_cash"`
		Movements []struct{ Kind string } `json:"movements"`
	} `json:"summary"`
}

// The close endpoint answers with the summary itself.
type closeResp struct {
	Shift struct {
		Status            string `json:"status"`
		DeclaredCashCents *int64 `json:"declared_cash_cents"`
		ExpectedCashCents *int64 `json:"expected_cash_cents"`
		VarianceCents     *int64 `json:"variance_cents"`
	} `json:"shift"`
}

type shiftTest struct {
	*liteSales
	mgr jar
	n   int
}

func newShiftTest(t *testing.T) *shiftTest {
	t.Helper()
	l := newLiteSales(t)
	l.e.addUser("mgr2", false, "business_manager")
	return &shiftTest{liteSales: l, mgr: l.e.mustLogin("mgr2")}
}

func (s *shiftTest) key(p string) string { s.n++; return fmt.Sprintf("%s-%04d-shift", p, s.n) }

func (s *shiftTest) post(j jar, path, prefix string, body map[string]any) *sendResp {
	return &sendResp{s.e.do(req{method: "POST", path: path, jar: j, header: map[string]string{"Idempotency-Key": s.key(prefix)}, body: body})}
}

func (s *shiftTest) move(j jar, shift int64, kind string, cents int64, reason string) *sendResp {
	return s.post(j, "/api/v1/shifts/"+itoa(shift)+"/cash-movements", "mv", map[string]any{"kind": kind, "amount_cents": cents, "reason": reason})
}

func (s *shiftTest) summary(t *testing.T, j jar, shift int64) sumResp {
	t.Helper()
	w := s.e.do(req{method: "GET", path: "/api/v1/shifts/" + itoa(shift) + "/summary", jar: j})
	expect(t, w, 200, "")
	return decodeInto[sumResp](t, w)
}

func (s *shiftTest) closeShift(j jar, shift int64, declared int64) *sendResp {
	return s.post(j, "/api/v1/shifts/"+itoa(shift)+"/close", "close", map[string]any{"declared_cash_cents": declared})
}

// freshShift gives the till a new shift with the given float (the helper's own shift is closed first).
func (s *shiftTest) freshShift(t *testing.T, float int64) int64 {
	t.Helper()
	expect(t, s.closeShift(s.till, s.shift, 0).ResponseRecorder, 200, "")
	s.shift = s.e.openShift(t, s.till, float)
	return s.shift
}

func TestAC09_ExpectedCashFromFixtures(t *testing.T) {
	s := newShiftTest(t)
	sh := s.freshShift(t, 50000)
	// A cash sale paid in full, a part-cash sale, and a sale paid by M-Pesa that is not cash.
	a, ta := s.unpaidSale(t, "ec-sale-00001")
	expect(t, s.pay(a, s.till, s.key("pay"), map[string]any{"method": "cash", "amount_cents": ta}).ResponseRecorder, 201, "")
	b, tb := s.unpaidSale(t, "ec-sale-00002")
	expect(t, s.pay(b, s.till, s.key("pay"), map[string]any{"method": "cash", "amount_cents": tb / 2}).ResponseRecorder, 201, "")
	c, _ := s.unpaidSale(t, "ec-sale-00003")
	expect(t, s.pay(c, s.till, s.key("pay"), map[string]any{"method": "mpesa_manual", "amount_cents": 5000, "reference": "EC1"}).ResponseRecorder, 201, "")
	expect(t, s.move(s.till, sh, "paid_in", 2000, "float top-up").ResponseRecorder, 201, "")
	expect(t, s.move(s.till, sh, "paid_out", 1500, "change for the bar").ResponseRecorder, 201, "")
	expect(t, s.move(s.till, sh, "expense", 500, "ice").ResponseRecorder, 201, "")

	want := int64(50000) + ta + tb/2 + 2000 - 1500 - 500
	sm := s.summary(t, s.till, sh).Summary
	if sm.Cash.Expected != want || sm.Cash.CashPaid != ta+tb/2 || sm.Cash.PaidIn != 2000 || sm.Cash.PaidOut != 1500 || sm.Cash.Expenses != 500 || sm.Cash.Opening != 50000 || len(sm.Movements) != 3 {
		t.Fatalf("summary = %+v, want expected %d", sm.Cash, want)
	}
	if sm.Shift.ExpectedCashCents != nil {
		t.Fatal("an open shift stores no expected figure; it is derived from rows")
	}
	// The current-shift endpoint shows the same figure.
	w := s.e.do(req{method: "GET", path: "/api/v1/shifts/current", jar: s.till})
	expect(t, w, 200, "")
	if cur := decodeInto[sumResp](t, w).Summary; cur.Cash.Expected != want || cur.Shift.ID != sh {
		t.Fatalf("current = %+v", cur)
	}
	// The sale carries the shift.
	if got := s.sale(t, a, s.till).Sale; got.ID != a {
		t.Fatal("sale not found")
	}
	if n := s.e.count(`SELECT count(*) FROM sales WHERE shift_id = $1`, sh); n != 3 {
		t.Fatalf("sales on the shift = %d, want 3", n)
	}
}

func TestAC09_VarianceStoredNotAbsorbed(t *testing.T) {
	s := newShiftTest(t)
	sh := s.freshShift(t, 10000)
	a, ta := s.unpaidSale(t, "vs-sale-00001")
	expect(t, s.pay(a, s.till, s.key("pay"), map[string]any{"method": "cash", "amount_cents": ta}).ResponseRecorder, 201, "")
	expected := 10000 + ta

	expect(t, s.post(s.till, "/api/v1/shifts/"+itoa(sh)+"/close", "close", map[string]any{}).ResponseRecorder, 400, "validation_failed") // the count is required
	expect(t, s.post(s.till, "/api/v1/shifts/"+itoa(sh)+"/close", "close", map[string]any{"declared_cash_cents": -1}).ResponseRecorder, 400, "validation_failed")
	key := "close-same-key-1"
	body := map[string]any{"declared_cash_cents": expected - 1000, "notes": "short"}
	r := &sendResp{s.e.do(req{method: "POST", path: "/api/v1/shifts/" + itoa(sh) + "/close", jar: s.till, header: map[string]string{"Idempotency-Key": key}, body: body})}
	expect(t, r.ResponseRecorder, 200, "")
	got := decodeInto[closeResp](t, r.ResponseRecorder).Shift
	if got.Status != "closed" || *got.ExpectedCashCents != expected || *got.DeclaredCashCents != expected-1000 || *got.VarianceCents != -1000 {
		t.Fatalf("closed = %+v (expected %d)", got, expected)
	}
	// A retry returns the stored answer.
	again := &sendResp{s.e.do(req{method: "POST", path: "/api/v1/shifts/" + itoa(sh) + "/close", jar: s.till, header: map[string]string{"Idempotency-Key": key}, body: body})}
	expect(t, again.ResponseRecorder, 200, "")
	if again.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("expected a replay")
	}
	// Closed means closed: no second close, no cash movements, no cash payments, no edits of the figures.
	expect(t, s.closeShift(s.till, sh, expected).ResponseRecorder, 409, "invalid_transition")
	expect(t, s.move(s.till, sh, "expense", 100, "late").ResponseRecorder, 409, "shift_closed")
	b, tb := s.unpaidSale(t, "vs-sale-00002")
	expect(t, s.pay(b, s.till, s.key("pay"), map[string]any{"method": "cash", "amount_cents": tb}).ResponseRecorder, 409, "shift_required")
	if _, err := s.e.pool.Exec(t.Context(), `UPDATE cashier_shifts SET declared_cash_cents = $2, variance_cents = 0 WHERE id = $1`, sh, expected); err == nil {
		t.Fatal("the database must refuse to absorb a variance")
	}
	if _, err := s.e.pool.Exec(t.Context(), `INSERT INTO payments (sale_id, purpose, method, status, amount_cents, created_by, confirmed_by, confirmed_at, shift_id)
		SELECT $1, 'sale_payment', 'cash', 'confirmed', 100, id, id, now(), $2 FROM users LIMIT 1`, b, sh); err == nil {
		t.Fatal("the database must refuse a payment on a closed shift")
	}
	if _, err := s.e.pool.Exec(t.Context(), `INSERT INTO cash_movements (shift_id, kind, amount_cents, reason, created_by) SELECT $1, 'expense', 1, 'x', id FROM users LIMIT 1`, sh); err == nil {
		t.Fatal("the database must refuse a cash movement on a closed shift")
	}
	sm := s.summary(t, s.till, sh).Summary
	if sm.Shift.Status != "closed" || *sm.Shift.VarianceCents != -1000 || sm.Cash.Expected != expected {
		t.Fatalf("summary after close = %+v", sm)
	}
	// A new shift can be opened after a close.
	s.e.openShift(t, s.till, 0)
}

func TestAC09_PendingMpesaExcludedFromCash(t *testing.T) {
	s := newShiftTest(t)
	sh := s.freshShift(t, 20000)
	a, ta := s.unpaidSale(t, "pm-sale-00001")
	r := s.pay(a, s.till, s.key("pay"), map[string]any{"method": "mpesa_manual", "amount_cents": ta, "reference": "PM1"})
	expect(t, r.ResponseRecorder, 201, "")
	pid := decodeInto[payResp](t, r.ResponseRecorder).Payment.ID
	sm := s.summary(t, s.till, sh).Summary
	if sm.Cash.Expected != 20000 || len(sm.Pending) != 1 || sm.Pending[0].Method != "mpesa_manual" || sm.Pending[0].AmountCents != ta {
		t.Fatalf("pending M-Pesa must be listed apart and not counted as cash: %+v", sm)
	}
	// Even once confirmed, M-Pesa is not drawer cash.
	expect(t, s.e.do(req{method: "POST", path: "/api/v1/payments/" + itoa(pid) + "/confirm", jar: s.mgr, header: map[string]string{"Idempotency-Key": s.key("cf")}, body: map[string]any{}}), 200, "")
	sm = s.summary(t, s.till, sh).Summary
	if sm.Cash.Expected != 20000 || sm.Cash.CashPaid != 0 || len(sm.Pending) != 0 {
		t.Fatalf("after confirm: %+v", sm)
	}
	// A second payment left pending does not stop the close (S-21) and is shown on the closed summary.
	b, tb := s.unpaidSale(t, "pm-sale-00002")
	expect(t, s.pay(b, s.till, s.key("pay"), map[string]any{"method": "card_manual", "amount_cents": tb, "reference": "PM2"}).ResponseRecorder, 201, "")
	expect(t, s.closeShift(s.till, sh, 20000).ResponseRecorder, 200, "")
	if cl := s.summary(t, s.till, sh).Summary; len(cl.Pending) != 1 || *cl.Shift.VarianceCents != 0 {
		t.Fatalf("closed summary = %+v", cl)
	}
}

func TestStage12_ShiftRules(t *testing.T) {
	s := newShiftTest(t)
	s.e.addUser("till2", false, "cashier")
	s.e.addUser("auditor", false, "accountant_auditor")
	t2, aj := s.e.mustLogin("till2"), s.e.mustLogin("auditor")
	open := func(j jar, key string, body map[string]any) *sendResp {
		h := map[string]string{}
		if key != "" {
			h["Idempotency-Key"] = key
		}
		return &sendResp{s.e.do(req{method: "POST", path: "/api/v1/shifts/open", jar: j, header: h, body: body})}
	}
	// One open shift per person; validation; the key; who may open.
	expect(t, open(s.till, "open-dup-000001", map[string]any{"opening_float_cents": 0}).ResponseRecorder, 409, "shift_already_open")
	expect(t, open(t2, "", map[string]any{"opening_float_cents": 0}).ResponseRecorder, 400, "validation_failed")
	expect(t, open(t2, "open-neg-000001", map[string]any{"opening_float_cents": -5}).ResponseRecorder, 400, "validation_failed")
	expect(t, open(aj, "open-aud-000001", map[string]any{"opening_float_cents": 0}).ResponseRecorder, 403, "forbidden")
	expect(t, open(s.wa, "open-wai-000001", map[string]any{"opening_float_cents": 0}).ResponseRecorder, 403, "forbidden")
	r := open(t2, "open-ok-0000001", map[string]any{"opening_float_cents": 1000})
	expect(t, r.ResponseRecorder, 201, "")
	again := open(t2, "open-ok-0000001", map[string]any{"opening_float_cents": 1000})
	expect(t, again.ResponseRecorder, 201, "")
	if again.Header().Get("Idempotent-Replayed") != "true" || s.e.count(`SELECT count(*) FROM cashier_shifts WHERE user_id = (SELECT id FROM users WHERE username = 'till2')`) != 1 {
		t.Fatal("a retry must not open a second shift")
	}
	if _, err := s.e.pool.Exec(t.Context(), `INSERT INTO cashier_shifts (user_id, opening_float_cents) SELECT id, 0 FROM users WHERE username = 'till2'`); err == nil {
		t.Fatal("the database must allow one open shift per user")
	}
	t2shift := decodeInto[struct {
		Shift struct{ ID int64 } `json:"shift"`
	}](t, r.ResponseRecorder).Shift.ID

	// Scope: another cashier's shift is invisible; the manager sees and may close any.
	expect(t, s.e.do(req{method: "GET", path: "/api/v1/shifts/" + itoa(t2shift) + "/summary", jar: s.till}), 404, "not_found")
	expect(t, s.move(s.till, t2shift, "expense", 100, "x").ResponseRecorder, 404, "not_found")
	expect(t, s.closeShift(s.till, t2shift, 0).ResponseRecorder, 404, "not_found")
	expect(t, s.move(s.mgr, t2shift, "paid_in", 100, "owner top-up").ResponseRecorder, 201, "")
	var list struct {
		Shifts []struct {
			ID int64 `json:"id"`
		} `json:"shifts"`
	}
	list = decodeInto[struct {
		Shifts []struct {
			ID int64 `json:"id"`
		} `json:"shifts"`
	}](t, s.e.do(req{method: "GET", path: "/api/v1/shifts", jar: s.till}))
	if len(list.Shifts) != 1 || list.Shifts[0].ID != s.shift {
		t.Fatalf("a cashier lists only their own shifts: %+v", list)
	}
	list = decodeInto[struct {
		Shifts []struct {
			ID int64 `json:"id"`
		} `json:"shifts"`
	}](t, s.e.do(req{method: "GET", path: "/api/v1/shifts?status=open", jar: s.mgr}))
	if len(list.Shifts) != 2 {
		t.Fatalf("the manager lists every shift: %+v", list)
	}
	expect(t, s.e.do(req{method: "GET", path: "/api/v1/shifts?status=bogus", jar: s.mgr}), 400, "validation_failed")
	expect(t, s.e.do(req{method: "GET", path: "/api/v1/shifts", jar: aj}), 403, "forbidden")

	// Movement validation.
	expect(t, s.move(s.till, s.shift, "refund", 100, "x").ResponseRecorder, 400, "validation_failed")
	expect(t, s.move(s.till, s.shift, "expense", 0, "x").ResponseRecorder, 400, "validation_failed")
	expect(t, s.move(s.till, s.shift, "expense", 100, "  ").ResponseRecorder, 400, "validation_failed")
	if _, err := s.e.pool.Exec(t.Context(), `UPDATE cash_movements SET amount_cents = 1`); err == nil {
		t.Fatal("cash movements must be immutable")
	}

	// Cash needs an open shift; a sale without cash does not.
	s.e.addUser("till3", false, "cashier")
	t3 := s.e.mustLogin("till3")
	o := s.order(t)
	body := map[string]any{"payments": []map[string]any{{"method": "cash", "amount_cents": 100}}}
	w := &sendResp{s.e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(o) + "/finalise", jar: t3, header: map[string]string{"Idempotency-Key": "no-shift-fin-01"}, body: body})}
	expect(t, w.ResponseRecorder, 409, "shift_required")
	if s.e.count(`SELECT count(*) FROM sales WHERE order_id = $1`, o) != 0 {
		t.Fatal("a refused finalise must leave no sale")
	}
	w = &sendResp{s.e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(o) + "/finalise", jar: t3, header: map[string]string{"Idempotency-Key": "no-shift-fin-02"}, body: map[string]any{}})}
	expect(t, w.ResponseRecorder, 201, "")
	sale := decodeInto[saleResp](t, w.ResponseRecorder).Sale.ID
	pr := &sendResp{s.e.do(req{method: "POST", path: "/api/v1/sales/" + itoa(sale) + "/payments", jar: t3, header: map[string]string{"Idempotency-Key": "no-shift-pay-01"}, body: map[string]any{"method": "cash", "amount_cents": 100}})}
	expect(t, pr.ResponseRecorder, 409, "shift_required")
	if s.e.count(`SELECT count(*) FROM sales WHERE id = $1 AND shift_id IS NULL`, sale) != 1 {
		t.Fatal("a sale taken with no open shift carries none")
	}

	// Reconcile: only after close, only with payments.reconcile.
	expect(t, s.post(s.mgr, "/api/v1/shifts/"+itoa(t2shift)+"/reconcile", "rc", map[string]any{}).ResponseRecorder, 409, "invalid_transition")
	expect(t, s.closeShift(s.mgr, t2shift, 1100).ResponseRecorder, 200, "")
	expect(t, s.post(s.till, "/api/v1/shifts/"+itoa(t2shift)+"/reconcile", "rc", map[string]any{}).ResponseRecorder, 403, "forbidden")
	rc := s.post(s.mgr, "/api/v1/shifts/"+itoa(t2shift)+"/reconcile", "rc", map[string]any{"note": "counted twice"})
	expect(t, rc.ResponseRecorder, 200, "")
	if n := s.e.count(`SELECT count(*) FROM cashier_shifts WHERE id = $1 AND status = 'reconciled' AND reconciled_by IS NOT NULL`, t2shift); n != 1 {
		t.Fatal("shift not reconciled")
	}
	expect(t, s.post(s.mgr, "/api/v1/shifts/"+itoa(t2shift)+"/reconcile", "rc", map[string]any{}).ResponseRecorder, 409, "invalid_transition")
	expect(t, s.e.do(req{method: "GET", path: "/api/v1/shifts/current", jar: aj}), 403, "forbidden")
	for _, a := range []string{"shift.opened", "shift.cash_movement", "shift.closed", "shift.reconciled"} {
		if s.e.auditN(a) == 0 {
			t.Errorf("no audit row for %s", a)
		}
	}
	// No open shift: current answers null, not an error.
	nw := s.e.do(req{method: "GET", path: "/api/v1/shifts/current", jar: t3})
	expect(t, nw, 200, "")
}

// A payment racing a close either lands on the shift before the close, and is counted, or is refused: the stored
// expected figure always matches the rows.
func TestAC09_CloseRacingPaymentsStaysConsistent(t *testing.T) {
	s := newShiftTest(t)
	sh := s.freshShift(t, 1000)
	sale, _ := s.unpaidSale(t, "race-sale-0001")
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.pay(sale, s.till, fmt.Sprintf("race-pay-%04d-x", i), map[string]any{"method": "cash", "amount_cents": 100})
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.closeShift(s.till, sh, 0)
	}()
	wg.Wait()
	var expected, cash int64
	if err := s.e.pool.QueryRow(t.Context(), `SELECT expected_cash_cents FROM cashier_shifts WHERE id = $1 AND status = 'closed'`, sh).Scan(&expected); err != nil {
		t.Fatal(err)
	}
	if err := s.e.pool.QueryRow(t.Context(), `SELECT COALESCE(sum(amount_cents), 0) FROM payments WHERE shift_id = $1 AND method = 'cash' AND status = 'confirmed'`, sh).Scan(&cash); err != nil {
		t.Fatal(err)
	}
	if expected != 1000+cash {
		t.Fatalf("expected %d does not match float + cash on the shift (%d)", expected, 1000+cash)
	}
}
