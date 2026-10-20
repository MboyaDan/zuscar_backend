package handler_test

import (
	"net/http/httptest"
	"sync"
	"testing"
)

// Stage 19: suppliers, purchases and receiving (AC-06; ASSUMPTIONS.md S-81 to S-84).

type purchaseOut struct {
	ID         int64  `json:"id"`
	PurchaseNo int64  `json:"purchase_no"`
	Status     string `json:"status"`
	TotalCents int64  `json:"total_cents"`
	Lines      []struct {
		QtyBase       int64 `json:"qty_base"`
		UnitCostCents int64 `json:"unit_cost_cents"`
		LineCents     int64 `json:"line_cents"`
	} `json:"lines"`
}

func (v *invEnv) supplier(j jar, name string, want int, errCode string) int64 {
	v.t.Helper()
	w := v.call(j, "POST", "/api/v1/suppliers", map[string]any{"name": name, "contact": "0700 000 000"}, want, errCode)
	if want >= 400 {
		return 0
	}
	return decodeInto[struct {
		Supplier struct {
			ID int64 `json:"id"`
		} `json:"supplier"`
	}](v.t, w).Supplier.ID
}

func (v *invEnv) buy(j jar, body map[string]any, key string, want int, errCode string) (purchaseOut, *httptest.ResponseRecorder) {
	v.t.Helper()
	w := v.e.do(req{method: "POST", path: "/api/v1/purchases", jar: j, header: map[string]string{"Idempotency-Key": key}, body: body})
	expect(v.t, w, want, errCode)
	if want >= 400 {
		return purchaseOut{}, w
	}
	return decodeInto[struct {
		Purchase purchaseOut `json:"purchase"`
	}](v.t, w).Purchase, w
}

func (v *invEnv) receive(j jar, id, loc int64, key string, want int, errCode string) *httptest.ResponseRecorder {
	v.t.Helper()
	w := v.e.do(req{method: "POST", path: "/api/v1/purchases/" + itoa(id) + "/receive", jar: j, header: map[string]string{"Idempotency-Key": key}, body: map[string]any{"location_id": loc, "invoice_ref": "INV-77"}})
	expect(v.t, w, want, errCode)
	return w
}

