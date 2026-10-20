package handler_test

import (
	"net/http/httptest"
	"sync"
	"testing"
)

// Stage 17: the stock ledger, adjustments, and what a sale does to stock (AC-06; ASSUMPTIONS.md S-71 to S-77).

type invMovement struct {
	ID                 int64  `json:"id"`
	ItemID             int64  `json:"item_id"`
	LocationID         int64  `json:"location_id"`
	QtyDelta           int64  `json:"qty_delta"`
	MovementType       string `json:"movement_type"`
	OverrideReason     string `json:"override_reason"`
	ReversesMovementID *int64 `json:"reverses_movement_id"`
	Reversed           bool   `json:"reversed"`
}

type invEnv struct {
	*kitchenEnv
	store          jar // storekeeper: inventory.adjust, no override
	ids            map[string]int64
	pc, ml, kitchn int64
	bar, mainStore int64
}

func newInvEnv(t *testing.T) *invEnv {
	t.Helper()
	k := newKitchenEnv(t)
	k.e.addUser("store", false, "storekeeper")
	v := &invEnv{kitchenEnv: k, store: k.e.mustLogin("store"), ids: map[string]int64{}}
	units := decodeInto[struct {
		Units []struct {
			ID   int64  `json:"id"`
			Code string `json:"code"`
		} `json:"units"`
	}](t, v.call(k.mgr, "GET", "/api/v1/inventory/units", nil, 200, ""))
	for _, u := range units.Units {
		v.ids["unit_"+u.Code] = u.ID
	}
	locs := decodeInto[struct {
		Locations []struct {
			ID   int64  `json:"id"`
			Code string `json:"code"`
		} `json:"locations"`
	}](t, v.call(k.mgr, "GET", "/api/v1/inventory/locations", nil, 200, ""))
	for _, l := range locs.Locations {
		v.ids["loc_"+l.Code] = l.ID
	}
	v.pc, v.ml = v.ids["unit_pc"], v.ids["unit_ml"]
	v.kitchn, v.bar, v.mainStore = v.ids["loc_kitchen"], v.ids["loc_bar"], v.ids["loc_main_store"]
	return v
}

func (v *invEnv) call(j jar, method, path string, body any, want int, errCode string) *httptest.ResponseRecorder {
	v.t.Helper()
	r := req{method: method, path: path, jar: j, body: body}
	if method != "GET" && (path == "/api/v1/inventory/opening-balances" || path == "/api/v1/inventory/adjustments" || (len(path) > 28 && path[len(path)-8:] == "/reverse")) {
		r.header = map[string]string{"Idempotency-Key": newKey()}
	}
	w := v.e.do(r)
	expect(v.t, w, want, errCode)
	return w
}

func (v *invEnv) item(name string, unit int64, extra ...func(map[string]any)) int64 {
	v.t.Helper()
	b := map[string]any{"name": name, "base_unit_id": unit}
	for _, f := range extra {
		f(b)
	}
	w := v.call(v.mgr, "POST", "/api/v1/inventory/items", b, 201, "")
	return decodeInto[struct {
		Item struct {
			ID int64 `json:"id"`
		} `json:"item"`
	}](v.t, w).Item.ID
}

func (v *invEnv) opening(item, loc, qty int64) {
	v.t.Helper()
	v.call(v.mgr, "POST", "/api/v1/inventory/opening-balances", map[string]any{"location_id": loc, "lines": []map[string]any{{"item_id": item, "qty": qty}}}, 201, "")
}

func (v *invEnv) adjust(j jar, item, loc, qty int64, extra map[string]any, want int, errCode string) invMovement {
	v.t.Helper()
	b := map[string]any{"item_id": item, "location_id": loc, "qty": qty, "reason": "count correction"}
	for k, x := range extra {
		b[k] = x
	}
	w := v.call(j, "POST", "/api/v1/inventory/adjustments", b, want, errCode)
	if want >= 400 {
		return invMovement{}
	}
	return decodeInto[struct {
		Movement invMovement `json:"movement"`
	}](v.t, w).Movement
}

