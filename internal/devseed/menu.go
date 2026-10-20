package devseed

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"zuscar/backend/internal/catalogue/repository"
	idrepo "zuscar/backend/internal/identity/repository"
	ordersrepo "zuscar/backend/internal/orders/repository"
)

// A small FAKE menu so the catalogue and order screens have something to show. Prices are invented. Every product
// uses the "standard" tax category only because a product must name one; that is a development placeholder, not a
// tax decision (docs/tax-configuration.md). Seeded products have SKUs starting with DEV-.
type menuProduct struct {
	sku, name string
	cents     int64
	kitchen   bool
	mods      []string
}

type menuCategory struct {
	module, name string
	sort         int32
	products     []menuProduct
}

type menuModifier struct {
	name  string
	delta int64
}

var sampleModifiers = []menuModifier{
	{"Extra kachumbari", 5000}, {"No onions", 0}, {"Extra chilli", 0}, {"Add egg", 4000}, {"Double tot", 25000}, {"With ice", 0},
}

var sampleMenu = []menuCategory{
	{"restaurant", "Mains", 1, []menuProduct{
		{"DEV-R001", "Nyama choma (half kg)", 85000, true, []string{"Extra kachumbari", "No onions", "Extra chilli"}},
		{"DEV-R002", "Ugali and sukuma", 25000, true, []string{"Extra kachumbari", "Add egg"}},
		{"DEV-R003", "Pilau beef", 45000, true, []string{"No onions", "Extra chilli"}},
		{"DEV-R004", "Grilled tilapia", 120000, true, []string{"Extra chilli"}},
	}},
	{"restaurant", "Sides", 2, []menuProduct{
		{"DEV-R010", "Chips", 30000, true, []string{"Extra chilli"}},
		{"DEV-R011", "Kachumbari", 15000, true, nil},
	}},
	{"restaurant", "Hot drinks", 3, []menuProduct{
		{"DEV-R020", "Chai", 8000, true, nil},
	}},
	{"bar", "Soft drinks", 1, []menuProduct{
		{"DEV-B001", "Soda 300ml", 10000, false, []string{"With ice"}},
		{"DEV-B002", "Water 500ml", 6000, false, nil},
	}},
	{"bar", "Beer", 2, []menuProduct{
		{"DEV-B010", "Local lager 500ml", 35000, false, []string{"With ice"}},
	}},
	{"bar", "Spirits", 3, []menuProduct{
		{"DEV-B020", "House whisky tot", 25000, false, []string{"Double tot", "With ice"}},
	}},
}

// seedMenu adds the sample menu once. A database that already has any category is left alone, so a re-run never
// duplicates or overwrites what someone has since changed. It returns the number of products created.
func seedMenu(ctx context.Context, tx pgx.Tx, ir *idrepo.Queries) (int, error) {
	q := repository.New(tx)
	if n, err := q.CountCategories(ctx); err != nil || n > 0 {
		return 0, err
	}
	taxID, err := q.GetTaxRateIDByCode(ctx, "standard")
	if err != nil {
		return 0, err
	}
	modID := map[string]int64{}
	for _, m := range sampleModifiers {
		id, err := q.CreateModifier(ctx, repository.CreateModifierParams{Name: m.name, PriceDeltaCents: m.delta})
		if err != nil {
			return 0, err
		}
		modID[m.name] = id
	}
	count := 0
	for _, c := range sampleMenu {
		cid, err := q.CreateCategory(ctx, repository.CreateCategoryParams{Module: c.module, Name: c.name, SortOrder: c.sort})
		if err != nil {
			return 0, err
		}
		for _, p := range c.products {
			pid, err := q.CreateProduct(ctx, repository.CreateProductParams{CategoryID: cid, Module: c.module, Name: p.name,
				Sku: pgtype.Text{String: p.sku, Valid: true}, PriceCents: p.cents, TaxRateID: taxID, KitchenRouted: p.kitchen, StockUnitsPerSale: 1})
			if err != nil {
				return 0, err
			}
			if err := q.InsertPriceHistory(ctx, repository.InsertPriceHistoryParams{ProductID: pid, PriceCents: p.cents}); err != nil {
				return 0, err
			}
			for _, name := range p.mods {
				if err := q.InsertProductModifier(ctx, repository.InsertProductModifierParams{ProductID: pid, ModifierID: modID[name]}); err != nil {
					return 0, err
				}
			}
			count++
		}
	}
	after, _ := json.Marshal(map[string]any{"categories": len(sampleMenu), "products": count, "modifiers": len(sampleModifiers)})
	return count, ir.InsertAudit(ctx, idrepo.InsertAuditParams{ActorLabel: "cli:seed-dev", Action: "devseed.menu_created", EntityType: "catalogue", AfterJson: after})
}

// seedTables adds six fake dining tables once; a database that already has any table is left alone.
func seedTables(ctx context.Context, tx pgx.Tx) error {
	n, err := ordersrepo.New(tx).CountTables(ctx)
	if err != nil || n > 0 {
		return err
	}
	for i, seats := range []int32{2, 2, 4, 4, 6, 8} {
		if _, err := ordersrepo.New(tx).CreateTable(ctx, ordersrepo.CreateTableParams{Label: "Table " + string(rune('1'+i)), Seats: seats}); err != nil {
			return err
		}
	}
	return nil
}
