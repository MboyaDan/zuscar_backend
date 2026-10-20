package handler_test

import (
	"encoding/csv"
	"strings"
	"testing"
)

// Stage 25: management reports (docs/api/routes.csv; ASSUMPTIONS.md S-107 to S-110).

type reportOut struct {
	Report struct {
		Name     string   `json:"name"`
		Notes    []string `json:"notes"`
		Sections []struct {
			Key    string  `json:"key"`
			Rows   [][]any `json:"rows"`
			Totals []any   `json:"totals"`
		} `json:"sections"`
	} `json:"report"`
}

func (o reportOut) section(t *testing.T, key string) (rows [][]any, totals []any) {
	t.Helper()
	for _, s := range o.Report.Sections {
		if s.Key == key {
			return s.Rows, s.Totals
		}
	}
	t.Fatalf("report %s has no section %q", o.Report.Name, key)
	return nil, nil
}

func (r *roomsEnv) report(j jar, name, query string, want int, code string) reportOut {
	r.t.Helper()
	w := r.call(j, "GET", "/api/v1/reports/"+name+"?from="+r.day(-30)+"&to="+r.day(30)+query, nil, want, code)
	if want >= 400 {
		return reportOut{}
	}
	return decodeInto[reportOut](r.t, w)
}

func num(v any) int64 { f, _ := v.(float64); return int64(f) }

// sell rings up qty teas as j and returns the sale id (payments nil leaves it unpaid).
func (r *roomsEnv) sell(j jar, tea int64, qty int, payments []map[string]any) int64 {
	r.t.Helper()
	order := r.order(j, tea, qty)
	body := map[string]any{}
	if payments != nil {
		body["payments"] = payments
	}
	w := r.e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(order) + "/finalise", jar: j, header: map[string]string{"Idempotency-Key": newKey()}, body: body})
	expect(r.t, w, 201, "")
	return decodeInto[saleResp](r.t, w).Sale.ID
}

func TestStage25_SalesCollectionsAndBalances(t *testing.T) {
	r := newRoomsEnv(t)
	tea, _, _ := menuFixture(t, r.e, r.mgr)
	r.e.openShift(t, r.mgr, 0)
	room := r.room("701", r.roomType("Standard", 650000, 2))
	ci := r.stay(room, 1)

	// A: 3 teas paid in cash (240.00). B: 2 teas left unpaid (160.00). C: 1 tea charged to the room (80.00).
	a := r.sell(r.mgr, tea, 3, cash(24000))
	b := r.sell(r.mgr, tea, 2, nil)
	r.charge(r.mgr, ci.FolioID, map[string]any{"order_id": r.order(r.mgr, tea, 1)}, newKey(), 201, "")
	// A mobile payment recorded but not confirmed must stay out of every collected figure (AC-20).
	w := r.e.do(req{method: "POST", path: "/api/v1/sales/" + itoa(b) + "/payments", jar: r.mgr, header: map[string]string{"Idempotency-Key": newKey()}, body: map[string]any{"method": "mpesa_manual", "amount_cents": 5000, "reference": "QWE123"}})
	expect(t, w, 201, "")

	sales := r.report(r.mgr, "sales", "", 200, "")
	_, tot := sales.section(t, "days")
	if num(tot[1]) != 3 || num(tot[4]) != 48000 || num(tot[6]) != 8000 || num(tot[7]) != 0 || num(tot[8]) != 48000 {
		t.Fatalf("sales totals: %v", tot)
	}
	col := r.report(r.mgr, "collections", "", 200, "")
	rows, ctot := col.section(t, "collected")
	if num(ctot[4]) != 24000 || len(rows) != 1 {
		t.Fatalf("collected must be the confirmed cash only: %v %v", rows, ctot)
	}
	if prow, _ := col.section(t, "pending"); len(prow) != 1 || num(prow[0][2]) != 5000 {
		t.Fatalf("pending: %v", prow)
	}
	due := r.report(r.mgr, "balances-due", "", 200, "")
	srows, stot := due.section(t, "sales")
	if len(srows) != 1 || num(stot[7]) != 16000 { // B owes 160.00; the unconfirmed 50.00 is not paid
		t.Fatalf("sales due: %v %v", srows, stot)
	}
	if _, ftot := due.section(t, "folios"); num(ftot[8]) != 650000+8000 {
		t.Fatalf("folio due: %v", ftot)
	}

	// Voiding A is a return on the day it was done: net sales drop and the cash paid back shows.
	w = r.e.do(req{method: "POST", path: "/api/v1/sales/" + itoa(a) + "/void", jar: r.mgr, header: map[string]string{"Idempotency-Key": newKey()}, body: map[string]any{"reason": "Wrong order"}})
	expect(t, w, 201, "")
	_, tot = r.report(r.mgr, "sales", "", 200, "").section(t, "days")
	if num(tot[4]) != 48000 || num(tot[7]) != 24000 || num(tot[8]) != 24000 {
		t.Fatalf("sales totals after the void: %v", tot)
	}
	paid, ptot := r.report(r.mgr, "collections", "", 200, "").section(t, "paid_out")
	if len(paid) != 1 || num(ptot[3]) != 24000 {
		t.Fatalf("paid out: %v %v", paid, ptot)
	}
	dv := r.report(r.mgr, "discounts-voids-refunds", "", 200, "")
	if rr, rt := dv.section(t, "returns"); len(rr) != 1 || rr[0][2] != "Void" || rr[0][5] != "Wrong order" || num(rt[4]) != 24000 {
		t.Fatalf("returns: %v %v", rr, rt)
	}

	// Rooms: one night sold at 6,500.00 and the house counts.
	rm := r.report(r.mgr, "rooms", "", 200, "")
	_, rt := rm.section(t, "nights")
	if num(rt[2]) != 1 || num(rt[4]) != 650000 {
		t.Fatalf("room totals: %v", rt)
	}
	if td, _ := rm.section(t, "today"); num(td[0][1]) != 1 {
		t.Fatalf("in the house: %v", td)
	}
	// Stock answers (nothing stocked yet).
	r.report(r.mgr, "stock", "", 200, "")
}