func TestAC06_PurchaseReceiveBooksReceiptsWithCost(t *testing.T) {
	v := newInvEnv(t)
	crate := decodeInto[struct {
		Unit struct {
			ID int64 `json:"id"`
		} `json:"unit"`
	}](t, v.call(v.mgr, "POST", "/api/v1/inventory/units", map[string]any{"code": "crate", "name": "Crate"}, 201, "")).Unit.ID
	beer := v.item("Lager 500ml", v.pc)
	v.call(v.mgr, "PUT", "/api/v1/inventory/items/"+itoa(beer)+"/conversions", map[string]any{"conversions": []map[string]any{{"unit_id": crate, "factor_num": 24}}}, 200, "")
	glass := v.item("Pint glass", v.pc)
	sup := v.supplier(v.store, "Kenya Breweries depot", 201, "")
	v.supplier(v.store, "kenya breweries DEPOT", 409, "duplicate_name")

	lines := []map[string]any{
		{"item_id": beer, "qty": 2, "unit_id": crate, "unit_cost_cents": 360000}, // 2 crates at KES 3,600 a crate
		{"item_id": glass, "qty": 10, "unit_cost_cents": 15000},
	}
	p1, _ := v.buy(v.store, map[string]any{"supplier_id": sup, "lines": lines}, newKey(), 201, "")
	if p1.Status != "draft" || p1.TotalCents != 2*360000+10*15000 || p1.Lines[0].QtyBase != 48 {
		t.Fatalf("draft wrong: %+v", p1)
	}
	if v.balance(beer, v.mainStore) != 0 {
		t.Fatal("a draft purchase changed stock")
	}

	key := newKey()
	v.receive(v.store, p1.ID, v.mainStore, key, 200, "")
	if v.balance(beer, v.mainStore) != 48 || v.balance(glass, v.mainStore) != 10 {
		t.Fatalf("beer %d (want 48), glasses %d (want 10)", v.balance(beer, v.mainStore), v.balance(glass, v.mainStore))
	}
	if n := v.e.count(`SELECT count(*) FROM inventory_movements WHERE movement_type = 'receipt' AND ref_type = 'purchase' AND ref_id = $1 AND unit_cost_cents IS NOT NULL AND created_by IS NOT NULL`, p1.ID); n != 2 {
		t.Fatalf("%d receipt movements with cost, want 2", n)
	}
	if n := v.e.count(`SELECT unit_cost_cents FROM inventory_movements WHERE item_id = $1 AND movement_type = 'receipt'`, beer); n != 360000 {
		t.Fatalf("receipt cost %d, want 360000 (per crate, as invoiced)", n)
	}
	// The same key again is the same answer and books nothing more.
	w := v.receive(v.store, p1.ID, v.mainStore, key, 200, "")
	if w.Header().Get("Idempotent-Replayed") != "true" || v.balance(beer, v.mainStore) != 48 {
		t.Fatal("a retry with the same key received twice or was not marked as a replay")
	}
	// A different key on a purchase already received is refused.
	v.receive(v.store, p1.ID, v.mainStore, newKey(), 409, "purchase_not_open")
	v.call(v.store, "POST", "/api/v1/purchases/"+itoa(p1.ID)+"/cancel", nil, 409, "purchase_not_open")

	// Numbers run on without a gap, and the database keeps a received purchase fixed.
	p2, _ := v.buy(v.store, map[string]any{"supplier_id": sup, "lines": lines[1:]}, newKey(), 201, "")
	if p2.PurchaseNo != p1.PurchaseNo+1 {
		t.Fatalf("purchase numbers %d then %d", p1.PurchaseNo, p2.PurchaseNo)
	}
	if _, err := v.e.pool.Exec(t.Context(), `UPDATE purchases SET note = 'edited' WHERE id = $1`, p1.ID); err == nil {
		t.Fatal("a received purchase was edited")
	}
	if _, err := v.e.pool.Exec(t.Context(), `UPDATE purchase_items SET unit_cost_cents = 1 WHERE purchase_id = $1`, p1.ID); err == nil {
		t.Fatal("a purchase line was edited")
	}
	if _, err := v.e.pool.Exec(t.Context(), `DELETE FROM purchases WHERE id = $1`, p2.ID); err == nil {
		t.Fatal("a purchase was deleted")
	}

	// A wrong receipt is undone by reversing its movement; the purchase itself stays received.
	var mv int64
	if err := v.e.pool.QueryRow(t.Context(), `SELECT id FROM inventory_movements WHERE item_id = $1 AND movement_type = 'receipt'`, glass).Scan(&mv); err != nil {
		t.Fatal(err)
	}
	v.call(v.store, "POST", "/api/v1/inventory/movements/"+itoa(mv)+"/reverse", map[string]any{"reason": "delivery rejected"}, 201, "")
	if v.balance(glass, v.mainStore) != 0 {
		t.Fatalf("glasses %d after reversing the receipt", v.balance(glass, v.mainStore))
	}
	v.receive(v.store, p1.ID, v.mainStore, newKey(), 409, "purchase_not_open")
}

