package handler_test

import (
	"context"
	"strconv"
	"testing"
)

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

type catItem struct {
	ID     int64  `json:"id"`
	Module string `json:"module"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
}

type catProduct struct {
	ID            int64   `json:"id"`
	CategoryID    int64   `json:"category_id"`
	Module        string  `json:"module"`
	Name          string  `json:"name"`
	SKU           *string `json:"sku"`
	PriceCents    int64   `json:"price_cents"`
	IsAvailable   bool    `json:"is_available"`
	KitchenRouted bool    `json:"kitchen_routed"`
	ModifierIDs   []int64 `json:"modifier_ids"`
	TaxRate       struct {
		ID        int64  `json:"id"`
		Treatment string `json:"treatment"`
	} `json:"tax_rate"`
}

type menuResp struct {
	Categories []struct {
		Name     string `json:"name"`
		Products []struct {
			ID        int64 `json:"id"`
			Modifiers []struct {
				ID int64 `json:"id"`
			} `json:"modifiers"`
		} `json:"products"`
	} `json:"categories"`
}

func (e *testEnv) rows(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestCatalogueLifecycle(t *testing.T) {
	e := newEnv(t)
	e.addUser("mgr", false, "business_manager")
	e.addUser("till", false, "cashier")
	e.addUser("cook", false, "kitchen_staff")
	e.addUser("serve", false, "waiter")
	mj, tj, kj, wj := e.mustLogin("mgr"), e.mustLogin("till"), e.mustLogin("cook"), e.mustLogin("serve")

	// Without catalogue.manage nothing can be read or changed; the kitchen role cannot even read the menu.
	expect(t, e.do(req{method: "GET", path: "/api/v1/categories", jar: tj}), 403, "forbidden")
	expect(t, e.do(req{method: "POST", path: "/api/v1/categories", jar: wj, body: map[string]any{"module": "bar", "name": "x"}}), 403, "forbidden")
	expect(t, e.do(req{method: "GET", path: "/api/v1/menu", jar: kj}), 403, "forbidden")

	// Categories: module is fixed, names are unique per module.
	expect(t, e.do(req{method: "POST", path: "/api/v1/categories", jar: mj, body: map[string]any{"module": "rooms", "name": "Mains"}}), 400, "validation_failed")
	w := e.do(req{method: "POST", path: "/api/v1/categories", jar: mj, body: map[string]any{"module": "restaurant", "name": "Mains", "sort_order": 1}})
	expect(t, w, 201, "")
	mains := decodeInto[struct {
		Category catItem `json:"category"`
	}](t, w).Category
	expect(t, e.do(req{method: "POST", path: "/api/v1/categories", jar: mj, body: map[string]any{"module": "restaurant", "name": "Mains"}}), 409, "conflict")
	w = e.do(req{method: "POST", path: "/api/v1/categories", jar: mj, body: map[string]any{"module": "bar", "name": "Mains"}})
	expect(t, w, 201, "") // the same name in another module is fine
	barCat := decodeInto[struct {
		Category catItem `json:"category"`
	}](t, w).Category

	rates := e.taxRates(mj)
	var standard, exempt int64
	for _, r := range rates {
		if r.Treatment == "standard" {
			standard = r.ID
		}
		if r.Treatment == "exempt" {
			exempt = r.ID
		}
	}

	// A product must name its tax category: there is no implicit standard rate.
	noTax := map[string]any{"category_id": mains.ID, "name": "Nyama choma", "price_cents": 85000, "kitchen_routed": true}
	w = e.do(req{method: "POST", path: "/api/v1/products", jar: mj, body: noTax})
	expect(t, w, 400, "validation_failed")
	expect(t, e.do(req{method: "POST", path: "/api/v1/products", jar: mj, body: map[string]any{"category_id": mains.ID, "name": "X", "price_cents": 100, "tax_rate_id": standard}}), 400, "validation_failed") // kitchen_routed missing
	expect(t, e.do(req{method: "POST", path: "/api/v1/products", jar: mj, body: map[string]any{"category_id": mains.ID, "name": "Free", "price_cents": -1, "tax_rate_id": standard, "kitchen_routed": true}}), 400, "validation_failed")
	expect(t, e.do(req{method: "POST", path: "/api/v1/products", jar: mj, body: map[string]any{"category_id": mains.ID, "name": "Ghost", "price_cents": 100, "tax_rate_id": 99999, "kitchen_routed": true}}), 400, "validation_failed")

	good := map[string]any{"category_id": mains.ID, "name": "Nyama choma", "sku": "NC-01", "price_cents": 85000, "tax_rate_id": standard, "kitchen_routed": true}
	w = e.do(req{method: "POST", path: "/api/v1/products", jar: mj, body: good})
	expect(t, w, 201, "")
	p := decodeInto[struct {
		Product catProduct `json:"product"`
	}](t, w).Product
	if p.Module != "restaurant" || p.PriceCents != 85000 || !p.IsAvailable || p.TaxRate.ID != standard {
		t.Fatalf("product = %+v", p)
	}
	if e.rows(`SELECT count(*) FROM price_history WHERE product_id = $1`, p.ID) != 1 {
		t.Fatal("the opening price must be the first price_history row")
	}
	expect(t, e.do(req{method: "POST", path: "/api/v1/products", jar: mj, body: good}), 409, "conflict") // same SKU

	// A deactivated category or tax category cannot receive new products.
	e.exec(`UPDATE tax_rates SET active = false WHERE id = $1`, exempt)
	expect(t, e.do(req{method: "POST", path: "/api/v1/products", jar: mj, body: map[string]any{"category_id": mains.ID, "name": "Water", "price_cents": 100, "tax_rate_id": exempt, "kitchen_routed": false}}), 400, "validation_failed")
	e.exec(`UPDATE tax_rates SET active = true WHERE id = $1`, exempt)

	// A product stays in its module.
	expect(t, e.do(req{method: "PATCH", path: "/api/v1/products/" + itoa(p.ID), jar: mj, body: map[string]any{"category_id": barCat.ID}}), 400, "validation_failed")
	if e.rows(`SELECT count(*) FROM products WHERE id = $1 AND module = 'restaurant'`, p.ID) != 1 {
		t.Fatal("module changed")
	}
	// The database backs that rule too.
	if _, err := e.pool.Exec(context.Background(), `UPDATE products SET module = 'bar' WHERE id = $1`, p.ID); err == nil {
		t.Fatal("products.module must match its category's module in the database")
	}
	w = e.do(req{method: "PATCH", path: "/api/v1/products/" + itoa(p.ID), jar: mj, body: map[string]any{"name": "Nyama choma (half)", "tax_rate_id": exempt, "sku": ""}})
	expect(t, w, 200, "")
	p = decodeInto[struct {
		Product catProduct `json:"product"`
	}](t, w).Product
	if p.Name != "Nyama choma (half)" || p.SKU != nil || p.TaxRate.Treatment != "exempt" {
		t.Fatalf("after patch: %+v", p)
	}

	// Price change: the old price stays in immutable history and the change is audited.
	expect(t, e.do(req{method: "PUT", path: "/api/v1/products/" + itoa(p.ID) + "/price", jar: mj, body: map[string]any{"price_cents": -5}}), 400, "validation_failed")
	expect(t, e.do(req{method: "PUT", path: "/api/v1/products/" + itoa(p.ID) + "/price", jar: mj, body: map[string]any{}}), 400, "validation_failed")
	expect(t, e.do(req{method: "PUT", path: "/api/v1/products/" + itoa(p.ID) + "/price", jar: mj, body: map[string]any{"price_cents": 90000}}), 200, "")
	expect(t, e.do(req{method: "PUT", path: "/api/v1/products/" + itoa(p.ID) + "/price", jar: mj, body: map[string]any{"price_cents": 90000}}), 200, "") // unchanged: no new row
	if n := e.rows(`SELECT count(*) FROM price_history WHERE product_id = $1`, p.ID); n != 2 {
		t.Fatalf("price_history rows = %d, want 2", n)
	}
	if e.auditN("product.price_changed") != 1 {
		t.Fatal("price change must be audited once")
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE price_history SET price_cents = 1`); err == nil {
		t.Fatal("price_history must reject UPDATE")
	}
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM price_history`); err == nil {
		t.Fatal("price_history must reject DELETE")
	}

	// Modifiers, links and the menu.
	w = e.do(req{method: "POST", path: "/api/v1/modifiers", jar: mj, body: map[string]any{"name": "Extra kachumbari", "price_delta_cents": 5000}})
	expect(t, w, 201, "")
	mod := decodeInto[struct {
		Modifier struct {
			ID int64 `json:"id"`
		} `json:"modifier"`
	}](t, w).Modifier
	expect(t, e.do(req{method: "POST", path: "/api/v1/modifiers", jar: mj, body: map[string]any{"name": "Extra kachumbari"}}), 409, "conflict")
	expect(t, e.do(req{method: "PATCH", path: "/api/v1/products/" + itoa(p.ID), jar: mj, body: map[string]any{"modifier_ids": []int64{mod.ID, 99999}}}), 400, "validation_failed")
	expect(t, e.do(req{method: "PATCH", path: "/api/v1/products/" + itoa(p.ID), jar: mj, body: map[string]any{"modifier_ids": []int64{mod.ID}}}), 200, "")

	menu := func() menuResp {
		w := e.do(req{method: "GET", path: "/api/v1/menu", jar: wj})
		expect(t, w, 200, "")
		return decodeInto[menuResp](t, w)
	}
	m := menu()
	if len(m.Categories) != 1 || len(m.Categories[0].Products) != 1 || len(m.Categories[0].Products[0].Modifiers) != 1 {
		t.Fatalf("menu = %+v", m)
	}
	expect(t, e.do(req{method: "PATCH", path: "/api/v1/modifiers/" + itoa(mod.ID), jar: mj, body: map[string]any{"active": false}}), 200, "")
	if len(menu().Categories[0].Products[0].Modifiers) != 0 {
		t.Fatal("an inactive modifier must not be on the menu")
	}

	// Availability: an unavailable product leaves the menu (and a category with nothing left disappears).
	expect(t, e.do(req{method: "PUT", path: "/api/v1/products/" + itoa(p.ID) + "/availability", jar: mj, body: map[string]any{}}), 400, "validation_failed")
	expect(t, e.do(req{method: "PUT", path: "/api/v1/products/" + itoa(p.ID) + "/availability", jar: mj, body: map[string]any{"is_available": false}}), 200, "")
	if len(menu().Categories) != 0 {
		t.Fatal("an unavailable product must not be on the menu")
	}
	expect(t, e.do(req{method: "PUT", path: "/api/v1/products/" + itoa(p.ID) + "/availability", jar: mj, body: map[string]any{"is_available": true}}), 200, "")
	expect(t, e.do(req{method: "PATCH", path: "/api/v1/categories/" + itoa(mains.ID), jar: mj, body: map[string]any{"active": false}}), 200, "")
	if len(menu().Categories) != 0 {
		t.Fatal("a deactivated category must not be on the menu")
	}

	// Reads and history.
	w = e.do(req{method: "GET", path: "/api/v1/products?module=restaurant&q=nyama", jar: mj})
	expect(t, w, 200, "")
	if got := decodeInto[struct {
		Items []catProduct `json:"items"`
	}](t, w).Items; len(got) != 1 {
		t.Fatalf("filtered products = %d", len(got))
	}
	expect(t, e.do(req{method: "GET", path: "/api/v1/products?q=%25", jar: mj}), 200, "") // a wildcard is searched for literally
	w = e.do(req{method: "GET", path: "/api/v1/products/" + itoa(p.ID) + "/price-history", jar: mj})
	expect(t, w, 200, "")
	expect(t, e.do(req{method: "GET", path: "/api/v1/products/99999/price-history", jar: mj}), 404, "not_found")
	expect(t, e.do(req{method: "PATCH", path: "/api/v1/products/99999", jar: mj, body: map[string]any{"name": "x"}}), 404, "not_found")
	for _, a := range []string{"category.created", "product.created", "product.updated", "product.availability_changed", "modifier.created", "modifier.updated", "category.updated"} {
		if e.auditN(a) == 0 {
			t.Errorf("no audit row for %s", a)
		}
	}
}