func TestStage25_AccessRangeAndCSV(t *testing.T) {
	r := newRoomsEnv(t)
	tea, _, _ := menuFixture(t, r.e, r.mgr)
	r.e.addUser("aud", false, "accountant_auditor")
	r.e.addUser("wait", false, "waiter")
	aud, wait := r.e.mustLogin("aud"), r.e.mustLogin("wait")
	r.e.openShift(t, r.mgr, 0)
	r.sell(r.mgr, tea, 1, cash(8000))
	r.e.openShift(t, r.cash, 0)
	r.sell(r.cash, tea, 2, cash(16000))

	// A cashier reads only their own sales and collections; the other reports are not theirs.
	_, ctot := r.report(r.cash, "sales", "", 200, "").section(t, "days")
	if num(ctot[4]) != 16000 {
		t.Fatalf("cashier sees %v, want only their own 160.00", ctot)
	}
	_, ctot = r.report(r.cash, "collections", "", 200, "").section(t, "collected")
	if num(ctot[4]) != 16000 {
		t.Fatalf("cashier collections %v", ctot)
	}
	for _, n := range []string{"balances-due", "discounts-voids-refunds", "stock", "rooms"} {
		r.report(r.cash, n, "", 403, "forbidden")
	}
	r.report(wait, "sales", "", 403, "forbidden")
	// A manager and an auditor see everything.
	_, mtot := r.report(r.mgr, "sales", "", 200, "").section(t, "days")
	_, atot := r.report(aud, "sales", "", 200, "").section(t, "days")
	if num(mtot[4]) != 24000 || num(atot[4]) != 24000 {
		t.Fatalf("manager %v auditor %v", mtot, atot)
	}

	// Ranges: from after to, a bad date, too long a range, an unknown report.
	r.call(r.mgr, "GET", "/api/v1/reports/sales?from=2026-10-20&to=2026-10-01", nil, 400, "validation_failed")
	r.call(r.mgr, "GET", "/api/v1/reports/sales?from=nope", nil, 400, "validation_failed")
	r.call(r.mgr, "GET", "/api/v1/reports/sales?from=2025-01-01&to=2026-10-01", nil, 400, "validation_failed")
	r.call(r.mgr, "GET", "/api/v1/reports/payroll", nil, 404, "")
	r.call(r.mgr, "GET", "/api/v1/reports/sales?format=pdf", nil, 400, "validation_failed")

	// CSV needs reports.export and is audited. The cashier and the (view-only) others get 403.
	before := r.e.auditN("report.exported")
	r.report(r.cash, "sales", "&format=csv", 403, "forbidden")
	if r.e.auditN("report.exported") != before {
		t.Fatal("a refused export was audited")
	}
	w := r.call(r.mgr, "GET", "/api/v1/reports/sales?format=csv&from="+r.day(0)+"&to="+r.day(0), nil, 200, "")
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("headers: %q %q", ct, w.Header().Get("Content-Disposition"))
	}
	cr := csv.NewReader(strings.NewReader(w.Body.String()))
	cr.FieldsPerRecord = -1 // sections have different widths
	if _, err := cr.ReadAll(); err != nil {
		t.Fatalf("csv does not parse: %v", err)
	}
	if !strings.Contains(w.Body.String(), "Sales total (KES)") || !strings.Contains(w.Body.String(), "240.00") {
		t.Fatalf("csv body: %s", w.Body.String())
	}
	r.call(aud, "GET", "/api/v1/reports/stock?format=csv", nil, 200, "")
	if r.e.auditN("report.exported") != before+2 {
		t.Fatalf("exports audited: %d", r.e.auditN("report.exported")-before)
	}
}
