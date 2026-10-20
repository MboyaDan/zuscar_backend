package handler_test

import (
	"net/http/httptest"
	"sync"
	"testing"
)

// Stage 21: stock counts and the low-stock list (AC-06; ASSUMPTIONS.md S-89 to S-92).

type countOut struct {
	Count struct {
		ID              int64  `json:"id"`
		CountNo         int64  `json:"count_no"`
		Status          string `json:"status"`
		LineCount       int64  `json:"line_count"`
		DifferenceCount int64  `json:"difference_count"`
		Lines           []struct {
			ItemID       int64  `json:"item_id"`
			CountedBase  int64  `json:"counted_base"`
			ExpectedBase int64  `json:"expected_base"`
			VarianceBase int64  `json:"variance_base"`
			BalanceNow   *int64 `json:"balance_now"`
		} `json:"lines"`
	} `json:"count"`
}

func (v *invEnv) startCount(j jar, loc int64, want int, errCode string) countOut {
	v.t.Helper()
	w := v.e.do(req{method: "POST", path: "/api/v1/inventory/counts", jar: j, header: map[string]string{"Idempotency-Key": newKey()}, body: map[string]any{"location_id": loc, "note": "weekly"}})
	expect(v.t, w, want, errCode)
	if want >= 400 {
		return countOut{}
	}
	return decodeInto[countOut](v.t, w)
}

func (v *invEnv) countLines(j jar, id int64, lines []map[string]any, want int, errCode string) countOut {
	v.t.Helper()
	w := v.call(j, "PUT", "/api/v1/inventory/counts/"+itoa(id)+"/lines", map[string]any{"lines": lines}, want, errCode)
	if want >= 400 {
		return countOut{}
	}
	return decodeInto[countOut](v.t, w)
}

func (v *invEnv) approveCountKey(j jar, id int64, key string, want int, errCode string) *httptest.ResponseRecorder {
	v.t.Helper()
	w := v.e.do(req{method: "POST", path: "/api/v1/inventory/counts/" + itoa(id) + "/approve", jar: j, header: map[string]string{"Idempotency-Key": key}})
	expect(v.t, w, want, errCode)
	return w
}