func (v *invEnv) balance(item, loc int64) int64 {
	v.t.Helper()
	var n int64
	if err := v.e.pool.QueryRow(v.t.Context(), `SELECT COALESCE(sum(qty_delta), 0) FROM inventory_movements WHERE item_id = $1 AND location_id = $2`, item, loc).Scan(&n); err != nil {
		v.t.Fatal(err)
	}
	return n
}

// apiBalance reads the balance through the API, which must agree with the ledger.
func (v *invEnv) apiBalance(item, loc int64) int64 {
	v.t.Helper()
	w := v.call(v.mgr, "GET", "/api/v1/inventory/balances?item_id="+itoa(item)+"&location_id="+itoa(loc), nil, 200, "")
	rows := decodeInto[struct {
		Balances []struct {
			QtyBase int64 `json:"qty_base"`
		} `json:"balances"`
	}](v.t, w).Balances
	if len(rows) == 0 {
		return 0
	}
	return rows[0].QtyBase
}

// link makes a product take stock: each unit sold takes per base units of the item.
func (v *invEnv) link(product, item, per int64) {
	v.t.Helper()
	v.call(v.mgr, "PATCH", "/api/v1/products/"+itoa(product), map[string]any{"inventory_item_id": item, "stock_units_per_sale": per}, 200, "")
}

