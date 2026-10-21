package handler_test

import (
	"os"
	"strings"
	"testing"
)

// Stage 26: the expenses ledger, the estimated profit report and the end-of-day closing summary (ASSUMPTIONS.md S-111 to S-114).

type expenseOut struct {
	ID          int64   `json:"id"`
	Date        string  `json:"expense_date"`
	Description string  `json:"description"`
	AmountCents int64   `json:"amount_cents"`
	Voided      bool    `json:"voided"`
	VoidReason  *string `json:"void_reason"`
}

type expenseList struct {
	Expenses   []expenseOut `json:"expenses"`
	More       bool         `json:"more"`
	Categories []string     `json:"categories"`
	Methods    []string     `json:"methods"`
}

func (r *roomsEnv) expense(j jar, key string, body map[string]any, want int, code string) expenseOut {
	r.t.Helper()
	w := r.e.do(req{method: "POST", path: "/api/v1/expenses", jar: j, header: map[string]string{"Idempotency-Key": key}, body: body})
	expect(r.t, w, want, code)
	if want >= 400 {
		return expenseOut{}
	}
	return decodeInto[struct {
		Expense expenseOut `json:"expense"`
	}](r.t, w).Expense
}

func (r *roomsEnv) rent(amount int64) map[string]any {
	return map[string]any{"expense_date": r.day(0), "description": "Rent for October", "category": "rent", "amount_cents": amount, "paid_by": "Diana (own pocket)", "paid_with": "mpesa"}
}

func TestStage26_ExpenseLedger(t *testing.T) {
	r := newRoomsEnv(t)
	key := newKey()
	x := r.expense(r.mgr, key, r.rent(150000), 201, "")
	if x.AmountCents != 150000 || x.Voided || x.Date != r.day(0) {
		t.Fatalf("expense: %+v", x)
	}
	// The same key again is the same answer and adds nothing.
	w := r.e.do(req{method: "POST", path: "/api/v1/expenses", jar: r.mgr, header: map[string]string{"Idempotency-Key": key}, body: r.rent(150000)})
	expect(t, w, 201, "")
	if w.Header().Get("Idempotent-Replayed") != "true" || r.e.count(`SELECT count(*) FROM expenses`) != 1 {
		t.Fatal("a retry with the same key added a second expense or was not marked as a replay")
	}
	r.expense(r.mgr, key, r.rent(99999), 422, "idempotency_key_reused")

	// What is refused.
	bad := func(change map[string]any, field string) {
		t.Helper()
		b := r.rent(1000)
		for k, v := range change {
			b[k] = v
		}
		w := r.e.do(req{method: "POST", path: "/api/v1/expenses", jar: r.mgr, header: map[string]string{"Idempotency-Key": newKey()}, body: b})
		expect(t, w, 400, "validation_failed")
		if !strings.Contains(w.Body.String(), field) {
			t.Fatalf("expected a complaint about %s: %s", field, w.Body.String())
		}
	}
	bad(map[string]any{"expense_date": r.day(1)}, "expense_date")
	bad(map[string]any{"expense_date": r.day(-401)}, "expense_date")
	bad(map[string]any{"expense_date": "yesterday"}, "expense_date")
	bad(map[string]any{"amount_cents": 0}, "amount_cents")
	bad(map[string]any{"amount_cents": -5}, "amount_cents")
	bad(map[string]any{"category": "fun"}, "category")
	bad(map[string]any{"paid_with": "cheque"}, "paid_with")
	bad(map[string]any{"paid_by": "  "}, "paid_by")
	bad(map[string]any{"description": ""}, "description")
	r.e.do(req{method: "POST", path: "/api/v1/expenses", jar: r.mgr, body: r.rent(1000)}) // no key: 400 from the middleware
	if r.e.count(`SELECT count(*) FROM expenses`) != 1 {
		t.Fatal("a refused expense was saved")
	}

	// Who may: the owner's business manager role only. Nobody else sees or changes the ledger.
	for _, j := range []jar{r.cash, r.recep} {
		r.call(j, "GET", "/api/v1/expenses", nil, 403, "forbidden")
		r.expense(j, newKey(), r.rent(1000), 403, "forbidden")
		r.call(j, "POST", "/api/v1/expenses/"+itoa(x.ID)+"/void", map[string]any{"reason": "x"}, 403, "forbidden")
	}

	// A wrong entry is voided, never edited; it stays on record.
	r.call(r.mgr, "POST", "/api/v1/expenses/"+itoa(x.ID)+"/void", map[string]any{"reason": " "}, 400, "validation_failed")
	r.call(r.mgr, "POST", "/api/v1/expenses/999999/void", map[string]any{"reason": "typo"}, 404, "not_found")
	v := decodeInto[struct {
		Expense expenseOut `json:"expense"`
	}](t, r.call(r.mgr, "POST", "/api/v1/expenses/"+itoa(x.ID)+"/void", map[string]any{"reason": "Entered twice"}, 200, "")).Expense
	if !v.Voided || v.VoidReason == nil || *v.VoidReason != "Entered twice" {
		t.Fatalf("voided: %+v", v)
	}
	r.call(r.mgr, "POST", "/api/v1/expenses/"+itoa(x.ID)+"/void", map[string]any{"reason": "again"}, 409, "already_voided")
	l := decodeInto[expenseList](t, r.call(r.mgr, "GET", "/api/v1/expenses", nil, 200, ""))
	if len(l.Expenses) != 0 || len(l.Categories) != 9 || len(l.Methods) != 5 {
		t.Fatalf("default list must hide voided entries and carry the choices: %+v", l)
	}
	if l = decodeInto[expenseList](t, r.call(r.mgr, "GET", "/api/v1/expenses?include_voided=true", nil, 200, "")); len(l.Expenses) != 1 {
		t.Fatalf("include_voided: %+v", l)
	}
	r.call(r.mgr, "GET", "/api/v1/expenses?from="+r.day(0)+"&to="+r.day(-1), nil, 400, "validation_failed")
	r.call(r.mgr, "GET", "/api/v1/expenses?from="+r.day(-400)+"&to="+r.day(0), nil, 400, "validation_failed")

	// The database itself keeps the record fixed.
	if _, err := r.e.pool.Exec(t.Context(), `UPDATE expenses SET amount_cents = 1 WHERE id = $1`, x.ID); err == nil {
		t.Fatal("a voided expense was edited")
	}
	y := r.expense(r.mgr, newKey(), r.rent(2000), 201, "")
	if _, err := r.e.pool.Exec(t.Context(), `UPDATE expenses SET amount_cents = 1 WHERE id = $1`, y.ID); err == nil {
		t.Fatal("an expense was edited")
	}
	if _, err := r.e.pool.Exec(t.Context(), `DELETE FROM expenses WHERE id = $1`, y.ID); err == nil {
		t.Fatal("an expense was deleted")
	}
	if r.e.count(`SELECT count(*) FROM audit_logs WHERE action = 'expense.created'`) != 2 || r.e.count(`SELECT count(*) FROM audit_logs WHERE action = 'expense.voided'`) != 1 {
		t.Fatal("expense entries and voids must be audited")
	}
}