func TestAC06_CountPostsDifferencesAndKeepsMovementsDuringTheCount(t *testing.T) {
	v := newInvEnv(t)
	beer := v.item("Lager 500ml", v.pc)
	crate := decodeInto[struct {
		Unit struct {
			ID int64 `json:"id"`
		} `json:"unit"`
	}](t, v.call(v.mgr, "POST", "/api/v1/inventory/units", map[string]any{"code": "crate", "name": "Crate"}, 201, "")).Unit.ID
	v.call(v.mgr, "PUT", "/api/v1/inventory/items/"+itoa(beer)+"/conversions", map[string]any{"conversions": []map[string]any{{"unit_id": crate, "factor_num": 24}}}, 200, "")
	soda := v.item("Soda 300ml", v.pc)
	gone := v.item("Tonic", v.pc)
	v.opening(beer, v.bar, 24)
	v.opening(soda, v.bar, 30)
	v.opening(gone, v.bar, 5)

	c := v.startCount(v.store, v.bar, 201, "")
	if c.Count.Status != "draft" {
		t.Fatal("a new count is not a draft")
	}
	// 20 beers on the shelf (books say 24), 2 crates of soda is not a unit soda has, 0 tonic left (books say 5).
	v.countLines(v.store, c.Count.ID, []map[string]any{{"item_id": soda, "qty": 2, "unit_id": crate}}, 400, "validation_failed")
	got := v.countLines(v.store, c.Count.ID, []map[string]any{{"item_id": beer, "qty": 20}, {"item_id": soda, "qty": 30}, {"item_id": gone, "qty": 0}}, 200, "")
	if got.Count.LineCount != 3 || got.Count.DifferenceCount != 2 {
		t.Fatalf("lines %d (want 3), differences %d (want 2)", got.Count.LineCount, got.Count.DifferenceCount)
	}
	if v.balance(beer, v.bar) != 24 {
		t.Fatal("entering a count changed stock before it was approved")
	}

	// A delivery of 10 beers arrives while people are still counting.
	v.adjust(v.store, beer, v.bar, 10, nil, 201, "")
	// Saving the same figures again keeps the books figure each line was entered against.
	again := v.countLines(v.store, c.Count.ID, []map[string]any{{"item_id": beer, "qty": 20}, {"item_id": soda, "qty": 30}, {"item_id": gone, "qty": 0}}, 200, "")
	for _, l := range again.Count.Lines {
		if l.ItemID == beer && (l.ExpectedBase != 24 || l.VarianceBase != -4 || l.BalanceNow == nil || *l.BalanceNow != 34) {
			t.Fatalf("beer line: expected %d variance %d balance now %v; want 24, -4, 34", l.ExpectedBase, l.VarianceBase, l.BalanceNow)
		}
	}

	key := newKey()
	w := v.approveCountKey(v.store, c.Count.ID, key, 200, "")
	if v.balance(beer, v.bar) != 30 || v.balance(soda, v.bar) != 30 || v.balance(gone, v.bar) != 0 {
		t.Fatalf("beer %d (want 30: 24 + 10 delivered - 4 short), soda %d (want 30), tonic %d (want 0)", v.balance(beer, v.bar), v.balance(soda, v.bar), v.balance(gone, v.bar))
	}
	if n := v.e.count(`SELECT count(*) FROM inventory_movements WHERE movement_type = 'count_adjustment' AND ref_type = 'stock_count' AND ref_id = $1 AND reason = $2`, c.Count.ID, "Stock count #"+itoa(c.Count.CountNo)); n != 2 {
		t.Fatalf("%d count adjustments, want 2 (the soda line matched the books and posts nothing)", n)
	}
	_ = w
	// A retry replays; another key, a new save, a cancel and a second approval are all refused.
	if r := v.approveCountKey(v.store, c.Count.ID, key, 200, ""); r.Header().Get("Idempotent-Replayed") != "true" || v.balance(beer, v.bar) != 30 {
		t.Fatal("a retry posted the differences again")
	}
	v.approveCountKey(v.store, c.Count.ID, newKey(), 409, "count_not_open")
	v.countLines(v.store, c.Count.ID, []map[string]any{{"item_id": beer, "qty": 1}}, 409, "count_not_open")
	v.call(v.store, "POST", "/api/v1/inventory/counts/"+itoa(c.Count.ID)+"/cancel", nil, 409, "count_not_open")
	// The database keeps an approved count fixed.
	if _, err := v.e.pool.Exec(t.Context(), `UPDATE stock_count_lines SET counted_base = 1 WHERE count_id = $1`, c.Count.ID); err == nil {
		t.Fatal("a line of an approved count was edited")
	}
	if _, err := v.e.pool.Exec(t.Context(), `DELETE FROM stock_count_lines WHERE count_id = $1`, c.Count.ID); err == nil {
		t.Fatal("a line of an approved count was deleted")
	}
	if _, err := v.e.pool.Exec(t.Context(), `DELETE FROM stock_counts WHERE id = $1`, c.Count.ID); err == nil {
		t.Fatal("a count was deleted")
	}
	// A count adjustment must name its count.
	if _, err := v.e.pool.Exec(t.Context(), `INSERT INTO inventory_movements (item_id, location_id, qty_delta, movement_type, reason, created_by) SELECT $1, $2, 1, 'count_adjustment', 'x', id FROM users LIMIT 1`, beer, v.bar); err == nil {
		t.Fatal("a count adjustment without a count was accepted")
	}

	// A count in crates, and a changed figure is entered against the books as they are then.
	c2 := v.startCount(v.store, v.bar, 201, "")
	if c2.Count.CountNo != c.Count.CountNo+1 {
		t.Fatalf("count numbers %d then %d", c.Count.CountNo, c2.Count.CountNo)
	}
	r := v.countLines(v.store, c2.Count.ID, []map[string]any{{"item_id": beer, "qty": 1, "unit_id": crate}}, 200, "")
	if r.Count.Lines[0].CountedBase != 24 || r.Count.Lines[0].ExpectedBase != 30 || r.Count.Lines[0].VarianceBase != -6 {
		t.Fatalf("crate line: %+v", r.Count.Lines[0])
	}
	// Taking the line off the sheet removes it; cancelling posts nothing.
	r = v.countLines(v.store, c2.Count.ID, []map[string]any{}, 200, "")
	if r.Count.LineCount != 0 {
		t.Fatal("a line left off the sheet was kept")
	}
	v.approveCountKey(v.store, c2.Count.ID, newKey(), 400, "validation_failed") // nothing counted
	v.call(v.store, "POST", "/api/v1/inventory/counts/"+itoa(c2.Count.ID)+"/cancel", nil, 200, "")
	v.countLines(v.store, c2.Count.ID, []map[string]any{{"item_id": beer, "qty": 1}}, 409, "count_not_open")
	if v.balance(beer, v.bar) != 30 {
		t.Fatal("a cancelled count changed stock")
	}
	list := decodeInto[struct {
		Counts []struct {
			Status string `json:"status"`
		} `json:"counts"`
	}](t, v.call(v.store, "GET", "/api/v1/inventory/counts?status=approved", nil, 200, ""))
	if len(list.Counts) != 1 {
		t.Fatalf("%d approved counts listed, want 1", len(list.Counts))
	}
}