func TestAC06_BalanceEqualsSumOfMovements(t *testing.T) {
	v := newInvEnv(t)
	it := v.item("Spaghetti", v.ids["unit_g"])
	v.opening(it, v.mainStore, 5000)
	v.adjust(v.store, it, v.mainStore, 1500, nil, 201, "")
	v.adjust(v.store, it, v.mainStore, -700, nil, 201, "")
	if got, want := v.apiBalance(it, v.mainStore), int64(5800); got != want || v.balance(it, v.mainStore) != want {
		t.Fatalf("api balance %d, ledger %d, want %d", got, v.balance(it, v.mainStore), want)
	}
	// Nothing is stored as a balance: there is no column to drift from the ledger.
	if n := v.e.count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'inventory_items' AND column_name ~ 'balance|on_hand|qty'`); n != 0 {
		t.Fatalf("inventory_items has %d balance-like columns", n)
	}
	// Another place is separate.
	if v.apiBalance(it, v.bar) != 0 {
		t.Fatal("the bar should have none")
	}
}

func TestAC06_LedgerRejectsUpdateDeleteTruncate(t *testing.T) {
	v := newInvEnv(t)
	it := v.item("Rice", v.ids["unit_g"])
	v.opening(it, v.mainStore, 100)
	for _, q := range []string{`UPDATE inventory_movements SET qty_delta = 5`, `DELETE FROM inventory_movements`, `TRUNCATE inventory_movements`} {
		if _, err := v.e.pool.Exec(t.Context(), q); err == nil {
			t.Fatalf("%q was allowed", q)
		}
	}
	// An item cannot change its base unit.
	if _, err := v.e.pool.Exec(t.Context(), `UPDATE inventory_items SET base_unit_id = $1 WHERE id = $2`, v.pc, it); err == nil {
		t.Fatal("base unit change allowed")
	}
	if v.balance(it, v.mainStore) != 100 {
		t.Fatal("ledger changed")
	}
}

func TestAC06_ConversionsBottleCrateShot(t *testing.T) {
	v := newInvEnv(t)
	mk := func(code, name string) int64 {
		w := v.call(v.mgr, "POST", "/api/v1/inventory/units", map[string]any{"code": code, "name": name}, 201, "")
		return decodeInto[struct {
			Unit struct {
				ID int64 `json:"id"`
			} `json:"unit"`
		}](t, w).Unit.ID
	}
	bottle, shot, crate, half := mk("bottle", "Bottle"), mk("shot", "Shot"), mk("crate", "Crate"), mk("half", "Half")
	whisky := v.item("Whisky", v.ml)
	v.call(v.mgr, "PUT", "/api/v1/inventory/items/"+itoa(whisky)+"/conversions", map[string]any{"conversions": []map[string]any{
		{"unit_id": bottle, "factor_num": 750}, {"unit_id": shot, "factor_num": 25}, {"unit_id": half, "factor_num": 1, "factor_den": 2}}}, 200, "")
	beer := v.item("Tusker", v.pc)
	v.call(v.mgr, "PUT", "/api/v1/inventory/items/"+itoa(beer)+"/conversions", map[string]any{"conversions": []map[string]any{{"unit_id": crate, "factor_num": 24}}}, 200, "")

	// 2 bottles in, 3 shots out; 1 crate in.
	v.call(v.mgr, "POST", "/api/v1/inventory/opening-balances", map[string]any{"location_id": v.bar, "lines": []map[string]any{
		{"item_id": whisky, "qty": 2, "unit_id": bottle}, {"item_id": beer, "qty": 1, "unit_id": crate}}}, 201, "")
	v.adjust(v.mgr, whisky, v.bar, -3, map[string]any{"unit_id": shot}, 201, "")
	if got := v.balance(whisky, v.bar); got != 1500-75 {
		t.Fatalf("whisky = %d ml, want 1425", got)
	}
	if got := v.balance(beer, v.bar); got != 24 {
		t.Fatalf("beer = %d, want 24", got)
	}
	// A quantity that does not come to a whole number of base units is refused, not rounded (S-03).
	v.adjust(v.mgr, whisky, v.bar, 1, map[string]any{"unit_id": half}, 400, "validation_failed")
	// A unit with no conversion for the item is refused.
	v.adjust(v.mgr, beer, v.bar, 1, map[string]any{"unit_id": bottle}, 400, "validation_failed")
	if got := v.balance(whisky, v.bar); got != 1425 {
		t.Fatalf("refused adjustments changed stock: %d", got)
	}
}

func TestAC06_ReversalIsCompensating(t *testing.T) {
	v := newInvEnv(t)
	it := v.item("Flour", v.ids["unit_g"])
	v.opening(it, v.mainStore, 1000)
	m := v.adjust(v.store, it, v.mainStore, 400, nil, 201, "")
	rv := func(id int64, want int, code string) *httptest.ResponseRecorder {
		return v.call(v.store, "POST", "/api/v1/inventory/movements/"+itoa(id)+"/reverse", map[string]any{"reason": "typed twice"}, want, code)
	}
	rv(m.ID, 201, "")
	if v.balance(it, v.mainStore) != 1000 {
		t.Fatalf("balance %d after reversal, want 1000", v.balance(it, v.mainStore))
	}
	// The original is untouched and a new row points at it; nothing was edited or deleted.
	if n := v.e.count(`SELECT count(*) FROM inventory_movements WHERE item_id = $1`, it); n != 3 {
		t.Fatalf("%d movements, want 3 (opening, adjustment, reversal)", n)
	}
	if n := v.e.count(`SELECT count(*) FROM inventory_movements WHERE reverses_movement_id = $1 AND qty_delta = -400`, m.ID); n != 1 {
		t.Fatal("reversal is not the exact opposite")
	}
	rv(m.ID, 409, "already_reversed")
	// A reversal cannot itself be reversed here.
	var rid int64
	if err := v.e.pool.QueryRow(t.Context(), `SELECT id FROM inventory_movements WHERE reverses_movement_id = $1`, m.ID).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	rv(rid, 409, "not_reversible")
	// The database refuses a crooked reversal even if the service did not.
	if _, err := v.e.pool.Exec(t.Context(), `INSERT INTO inventory_movements (item_id, location_id, qty_delta, movement_type, reason, reverses_movement_id, created_by)
		SELECT item_id, location_id, 1, 'reversal', 'x', id, created_by FROM inventory_movements WHERE movement_type = 'opening_balance' AND item_id = $1`, it); err == nil {
		t.Fatal("a reversal that is not the exact opposite was accepted")
	}
	// A sale's own movement is never reversed this way.
	v.link(v.tea, it, 10)
	v.sell(v.cash, nil, nil, [2]int64{v.tea, 1})
	var sm int64
	if err := v.e.pool.QueryRow(t.Context(), `SELECT id FROM inventory_movements WHERE movement_type = 'sale_consumption' AND item_id = $1`, it).Scan(&sm); err != nil {
		t.Fatal(err)
	}
	rv(sm, 409, "not_reversible")
}

func TestAC06_NegativeStockNeedsOverride(t *testing.T) {
	v := newInvEnv(t)
	it := v.item("Sugar", v.ids["unit_g"])
	v.opening(it, v.mainStore, 100)
	// Without an override: refused, nothing written.
	v.adjust(v.store, it, v.mainStore, -150, nil, 409, "negative_stock")
	// Asking to override without the permission: refused.
	v.adjust(v.store, it, v.mainStore, -150, map[string]any{"override_negative": true, "override_reason": "spillage"}, 403, "override_not_permitted")
	// With the permission but no reason: refused.
	v.adjust(v.mgr, it, v.mainStore, -150, map[string]any{"override_negative": true}, 400, "validation_failed")
	if v.balance(it, v.mainStore) != 100 {
		t.Fatal("a refused adjustment changed stock")
	}
	m := v.adjust(v.mgr, it, v.mainStore, -150, map[string]any{"override_negative": true, "override_reason": "bag split, counted later"}, 201, "")
	if m.OverrideReason == "" || v.balance(it, v.mainStore) != -50 {
		t.Fatalf("override not recorded (%+v) or balance %d", m, v.balance(it, v.mainStore))
	}
	// Under the block policy nobody may go below zero, override or not.
	v.e.exec(`UPDATE system_settings SET value = '"block"' WHERE key = 'negative_stock_policy'`)
	v.adjust(v.mgr, it, v.mainStore, -10, map[string]any{"override_negative": true, "override_reason": "x"}, 409, "insufficient_stock")
}

func TestAC06_OpeningBalanceOncePerPlace(t *testing.T) {
	v := newInvEnv(t)
	it := v.item("Salt", v.ids["unit_g"])
	v.opening(it, v.mainStore, 100)
	v.call(v.mgr, "POST", "/api/v1/inventory/opening-balances", map[string]any{"location_id": v.mainStore, "lines": []map[string]any{{"item_id": it, "qty": 5}}}, 409, "opening_exists")
	v.opening(it, v.bar, 50) // another place is fine
	if v.balance(it, v.mainStore) != 100 {
		t.Fatal("the refused opening changed stock")
	}
}

func TestAC06_SaleDeductionNotRepeatedOnRetry(t *testing.T) {
	v := newInvEnv(t)
	it := v.item("Tea leaves", v.ids["unit_g"])
	v.opening(it, v.kitchn, 100)
	v.link(v.tea, it, 3)

	o := v.open()
	v.add(o, v.tea, 2, "")
	key := newKey()
	fin := func(k string) *httptest.ResponseRecorder {
		return v.e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(o) + "/finalise", jar: v.cash, header: map[string]string{"Idempotency-Key": k}, body: map[string]any{}})
	}
	first := fin(key)
	expect(t, first, 201, "")
	if got := v.balance(it, v.kitchn); got != 94 {
		t.Fatalf("after one sale of 2 teas at 3 g: %d, want 94", got)
	}
	// The same request again is replayed; a fresh key finds the order already finalised. Neither deducts again.
	again := fin(key)
	expect(t, again, 201, "")
	if again.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("not a replay")
	}
	expect(t, fin(newKey()), 409, "")
	if got := v.balance(it, v.kitchn); got != 94 {
		t.Fatalf("retries changed stock: %d", got)
	}
	if n := v.e.count(`SELECT count(*) FROM inventory_movements WHERE movement_type = 'sale_consumption' AND item_id = $1`, it); n != 1 {
		t.Fatalf("%d sale movements, want 1", n)
	}
	// The database also refuses a second deduction for the same sale.
	if _, err := v.e.pool.Exec(t.Context(), `INSERT INTO inventory_movements (item_id, location_id, qty_delta, movement_type, ref_type, ref_id, created_by)
		SELECT item_id, location_id, -1, 'sale_consumption', ref_type, ref_id, created_by FROM inventory_movements WHERE movement_type = 'sale_consumption' AND item_id = $1`, it); err == nil {
		t.Fatal("a second deduction for the same sale was accepted")
	}
}

func TestAC06_BarProductsComeFromTheBar(t *testing.T) {
	v := newInvEnv(t)
	soda := v.item("Soda bottle", v.pc)
	v.opening(soda, v.bar, 10)
	v.opening(soda, v.kitchn, 10)
	v.link(v.kitchenEnv.bar, soda, 1)
	v.sell(v.cash, nil, nil, [2]int64{v.kitchenEnv.bar, 3})
	if v.balance(soda, v.bar) != 7 || v.balance(soda, v.kitchn) != 10 {
		t.Fatalf("bar %d (want 7), kitchen %d (want 10)", v.balance(soda, v.bar), v.balance(soda, v.kitchn))
	}
}

func TestAC06_SaleNeverBlockedByEmptyShelfUnlessPolicyBlocks(t *testing.T) {
	v := newInvEnv(t)
	it := v.item("Milk", v.ml)
	v.opening(it, v.kitchn, 100)
	v.link(v.tea, it, 60)
	s := v.sell(v.cash, nil, nil, [2]int64{v.tea, 2}) // needs 120, only 100 there
	if v.balance(it, v.kitchn) != -20 {
		t.Fatalf("balance %d, want -20", v.balance(it, v.kitchn))
	}
	if n := v.e.count(`SELECT count(*) FROM inventory_movements WHERE movement_type = 'sale_consumption' AND override_reason IS NOT NULL AND ref_id = $1`, s.ID); n != 1 {
		t.Fatal("the below-zero sale carries no automatic reason")
	}
	// Under "block" the sale is refused whole.
	v.e.exec(`UPDATE system_settings SET value = '"block"' WHERE key = 'negative_stock_policy'`)
	salesBefore := v.e.count(`SELECT count(*) FROM sales`)
	o := v.open()
	v.add(o, v.tea, 1, "")
	w := v.e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(o) + "/finalise", jar: v.cash, header: map[string]string{"Idempotency-Key": newKey()}, body: map[string]any{}})
	expect(t, w, 409, "insufficient_stock")
	if v.e.count(`SELECT count(*) FROM sales`) != salesBefore || v.balance(it, v.kitchn) != -20 {
		t.Fatal("a refused sale left something behind")
	}
}

func TestAC06_VoidRestocksRefundOnlyWhenAsked(t *testing.T) {
	v := newInvEnv(t)
	v.e.openShift(t, v.cash, 0) // cash needs an open shift, for the till and for the manager handing cash back
	v.e.openShift(t, v.mgr, 0)
	it := v.item("Bread", v.pc)
	v.opening(it, v.kitchn, 50)
	v.link(v.tea, it, 2)

	a := v.sell(v.cash, nil, cash(16000), [2]int64{v.tea, 2}) // takes 4
	if v.balance(it, v.kitchn) != 46 {
		t.Fatalf("balance %d, want 46", v.balance(it, v.kitchn))
	}
	// A refund puts nothing back unless the person says so.
	v.refund(v.mgr, a.ID, map[string]any{"reason": "customer returned one", "lines": []map[string]any{{"sale_item_id": v.lineRef(a, 0), "qty": 1}}, "payouts": cash(8000)}, 201, "")
	if v.balance(it, v.kitchn) != 46 {
		t.Fatalf("refund without restock changed stock: %d", v.balance(it, v.kitchn))
	}
	v.refund(v.mgr, a.ID, map[string]any{"reason": "second one too, still sealed", "restock": true, "lines": []map[string]any{{"sale_item_id": v.lineRef(a, 0), "qty": 1}}, "payouts": cash(8000)}, 201, "")
	if v.balance(it, v.kitchn) != 48 { // one tea = 2 units back
		t.Fatalf("refund with restock: %d, want 48", v.balance(it, v.kitchn))
	}
	// A void puts back exactly what the sale took.
	b := v.sell(v.cash, nil, cash(8000), [2]int64{v.tea, 1}) // takes 2
	if v.balance(it, v.kitchn) != 46 {
		t.Fatalf("balance %d, want 46", v.balance(it, v.kitchn))
	}
	v.void(v.mgr, b.ID, "rung up by mistake", 201, "")
	if v.balance(it, v.kitchn) != 48 {
		t.Fatalf("void: %d, want 48", v.balance(it, v.kitchn))
	}
	if n := v.e.count(`SELECT count(*) FROM inventory_movements WHERE movement_type = 'sale_return'`); n != 2 {
		t.Fatalf("%d sale_return movements, want 2", n)
	}
	// Changing the product's link afterwards does not change what an old sale put back: the line kept its own numbers.
	v.link(v.tea, it, 9)
	if n := v.e.count(`SELECT count(*) FROM sale_items WHERE stock_units_per_sale = 2`); n != 2 {
		t.Fatalf("%d lines kept their snapshot, want 2", n)
	}
}

func TestAC06_OutageEntryTakesStock(t *testing.T) {
	v := newInvEnv(t)
	it := v.item("Chai masala", v.ids["unit_g"])
	v.opening(it, v.kitchn, 100)
	v.link(v.tea, it, 5)
	v.mustEnter(v.paper("STK-1")) // two teas
	if got := v.balance(it, v.kitchn); got != 90 {
		t.Fatalf("balance %d, want 90", got)
	}
}

func TestAC06_ConcurrentAdjustmentsNeverGoBelowZero(t *testing.T) {
	v := newInvEnv(t)
	it := v.item("Charcoal", v.pc)
	v.opening(it, v.mainStore, 5)
	codes := make([]int, 12)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := v.e.do(req{method: "POST", path: "/api/v1/inventory/adjustments", jar: v.store, header: map[string]string{"Idempotency-Key": newKey()},
				body: map[string]any{"item_id": it, "location_id": v.mainStore, "qty": -1, "reason": "burnt"}})
			codes[i] = w.Code
		}()
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == 201 {
			ok++
		}
	}
	if ok != 5 || v.balance(it, v.mainStore) != 0 {
		t.Fatalf("%d adjustments went through (want 5), balance %d: %v", ok, v.balance(it, v.mainStore), codes)
	}
}

func TestAC06_PermissionsAndIdempotency(t *testing.T) {
	v := newInvEnv(t)
	it := v.item("Oil", v.ml)
	// A cashier sees nothing and changes nothing.
	v.call(v.cash, "GET", "/api/v1/inventory/items", nil, 403, "forbidden")
	v.call(v.cash, "POST", "/api/v1/inventory/adjustments", map[string]any{"item_id": it, "location_id": v.bar, "qty": 1, "reason": "x"}, 403, "forbidden")
	// The same adjustment sent twice with one key is applied once.
	key := newKey()
	body := map[string]any{"item_id": it, "location_id": v.bar, "qty": 7, "reason": "found a case"}
	for i := 0; i < 2; i++ {
		w := v.e.do(req{method: "POST", path: "/api/v1/inventory/adjustments", jar: v.store, header: map[string]string{"Idempotency-Key": key}, body: body})
		expect(t, w, 201, "")
		if (i == 1) != (w.Header().Get("Idempotent-Replayed") == "true") {
			t.Fatalf("attempt %d: replay header %q", i, w.Header().Get("Idempotent-Replayed"))
		}
	}
	if v.balance(it, v.bar) != 7 {
		t.Fatalf("balance %d, want 7", v.balance(it, v.bar))
	}
	// Changing the body under the same key is refused.
	body["qty"] = 8
	w := v.e.do(req{method: "POST", path: "/api/v1/inventory/adjustments", jar: v.store, header: map[string]string{"Idempotency-Key": key}, body: body})
	expect(t, w, 422, "idempotency_key_reused")
	// Duplicate item names (any case) are refused.
	v.call(v.mgr, "POST", "/api/v1/inventory/items", map[string]any{"name": "OIL", "base_unit_id": v.ml}, 409, "")
	// A product cannot be linked to an item that does not exist.
	v.call(v.mgr, "PATCH", "/api/v1/products/"+itoa(v.tea), map[string]any{"inventory_item_id": 99999}, 400, "validation_failed")
}