func TestStage26_ProfitAndClosingSummary(t *testing.T) {
	v := newInvEnv(t)
	r := &roomsEnv{t: t, e: v.e, mgr: v.mgr, cash: v.cash}
	today := r.day(0)

	v.e.openShift(t, v.cash, 5000)
	mgrShift := v.e.openShift(t, v.mgr, 0)
	sale := v.sell(v.cash, nil, cash(24000), [2]int64{v.tea, 3})
	if sale.TotalCents != 24000 {
		t.Fatalf("test fixture changed: three teas cost %d", sale.TotalCents)
	}
	// Cash spent from the drawer is entered on the shift; the rest goes in the ledger.
	w := v.e.do(req{method: "POST", path: "/api/v1/shifts/" + itoa(mgrShift) + "/cash-movements", jar: v.mgr, header: map[string]string{"Idempotency-Key": newKey()}, body: map[string]any{"kind": "expense", "amount_cents": 2000, "reason": "Charcoal"}})
	expect(t, w, 201, "")
	gone := r.expense(v.mgr, newKey(), r.rent(7000), 201, "")
	r.expense(v.mgr, newKey(), map[string]any{"expense_date": today, "description": "Fuel for the generator", "category": "utilities", "amount_cents": 5000, "paid_by": "Joseph", "paid_with": "cash"}, 201, "")
	r.call(v.mgr, "POST", "/api/v1/expenses/"+itoa(gone.ID)+"/void", map[string]any{"reason": "Wrong month"}, 200, "")

	// Stock bought and received counts as what the delivery really cost.
	glass := v.item("Pint glass", v.pc)
	sup := v.supplier(v.store, "Glassworks", 201, "")
	p, _ := v.buy(v.store, map[string]any{"supplier_id": sup, "lines": []map[string]any{{"item_id": glass, "qty": 10, "unit_cost_cents": 1500}}}, newKey(), 201, "")
	v.receive(v.store, p.ID, v.mainStore, newKey(), 200, "")

	var tax int64
	if err := v.e.pool.QueryRow(t.Context(), `SELECT COALESCE(sum(tax_cents), 0) FROM sales`).Scan(&tax); err != nil {
		t.Fatal(err)
	}
	get := func(name, query string) reportOut {
		w := v.e.do(req{method: "GET", path: "/api/v1/reports/" + name + query, jar: v.mgr})
		expect(t, w, 200, "")
		return decodeInto[reportOut](t, w)
	}
	prof := get("expenses-profit", "?from="+today+"&to="+today)
	rows, tot := prof.section(t, "profit")
	want := []int64{24000, 0, 24000, -tax, 24000 - tax, -15000, -5000, -2000}
	if len(rows) != len(want) {
		t.Fatalf("profit rows: %v", rows)
	}
	for i, w := range want {
		if num(rows[i][1]) != w {
			t.Fatalf("profit row %d (%v) = %v, want %d", i, rows[i][0], rows[i][1], w)
		}
	}
	if num(tot[1]) != 24000-tax-15000-5000-2000 {
		t.Fatalf("estimated profit %v, want %d", tot[1], 24000-tax-15000-5000-2000)
	}
	if _, ctot := prof.section(t, "by_category"); num(ctot[1]) != 2 || num(ctot[2]) != 7000 {
		t.Fatalf("spending by kind: %v (the voided rent must not count)", ctot)
	}
	if list, ltot := prof.section(t, "expenses"); len(list) != 2 || num(ltot[4]) != 7000 {
		t.Fatalf("expense list: %v %v", list, ltot)
	}
	if srows, stot := prof.section(t, "stock_bought"); len(srows) != 1 || num(stot[2]) != 15000 {
		t.Fatalf("stock received: %v %v", srows, stot)
	}
	// A range with nothing in it is all zeros, not an error.
	if _, z := get("expenses-profit", "?from="+r.day(-20)+"&to="+r.day(-10)).section(t, "profit"); num(z[1]) != 0 {
		t.Fatalf("empty range: %v", z)
	}

	// The closing sheet for one day.
	dc := get("day-close", "?from="+today)
	if _, st := dc.section(t, "days"); num(st[4]) != 24000 {
		t.Fatalf("closing sales: %v", st)
	}
	if _, ct := dc.section(t, "collected"); num(ct[4]) != 24000 {
		t.Fatalf("closing collected: %v", ct)
	}
	if sh, _ := dc.section(t, "shifts"); len(sh) != 2 || num(sh[0][2]) != 5000 || sh[0][3] != "still open" {
		t.Fatalf("closing shifts: %v", sh)
	}
	if mv, _ := dc.section(t, "cash_movements"); len(mv) != 1 || mv[0][0] != "Expenses paid in cash" || num(mv[0][2]) != 2000 {
		t.Fatalf("closing cash movements: %v", mv)
	}
	if ex, et := dc.section(t, "expenses"); len(ex) != 2 || num(et[4]) != 7000 {
		t.Fatalf("closing expenses: %v %v", ex, et)
	}
	for _, key := range []string{"net_by_method", "pending", "paid_out", "discounts", "returns", "nights", "today", "modules", "payment_status"} {
		dc.section(t, key)
	}
	r.call(v.mgr, "GET", "/api/v1/reports/day-close?from="+r.day(-1)+"&to="+today, nil, 400, "validation_failed")

	// Who may read them, and the audited download.
	for _, name := range []string{"expenses-profit", "day-close"} {
		r.call(v.cash, "GET", "/api/v1/reports/"+name, nil, 403, "forbidden")
		r.call(v.wait, "GET", "/api/v1/reports/"+name, nil, 403, "forbidden")
		w := r.call(v.mgr, "GET", "/api/v1/reports/"+name+"?format=csv", nil, 200, "")
		if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/csv") {
			t.Fatalf("%s csv: %s", name, w.Header().Get("Content-Type"))
		}
	}
	if v.e.count(`SELECT count(*) FROM audit_logs WHERE action = 'report.exported' AND entity_id IN ('expenses-profit', 'day-close')`) != 2 {
		t.Fatal("both downloads must be audited")
	}
}