func TestAC06_PurchaseRules(t *testing.T) {
	v := newInvEnv(t)
	it := v.item("Serviettes", v.pc)
	off := v.item("Old stock", v.pc)
	sup := v.supplier(v.store, "Corner wholesaler", 201, "")
	ok := []map[string]any{{"item_id": it, "qty": 5, "unit_cost_cents": 1000}}

	v.supplier(v.cash, "Nope Ltd", 403, "forbidden")
	v.buy(v.cash, map[string]any{"supplier_id": sup, "lines": ok}, newKey(), 403, "forbidden")
	v.buy(v.store, map[string]any{"supplier_id": sup, "lines": []map[string]any{}}, newKey(), 400, "validation_failed")
	v.buy(v.store, map[string]any{"supplier_id": 99999, "lines": ok}, newKey(), 400, "validation_failed")
	v.buy(v.store, map[string]any{"supplier_id": sup, "lines": []map[string]any{{"item_id": 99999, "qty": 1, "unit_cost_cents": 1}}}, newKey(), 400, "validation_failed")
	v.buy(v.store, map[string]any{"supplier_id": sup, "lines": []map[string]any{{"item_id": it, "qty": 1, "unit_cost_cents": -1}}}, newKey(), 400, "validation_failed")
	v.buy(v.store, map[string]any{"supplier_id": sup, "lines": []map[string]any{{"item_id": it, "qty": 1, "unit_cost_cents": 10000001}}}, newKey(), 400, "validation_failed")
	v.buy(v.store, map[string]any{"supplier_id": sup, "lines": []map[string]any{{"item_id": it, "qty": 0, "unit_cost_cents": 1}}}, newKey(), 400, "validation_failed")
	v.buy(v.store, map[string]any{"supplier_id": sup, "lines": []map[string]any{{"item_id": it, "qty": 1, "unit_cost_cents": 1}, {"item_id": it, "qty": 2, "unit_cost_cents": 1}}}, newKey(), 400, "validation_failed")
	v.call(v.mgr, "PATCH", "/api/v1/inventory/items/"+itoa(off), map[string]any{"active": false}, 200, "")
	v.buy(v.store, map[string]any{"supplier_id": sup, "lines": []map[string]any{{"item_id": off, "qty": 1, "unit_cost_cents": 1}}}, newKey(), 409, "item_inactive")
	if v.e.count(`SELECT count(*) FROM purchases`) != 0 {
		t.Fatal("a refused purchase left a row behind")
	}
	if v.e.count(`SELECT next_value FROM document_counters WHERE doc_type = 'purchase' AND scope = ''`) != 1 {
		t.Fatal("a refused purchase used up a number")
	}

	// A switched-off supplier cannot be bought from; cancelling a draft books nothing and closes it.
	p, _ := v.buy(v.store, map[string]any{"supplier_id": sup, "lines": ok}, newKey(), 201, "")
	v.call(v.store, "POST", "/api/v1/purchases/"+itoa(p.ID)+"/cancel", nil, 200, "")
	v.receive(v.store, p.ID, v.mainStore, newKey(), 409, "purchase_not_open")
	v.call(v.store, "PATCH", "/api/v1/suppliers/"+itoa(sup), map[string]any{"active": false}, 200, "")
	v.buy(v.store, map[string]any{"supplier_id": sup, "lines": ok}, newKey(), 409, "supplier_inactive")
	if v.balance(it, v.mainStore) != 0 {
		t.Fatal("stock appeared without a receive")
	}

	// Receiving needs a real place and the right to do it.
	v.call(v.store, "PATCH", "/api/v1/suppliers/"+itoa(sup), map[string]any{"active": true}, 200, "")
	p2, _ := v.buy(v.store, map[string]any{"supplier_id": sup, "lines": ok}, newKey(), 201, "")
	v.receive(v.store, p2.ID, 99999, newKey(), 400, "validation_failed")
	v.receive(v.cash, p2.ID, v.mainStore, newKey(), 403, "forbidden")
	v.receive(v.store, 99999, v.mainStore, newKey(), 404, "not_found")
	v.call(v.store, "GET", "/api/v1/purchases/"+itoa(p2.ID), nil, 200, "")
	v.call(v.cash, "GET", "/api/v1/purchases", nil, 403, "forbidden")
	v.call(v.store, "GET", "/api/v1/purchases?status=bogus", nil, 400, "validation_failed")
}

func TestAC06_ConcurrentReceivesBookOnce(t *testing.T) {
	v := newInvEnv(t)
	it := v.item("Charcoal bags", v.pc)
	sup := v.supplier(v.store, "Charcoal Ltd", 201, "")
	p, _ := v.buy(v.store, map[string]any{"supplier_id": sup, "lines": []map[string]any{{"item_id": it, "qty": 7, "unit_cost_cents": 90000}}}, newKey(), 201, "")
	codes := make([]int, 8)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := v.e.do(req{method: "POST", path: "/api/v1/purchases/" + itoa(p.ID) + "/receive", jar: v.store, header: map[string]string{"Idempotency-Key": newKey()},
				body: map[string]any{"location_id": v.mainStore}})
			codes[i] = w.Code
		}()
	}
	wg.Wait()
	won := 0
	for _, c := range codes {
		if c == 200 {
			won++
		} else if c != 409 {
			t.Fatalf("unexpected status %d", c)
		}
	}
	if won != 1 || v.balance(it, v.mainStore) != 7 {
		t.Fatalf("%d receives succeeded, balance %d; want exactly 1 and 7", won, v.balance(it, v.mainStore))
	}
}