func TestAC06_CountRules(t *testing.T) {
	v := newInvEnv(t)
	v.e.addUser("barman", false, "bartender")
	bj := v.e.mustLogin("barman")
	it := v.item("Glasses", v.pc)
	off := v.item("Old glasses", v.pc)
	v.call(v.mgr, "PATCH", "/api/v1/inventory/items/"+itoa(off), map[string]any{"active": false}, 200, "")

	v.startCount(v.cash, v.bar, 403, "forbidden")
	v.startCount(bj, v.bar, 403, "forbidden") // the bartender may move and write off stock, not count it
	v.startCount(v.store, 99999, 400, "validation_failed")
	if v.e.count(`SELECT next_value FROM document_counters WHERE doc_type = 'count' AND scope = ''`) != 1 {
		t.Fatal("a refused count used up a number")
	}
	c := v.startCount(v.store, v.bar, 201, "")
	v.countLines(v.cash, c.Count.ID, []map[string]any{}, 403, "forbidden")
	v.countLines(v.store, c.Count.ID, []map[string]any{{"item_id": 99999, "qty": 1}}, 400, "validation_failed")
	v.countLines(v.store, c.Count.ID, []map[string]any{{"item_id": it, "qty": -1}}, 400, "validation_failed")
	v.countLines(v.store, c.Count.ID, []map[string]any{{"item_id": it, "qty": 1}, {"item_id": it, "qty": 2}}, 400, "validation_failed")
	v.countLines(v.store, c.Count.ID, []map[string]any{{"item_id": off, "qty": 1}}, 409, "item_inactive")
	v.countLines(v.store, 99999, []map[string]any{}, 404, "not_found")
	v.call(v.store, "GET", "/api/v1/inventory/counts/"+itoa(c.Count.ID), nil, 200, "")
	v.call(v.cash, "GET", "/api/v1/inventory/counts", nil, 403, "forbidden")
	v.call(v.store, "GET", "/api/v1/inventory/counts?status=bogus", nil, 400, "validation_failed")
	v.approveCountKey(v.cash, c.Count.ID, newKey(), 403, "forbidden")
	v.approveCountKey(v.store, 99999, newKey(), 404, "not_found")
	if n := v.e.count(`SELECT count(*) FROM stock_count_lines`); n != 0 {
		t.Fatalf("%d lines left behind by refused requests", n)
	}

	// Approving is the correction itself: a place that ends below zero is allowed and shows as below zero.
	v.opening(it, v.bar, 5)
	v.countLines(v.store, c.Count.ID, []map[string]any{{"item_id": it, "qty": 0}}, 200, "")
	v.adjust(v.mgr, it, v.bar, -5, map[string]any{"override_negative": true, "override_reason": "sold before counting"}, 201, "")
	v.approveCountKey(v.store, c.Count.ID, newKey(), 200, "")
	if v.balance(it, v.bar) != -5 {
		t.Fatalf("balance %d, want -5 (the count said 0 when the books said 5, then 5 more went out)", v.balance(it, v.bar))
	}
}

func TestAC06_ConcurrentApprovalsPostOnce(t *testing.T) {
	v := newInvEnv(t)
	it := v.item("Napkins", v.pc)
	v.opening(it, v.bar, 10)
	c := v.startCount(v.store, v.bar, 201, "")
	v.countLines(v.store, c.Count.ID, []map[string]any{{"item_id": it, "qty": 7}}, 200, "")
	codes := make([]int, 6)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := v.e.do(req{method: "POST", path: "/api/v1/inventory/counts/" + itoa(c.Count.ID) + "/approve", jar: v.store, header: map[string]string{"Idempotency-Key": newKey()}})
			codes[i] = w.Code
		}()
	}
	wg.Wait()
	won := 0
	for _, code := range codes {
		if code == 200 {
			won++
		} else if code != 409 {
			t.Fatalf("unexpected status %d", code)
		}
	}
	if won != 1 || v.balance(it, v.bar) != 7 {
		t.Fatalf("%d approvals succeeded, balance %d; want exactly 1 and 7", won, v.balance(it, v.bar))
	}
}

func TestAC06_LowStockList(t *testing.T) {
	v := newInvEnv(t)
	low := func(n int64) func(map[string]any) { return func(b map[string]any) { b["low_stock_threshold"] = n } }
	a := v.item("Gin 750ml", v.pc, low(6))
	b := v.item("Tonic", v.pc, low(24))
	c := v.item("Salt", v.pc) // no level set
	d := v.item("Old rum", v.pc, low(10))
	e := v.item("Fine whisky", v.pc, low(2))
	v.opening(a, v.bar, 4)  // at 4, level 6: low
	v.opening(b, v.bar, 24) // exactly at the level: low
	v.opening(c, v.bar, 0+1)
	v.opening(d, v.bar, 1)
	v.opening(e, v.bar, 50) // well above its level
	v.call(v.mgr, "PATCH", "/api/v1/inventory/items/"+itoa(d), map[string]any{"active": false}, 200, "")

	got := decodeInto[struct {
		Items []struct {
			ID        int64 `json:"id"`
			TotalBase int64 `json:"total_base"`
		} `json:"items"`
	}](t, v.call(v.store, "GET", "/api/v1/inventory/low-stock", nil, 200, "")).Items
	if len(got) != 2 || got[0].ID != a || got[1].ID != b {
		t.Fatalf("low-stock list %+v, want gin (4) then tonic (24)", got)
	}
	v.call(v.cash, "GET", "/api/v1/inventory/low-stock", nil, 403, "forbidden")
	v.call(v.e.mustLogin("store"), "GET", "/api/v1/inventory/low-stock", nil, 200, "")
}