// Bootstrap is not re-run on a live system, so the migration must give the manager role the new permission on its own.
// This runs the grant statements from the migration file itself against a database that lacks them.
func TestStage26_MigrationGrantsThePermission(t *testing.T) {
	e := newEnv(t)
	raw, err := os.ReadFile("../../../database/migrations/0029_expenses.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	grant := sql[strings.Index(sql, "INSERT INTO permissions"):]
	// A live database from before this stage: no row for the permission and no grant.
	for _, q := range []string{`DELETE FROM role_permissions WHERE permission_id = (SELECT id FROM permissions WHERE key = 'expenses.manage')`, `DELETE FROM permissions WHERE key = 'expenses.manage'`} {
		if _, err := e.pool.Exec(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ { // twice: safe to repeat
		if _, err := e.pool.Exec(t.Context(), grant); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.count(`SELECT count(*) FROM role_permissions rp JOIN roles r ON r.id = rp.role_id JOIN permissions p ON p.id = rp.permission_id WHERE p.key = 'expenses.manage' AND r.code = 'business_manager'`); n != 1 {
		t.Fatalf("business manager holds expenses.manage %d times, want once", n)
	}
	if n := e.count(`SELECT count(*) FROM role_permissions rp JOIN permissions p ON p.id = rp.permission_id WHERE p.key = 'expenses.manage'`); n != 1 {
		t.Fatalf("expenses.manage is held by %d roles, want only the business manager", n)
	}
}