// Stage 20: what actually arrived (S-85 to S-88).

type arrivedOut struct {
	Purchase struct {
		ID           int64  `json:"id"`
		Status       string `json:"status"`
		TotalCents   int64  `json:"total_cents"`
		OrderedCents int64  `json:"ordered_cents"`
		Lines        []struct {
			ItemID          int64  `json:"item_id"`
			ReceivedQty     *int64 `json:"received_qty"`
			ReceivedQtyBase *int64 `json:"received_qty_base"`
			ReceivedCost    *int64 `json:"received_unit_cost_cents"`
			LineCents       int64  `json:"line_cents"`
			ReceiptReversed bool   `json:"receipt_reversed"`
		} `json:"lines"`
	} `json:"purchase"`
}

func (v *invEnv) receiveLines(j jar, id, loc int64, key string, lines []map[string]any, want int, errCode string) *httptest.ResponseRecorder {
	v.t.Helper()
	w := v.e.do(req{method: "POST", path: "/api/v1/purchases/" + itoa(id) + "/receive", jar: j, header: map[string]string{"Idempotency-Key": key}, body: map[string]any{"location_id": loc, "lines": lines}})
	expect(v.t, w, want, errCode)
	return w
}

func TestAC06_ReceiveRecordsWhatActuallyArrived(t *testing.T) {
	v := newInvEnv(t)
	a, b := v.item("Lemons", v.pc), v.item("Limes", v.pc)
	sup := v.supplier(v.store, "Greengrocer", 201, "")
	order := []map[string]any{{"item_id": a, "qty": 10, "unit_cost_cents": 1000}, {"item_id": b, "qty": 4, "unit_cost_cents": 5000}}
	p, _ := v.buy(v.store, map[string]any{"supplier_id": sup, "lines": order}, newKey(), 201, "")
	ordered := int64(10*1000 + 4*5000)

	// Refused answers leave the purchase a draft with nothing recorded and no stock booked.
	v.receiveLines(v.store, p.ID, v.mainStore, newKey(), []map[string]any{{"item_id": a, "qty": 0}, {"item_id": b, "qty": 0}}, 400, "validation_failed") // nothing arrived
	v.receiveLines(v.store, p.ID, v.mainStore, newKey(), []map[string]any{{"item_id": 99999, "qty": 1}}, 400, "validation_failed")                       // not on the delivery
	v.receiveLines(v.store, p.ID, v.mainStore, newKey(), []map[string]any{{"item_id": a, "qty": 1}, {"item_id": a, "qty": 2}}, 400, "validation_failed") // twice
	v.receiveLines(v.store, p.ID, v.mainStore, newKey(), []map[string]any{{"item_id": a, "qty": -1}}, 400, "validation_failed")
	v.receiveLines(v.store, p.ID, v.mainStore, newKey(), []map[string]any{{"item_id": a, "qty": 1, "unit_cost_cents": 10000001}}, 400, "validation_failed")
	if n := v.e.count(`SELECT count(*) FROM purchase_items WHERE purchase_id = $1 AND received_qty IS NOT NULL`, p.ID); n != 0 {
		t.Fatalf("%d lines were marked received by refused requests", n)
	}
	if v.balance(a, v.mainStore) != 0 || v.e.count(`SELECT count(*) FROM purchases WHERE id = $1 AND status = 'draft'`, p.ID) != 1 {
		t.Fatal("a refused receive changed stock or the purchase")
	}

	// 8 of 10 lemons arrived, at a higher price; no limes came.
	key := newKey()
	lines := []map[string]any{{"item_id": a, "qty": 8, "unit_cost_cents": 1100}, {"item_id": b, "qty": 0}}
	got := decodeInto[arrivedOut](t, v.receiveLines(v.store, p.ID, v.mainStore, key, lines, 200, ""))
	if got.Purchase.Status != "received" || got.Purchase.TotalCents != 8*1100 || got.Purchase.OrderedCents != ordered {
		t.Fatalf("total %d (want %d), ordered %d (want %d)", got.Purchase.TotalCents, 8*1100, got.Purchase.OrderedCents, ordered)
	}
	if v.balance(a, v.mainStore) != 8 || v.balance(b, v.mainStore) != 0 {
		t.Fatalf("lemons %d (want 8), limes %d (want 0)", v.balance(a, v.mainStore), v.balance(b, v.mainStore))
	}
	if n := v.e.count(`SELECT unit_cost_cents FROM inventory_movements WHERE item_id = $1 AND movement_type = 'receipt'`, a); n != 1100 {
		t.Fatalf("receipt cost %d, want the price actually paid, 1100", n)
	}
	if n := v.e.count(`SELECT count(*) FROM inventory_movements WHERE item_id = $1 AND movement_type = 'receipt'`, b); n != 0 {
		t.Fatalf("%d receipts for the item that did not arrive", n)
	}
	// The order is still on the line beside what arrived.
	for _, l := range decodeInto[arrivedOut](t, v.call(v.store, "GET", "/api/v1/purchases/"+itoa(p.ID), nil, 200, "")).Purchase.Lines {
		if l.ReceivedQty == nil || l.ReceivedCost == nil {
			t.Fatalf("line %d does not say what arrived", l.ItemID)
		}
		if l.ItemID == b && (*l.ReceivedQty != 0 || l.LineCents != 0) {
			t.Fatalf("limes: received %d, line %d; want 0 and 0", *l.ReceivedQty, l.LineCents)
		}
	}
	if v.e.count(`SELECT unit_cost_cents FROM purchase_items WHERE purchase_id = $1 AND item_id = $2`, p.ID, a) != 1000 {
		t.Fatal("the ordered price was overwritten")
	}

	// The same key again replays; another key is refused.
	w := v.receiveLines(v.store, p.ID, v.mainStore, key, lines, 200, "")
	if w.Header().Get("Idempotent-Replayed") != "true" || v.balance(a, v.mainStore) != 8 {
		t.Fatal("a retry received twice")
	}
	v.receiveLines(v.store, p.ID, v.mainStore, newKey(), lines, 409, "purchase_not_open")

	// The database keeps what arrived fixed.
	if _, err := v.e.pool.Exec(t.Context(), `UPDATE purchase_items SET received_qty = 9, received_qty_base = 9 WHERE purchase_id = $1 AND item_id = $2`, p.ID, a); err == nil {
		t.Fatal("what arrived was edited after receiving")
	}
	if _, err := v.e.pool.Exec(t.Context(), `UPDATE purchase_items SET entered_qty = 99 WHERE purchase_id = $1 AND item_id = $2`, p.ID, a); err == nil {
		t.Fatal("the order on a line was edited")
	}
	p2, _ := v.buy(v.store, map[string]any{"supplier_id": sup, "lines": order[:1]}, newKey(), 201, "")
	if _, err := v.e.pool.Exec(t.Context(), `UPDATE purchases SET status = 'received', received_at = now(), received_by = created_by, location_id = $2 WHERE id = $1`, p2.ID, v.mainStore); err == nil {
		t.Fatal("a purchase was marked received without saying what arrived")
	}

	// Reversing the lemons' receipt shows on the delivery; the purchase stays received.
	var mv int64
	if err := v.e.pool.QueryRow(t.Context(), `SELECT id FROM inventory_movements WHERE item_id = $1 AND movement_type = 'receipt'`, a).Scan(&mv); err != nil {
		t.Fatal(err)
	}
	v.call(v.store, "POST", "/api/v1/inventory/movements/"+itoa(mv)+"/reverse", map[string]any{"reason": "bad batch, sent back"}, 201, "")
	for _, l := range decodeInto[arrivedOut](t, v.call(v.store, "GET", "/api/v1/purchases/"+itoa(p.ID), nil, 200, "")).Purchase.Lines {
		if l.ItemID == a && !l.ReceiptReversed {
			t.Fatal("a reversed receipt is not shown as reversed")
		}
		if l.ItemID == b && l.ReceiptReversed {
			t.Fatal("a line with no receipt is shown as reversed")
		}
	}
}
