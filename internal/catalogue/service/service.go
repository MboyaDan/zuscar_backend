// Package service is the catalogue module's rules (Stage 9a): categories, products, modifiers, price history and
// availability. Every change needs catalogue.manage and writes an audit row with before and after values in the
// same transaction. Money is integer cents. A product must name its tax category explicitly; nothing is assumed
// to be standard-rated (docs/tax-configuration.md).
package service

import (
	"context"
	"errors"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"zuscar/backend/internal/catalogue/repository"
	"zuscar/backend/internal/platform/audit"
	"zuscar/backend/internal/platform/auth"
)

var ErrForbidden = errors.New("you do not have permission to do this")

// NotFoundError names what was not found, so the answer can say so.
type NotFoundError struct{ What string }

func (e *NotFoundError) Error() string { return e.What + " not found" }

// ValidationError is a field-level input problem.
type ValidationError struct{ Field, Msg string }

func (e *ValidationError) Error() string { return e.Field + " " + e.Msg }

// ConflictError is a uniqueness clash (a name or SKU already in use).
type ConflictError struct{ Field, Msg string }

func (e *ConflictError) Error() string { return e.Field + " " + e.Msg }

type Meta struct {
	RequestID string
	IP        *netip.Addr
}

type Service struct {
	pool *pgxpool.Pool
	rec  audit.Recorder
}

func New(pool *pgxpool.Pool, rec audit.Recorder) *Service { return &Service{pool: pool, rec: rec} }

const maxPriceCents = 100_000_000 // KES 1,000,000: a guard against a stray extra zero, not a business rule

func (s *Service) manage(actor *auth.Principal) error {
	if actor == nil || !actor.Has("catalogue.manage") {
		return ErrForbidden
	}
	return nil
}

func (s *Service) record(ctx context.Context, tx pgx.Tx, actor *auth.Principal, m Meta, action, entity string, id int64, before, after map[string]any) error {
	return s.rec.Record(ctx, tx, audit.Entry{ActorID: actor.UserID, ActorLabel: actor.Username, Action: action, EntityType: entity,
		EntityID: itoa(id), Before: before, After: after, RequestID: m.RequestID, IP: m.IP})
}

// ---- views ----

type CategoryView struct {
	ID        int64  `json:"id"`
	Module    string `json:"module"`
	Name      string `json:"name"`
	SortOrder int32  `json:"sort_order"`
	Active    bool   `json:"active"`
}

type ModifierView struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	PriceDeltaCents int64  `json:"price_delta_cents"`
	Active          bool   `json:"active"`
}

type TaxView struct {
	ID        int64  `json:"id"`
	Code      string `json:"code"`
	Name      string `json:"name"`
	RateBP    int32  `json:"rate_bp"`
	Treatment string `json:"treatment"`
}

type ProductView struct {
	ID            int64     `json:"id"`
	CategoryID    int64     `json:"category_id"`
	CategoryName  string    `json:"category_name"`
	Module        string    `json:"module"`
	Name          string    `json:"name"`
	SKU           *string   `json:"sku"`
	PriceCents    int64     `json:"price_cents"`
	TaxRate       TaxView   `json:"tax_rate"`
	IsAvailable   bool      `json:"is_available"`
	KitchenRouted bool      `json:"kitchen_routed"`
	ModifierIDs   []int64   `json:"modifier_ids"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type PriceHistoryView struct {
	PriceCents    int64     `json:"price_cents"`
	EffectiveFrom time.Time `json:"effective_from"`
	ChangedBy     *int64    `json:"changed_by"`
}

func catView(c repository.Category) CategoryView {
	return CategoryView{ID: c.ID, Module: c.Module, Name: c.Name, SortOrder: c.SortOrder, Active: c.Active}
}

func modView(m repository.Modifier) ModifierView {
	return ModifierView{ID: m.ID, Name: m.Name, PriceDeltaCents: m.PriceDeltaCents, Active: m.Active}
}

func prodView(r repository.GetProductRow, mods []int64) ProductView {
	var sku *string
	if r.Sku.Valid {
		v := r.Sku.String
		sku = &v
	}
	if mods == nil {
		mods = []int64{}
	}
	return ProductView{ID: r.ID, CategoryID: r.CategoryID, CategoryName: r.CategoryName, Module: r.Module, Name: r.Name, SKU: sku,
		PriceCents: r.PriceCents, TaxRate: TaxView{ID: r.TaxRateID, Code: r.TaxCode, Name: r.TaxName, RateBP: r.TaxRateBp, Treatment: r.TaxTreatment},
		IsAvailable: r.IsAvailable, KitchenRouted: r.KitchenRouted, ModifierIDs: mods, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time}
}

// ---- validation helpers ----

func checkName(field, v string, max int) (string, error) {
	v = strings.TrimSpace(v)
	if n := len([]rune(v)); n < 1 || n > max {
		return "", &ValidationError{Field: field, Msg: "must be 1 to " + itoa(int64(max)) + " characters"}
	}
	return v, nil
}

func checkModule(v string) error {
	if v != "restaurant" && v != "bar" {
		return &ValidationError{Field: "module", Msg: "must be restaurant or bar"}
	}
	return nil
}

func checkPrice(field string, cents int64) error {
	if cents < 0 || cents > maxPriceCents {
		return &ValidationError{Field: field, Msg: "must be between 0 and " + itoa(maxPriceCents) + " cents"}
	}
	return nil
}

func checkDelta(cents int64) error {
	if cents < -maxPriceCents || cents > maxPriceCents {
		return &ValidationError{Field: "price_delta_cents", Msg: "is outside the allowed range"}
	}
	return nil
}

var skuPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)

// skuText returns a NULL for an empty SKU.
func skuText(v string) (pgtype.Text, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return pgtype.Text{}, nil
	}
	if !skuPattern.MatchString(v) {
		return pgtype.Text{}, &ValidationError{Field: "sku", Msg: "must be 1 to 40 letters, digits, dot, dash or underscore"}
	}
	return pgtype.Text{String: v, Valid: true}, nil
}

func conflict(err error) error {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23505" {
		return nil
	}
	switch pe.ConstraintName {
	case "categories_module_name_key":
		return &ConflictError{Field: "name", Msg: "is already used by a category in this module"}
	case "modifiers_name_key":
		return &ConflictError{Field: "name", Msg: "is already used by another modifier"}
	case "products_sku_key":
		return &ConflictError{Field: "sku", Msg: "is already used by another product"}
	}
	return nil
}

// ---- categories ----

func (s *Service) ListCategories(ctx context.Context, actor *auth.Principal) ([]CategoryView, error) {
	if err := s.manage(actor); err != nil {
		return nil, err
	}
	rows, err := repository.New(s.pool).ListCategories(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]CategoryView, 0, len(rows))
	for _, r := range rows {
		out = append(out, catView(r))
	}
	return out, nil
}

func (s *Service) CreateCategory(ctx context.Context, actor *auth.Principal, module, name string, sortOrder int32, m Meta) (CategoryView, error) {
	if err := s.manage(actor); err != nil {
		return CategoryView{}, err
	}
	if err := checkModule(module); err != nil {
		return CategoryView{}, err
	}
	name, err := checkName("name", name, 80)
	if err != nil {
		return CategoryView{}, err
	}
	var out CategoryView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		id, err := q.CreateCategory(ctx, repository.CreateCategoryParams{Module: module, Name: name, SortOrder: sortOrder})
		if err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "category.created", "category", id, nil, map[string]any{"module": module, "name": name, "sort_order": sortOrder}); err != nil {
			return err
		}
		row, err := q.GetCategory(ctx, id)
		out = catView(row)
		return err
	})
	if c := conflict(err); c != nil {
		return CategoryView{}, c
	}
	return out, err
}

type CategoryChange struct {
	Name      *string
	SortOrder *int32
	Active    *bool
}

func (s *Service) UpdateCategory(ctx context.Context, actor *auth.Principal, id int64, ch CategoryChange, m Meta) (CategoryView, error) {
	if err := s.manage(actor); err != nil {
		return CategoryView{}, err
	}
	var out CategoryView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := q.GetCategory(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "category"}
		}
		if err != nil {
			return err
		}
		name, sortOrder, active := cur.Name, cur.SortOrder, cur.Active
		if ch.Name != nil {
			if name, err = checkName("name", *ch.Name, 80); err != nil {
				return err
			}
		}
		if ch.SortOrder != nil {
			sortOrder = *ch.SortOrder
		}
		if ch.Active != nil {
			active = *ch.Active
		}
		if name == cur.Name && sortOrder == cur.SortOrder && active == cur.Active {
			out = catView(cur)
			return nil
		}
		if err := q.UpdateCategory(ctx, repository.UpdateCategoryParams{ID: id, Name: name, SortOrder: sortOrder, Active: active}); err != nil {
			return err
		}
		before := map[string]any{"name": cur.Name, "sort_order": cur.SortOrder, "active": cur.Active}
		after := map[string]any{"name": name, "sort_order": sortOrder, "active": active}
		if err := s.record(ctx, tx, actor, m, "category.updated", "category", id, before, after); err != nil {
			return err
		}
		row, err := q.GetCategory(ctx, id)
		out = catView(row)
		return err
	})
	if c := conflict(err); c != nil {
		return CategoryView{}, c
	}
	return out, err
}

// ---- modifiers ----

func (s *Service) ListModifiers(ctx context.Context, actor *auth.Principal) ([]ModifierView, error) {
	if err := s.manage(actor); err != nil {
		return nil, err
	}
	rows, err := repository.New(s.pool).ListModifiers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ModifierView, 0, len(rows))
	for _, r := range rows {
		out = append(out, modView(r))
	}
	return out, nil
}

func (s *Service) CreateModifier(ctx context.Context, actor *auth.Principal, name string, delta int64, m Meta) (ModifierView, error) {
	if err := s.manage(actor); err != nil {
		return ModifierView{}, err
	}
	name, err := checkName("name", name, 80)
	if err != nil {
		return ModifierView{}, err
	}
	if err := checkDelta(delta); err != nil {
		return ModifierView{}, err
	}
	var out ModifierView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		id, err := q.CreateModifier(ctx, repository.CreateModifierParams{Name: name, PriceDeltaCents: delta})
		if err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "modifier.created", "modifier", id, nil, map[string]any{"name": name, "price_delta_cents": delta}); err != nil {
			return err
		}
		row, err := q.GetModifier(ctx, id)
		out = modView(row)
		return err
	})
	if c := conflict(err); c != nil {
		return ModifierView{}, c
	}
	return out, err
}

type ModifierChange struct {
	Name            *string
	PriceDeltaCents *int64
	Active          *bool
}

func (s *Service) UpdateModifier(ctx context.Context, actor *auth.Principal, id int64, ch ModifierChange, m Meta) (ModifierView, error) {
	if err := s.manage(actor); err != nil {
		return ModifierView{}, err
	}
	var out ModifierView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := q.GetModifier(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "modifier"}
		}
		if err != nil {
			return err
		}
		name, delta, active := cur.Name, cur.PriceDeltaCents, cur.Active
		if ch.Name != nil {
			if name, err = checkName("name", *ch.Name, 80); err != nil {
				return err
			}
		}
		if ch.PriceDeltaCents != nil {
			if err := checkDelta(*ch.PriceDeltaCents); err != nil {
				return err
			}
			delta = *ch.PriceDeltaCents
		}
		if ch.Active != nil {
			active = *ch.Active
		}
		if name == cur.Name && delta == cur.PriceDeltaCents && active == cur.Active {
			out = modView(cur)
			return nil
		}
		if err := q.UpdateModifier(ctx, repository.UpdateModifierParams{ID: id, Name: name, PriceDeltaCents: delta, Active: active}); err != nil {
			return err
		}
		before := map[string]any{"name": cur.Name, "price_delta_cents": cur.PriceDeltaCents, "active": cur.Active}
		after := map[string]any{"name": name, "price_delta_cents": delta, "active": active}
		if err := s.record(ctx, tx, actor, m, "modifier.updated", "modifier", id, before, after); err != nil {
			return err
		}
		row, err := q.GetModifier(ctx, id)
		out = modView(row)
		return err
	})
	if c := conflict(err); c != nil {
		return ModifierView{}, c
	}
	return out, err
}

// ---- products ----

type ProductFilter struct {
	CategoryID *int64
	Module     string
	Available  *bool
	Query      string
}

func likeEscape(v string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(v)
}

func (s *Service) links(ctx context.Context, q *repository.Queries, ids []int64) (map[int64][]int64, error) {
	out := map[int64][]int64{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.ListProductModifierLinks(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ProductID] = append(out[r.ProductID], r.ModifierID)
	}
	return out, nil
}

func (s *Service) ListProducts(ctx context.Context, actor *auth.Principal, f ProductFilter) ([]ProductView, error) {
	if err := s.manage(actor); err != nil {
		return nil, err
	}
	if f.Module != "" {
		if err := checkModule(f.Module); err != nil {
			return nil, err
		}
	}
	p := repository.ListProductsParams{}
	if f.CategoryID != nil {
		p.CategoryID = pgtype.Int8{Int64: *f.CategoryID, Valid: true}
	}
	if f.Module != "" {
		p.Module = pgtype.Text{String: f.Module, Valid: true}
	}
	if f.Available != nil {
		p.Available = pgtype.Bool{Bool: *f.Available, Valid: true}
	}
	if qs := strings.TrimSpace(f.Query); qs != "" {
		p.Q = pgtype.Text{String: likeEscape(qs), Valid: true}
	}
	q := repository.New(s.pool)
	rows, err := q.ListProducts(ctx, p)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	links, err := s.links(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	out := make([]ProductView, 0, len(rows))
	for _, r := range rows {
		out = append(out, prodView(repository.GetProductRow(r), links[r.ID]))
	}
	return out, nil
}

func (s *Service) getProduct(ctx context.Context, q *repository.Queries, id int64) (ProductView, error) {
	row, err := q.GetProduct(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProductView{}, &NotFoundError{What: "product"}
	}
	if err != nil {
		return ProductView{}, err
	}
	links, err := s.links(ctx, q, []int64{id})
	if err != nil {
		return ProductView{}, err
	}
	return prodView(row, links[id]), nil
}

func (s *Service) checkTax(ctx context.Context, q *repository.Queries, id int64) error {
	t, err := q.GetTaxRateForProduct(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !t.Active) {
		return &ValidationError{Field: "tax_rate_id", Msg: "must be an active tax category"}
	}
	return err
}

func (s *Service) checkModifiers(ctx context.Context, q *repository.Queries, ids []int64) ([]int64, error) {
	seen := map[int64]bool{}
	clean := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, &ValidationError{Field: "modifier_ids", Msg: "contains an invalid id"}
		}
		if !seen[id] {
			seen[id] = true
			clean = append(clean, id)
		}
	}
	sort.Slice(clean, func(i, j int) bool { return clean[i] < clean[j] })
	if len(clean) > 0 {
		n, err := q.CountModifiersByID(ctx, clean)
		if err != nil {
			return nil, err
		}
		if int(n) != len(clean) {
			return nil, &ValidationError{Field: "modifier_ids", Msg: "contains a modifier that does not exist"}
		}
	}
	return clean, nil
}

func (s *Service) setLinks(ctx context.Context, q *repository.Queries, productID int64, ids []int64) error {
	if err := q.DeleteProductModifiers(ctx, productID); err != nil {
		return err
	}
	for _, id := range ids {
		if err := q.InsertProductModifier(ctx, repository.InsertProductModifierParams{ProductID: productID, ModifierID: id}); err != nil {
			return err
		}
	}
	return nil
}

type NewProduct struct {
	CategoryID    int64
	Name          string
	SKU           string
	PriceCents    int64
	TaxRateID     int64
	KitchenRouted bool
	ModifierIDs   []int64
}

// CreateProduct adds a product in an active category (its module comes from the category). The opening price is
// also the first price_history row.
func (s *Service) CreateProduct(ctx context.Context, actor *auth.Principal, in NewProduct, m Meta) (ProductView, error) {
	if err := s.manage(actor); err != nil {
		return ProductView{}, err
	}
	name, err := checkName("name", in.Name, 120)
	if err != nil {
		return ProductView{}, err
	}
	sku, err := skuText(in.SKU)
	if err != nil {
		return ProductView{}, err
	}
	if err := checkPrice("price_cents", in.PriceCents); err != nil {
		return ProductView{}, err
	}
	var out ProductView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cat, err := q.GetCategory(ctx, in.CategoryID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !cat.Active) {
			return &ValidationError{Field: "category_id", Msg: "must be an active category"}
		}
		if err != nil {
			return err
		}
		if err := s.checkTax(ctx, q, in.TaxRateID); err != nil {
			return err
		}
		mods, err := s.checkModifiers(ctx, q, in.ModifierIDs)
		if err != nil {
			return err
		}
		id, err := q.CreateProduct(ctx, repository.CreateProductParams{CategoryID: cat.ID, Module: cat.Module, Name: name, Sku: sku,
			PriceCents: in.PriceCents, TaxRateID: in.TaxRateID, KitchenRouted: in.KitchenRouted})
		if err != nil {
			return err
		}
		if err := q.InsertPriceHistory(ctx, repository.InsertPriceHistoryParams{ProductID: id, PriceCents: in.PriceCents, ChangedBy: pgtype.Int8{Int64: actor.UserID, Valid: true}}); err != nil {
			return err
		}
		if err := s.setLinks(ctx, q, id, mods); err != nil {
			return err
		}
		after := map[string]any{"category_id": cat.ID, "module": cat.Module, "name": name, "sku": sku.String, "price_cents": in.PriceCents,
			"tax_rate_id": in.TaxRateID, "kitchen_routed": in.KitchenRouted, "modifier_ids": mods}
		if err := s.record(ctx, tx, actor, m, "product.created", "product", id, nil, after); err != nil {
			return err
		}
		out, err = s.getProduct(ctx, q, id)
		return err
	})
	if c := conflict(err); c != nil {
		return ProductView{}, c
	}
	return out, err
}

type ProductChange struct {
	CategoryID    *int64
	Name          *string
	SKU           *string // empty text clears the SKU
	TaxRateID     *int64
	KitchenRouted *bool
	ModifierIDs   *[]int64
}

// UpdateProduct changes descriptive fields, never the price or availability (they have their own endpoints, so
// each is audited as its own event). A product stays in its module: it may only move to a category of the same module.
func (s *Service) UpdateProduct(ctx context.Context, actor *auth.Principal, id int64, ch ProductChange, m Meta) (ProductView, error) {
	if err := s.manage(actor); err != nil {
		return ProductView{}, err
	}
	var out ProductView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := s.getProduct(ctx, q, id)
		if err != nil {
			return err
		}
		name, categoryID, taxID, kitchen := cur.Name, cur.CategoryID, cur.TaxRate.ID, cur.KitchenRouted
		sku := pgtype.Text{}
		if cur.SKU != nil {
			sku = pgtype.Text{String: *cur.SKU, Valid: true}
		}
		if ch.Name != nil {
			if name, err = checkName("name", *ch.Name, 120); err != nil {
				return err
			}
		}
		if ch.SKU != nil {
			if sku, err = skuText(*ch.SKU); err != nil {
				return err
			}
		}
		if ch.CategoryID != nil && *ch.CategoryID != cur.CategoryID {
			cat, err := q.GetCategory(ctx, *ch.CategoryID)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && !cat.Active) {
				return &ValidationError{Field: "category_id", Msg: "must be an active category"}
			}
			if err != nil {
				return err
			}
			if cat.Module != cur.Module {
				return &ValidationError{Field: "category_id", Msg: "must be a category in the same module (" + cur.Module + ")"}
			}
			categoryID = cat.ID
		}
		if ch.TaxRateID != nil && *ch.TaxRateID != cur.TaxRate.ID {
			if err := s.checkTax(ctx, q, *ch.TaxRateID); err != nil {
				return err
			}
			taxID = *ch.TaxRateID
		}
		if ch.KitchenRouted != nil {
			kitchen = *ch.KitchenRouted
		}
		mods := cur.ModifierIDs
		if ch.ModifierIDs != nil {
			if mods, err = s.checkModifiers(ctx, q, *ch.ModifierIDs); err != nil {
				return err
			}
		}
		sameMods := len(mods) == len(cur.ModifierIDs)
		for i := 0; sameMods && i < len(mods); i++ {
			sameMods = mods[i] == cur.ModifierIDs[i]
		}
		curSKU := ""
		if cur.SKU != nil {
			curSKU = *cur.SKU
		}
		if name == cur.Name && categoryID == cur.CategoryID && taxID == cur.TaxRate.ID && kitchen == cur.KitchenRouted && sku.String == curSKU && sameMods {
			out = cur
			return nil
		}
		if err := q.UpdateProduct(ctx, repository.UpdateProductParams{ID: id, CategoryID: categoryID, Module: cur.Module, Name: name, Sku: sku, TaxRateID: taxID, KitchenRouted: kitchen}); err != nil {
			return err
		}
		if !sameMods {
			if err := s.setLinks(ctx, q, id, mods); err != nil {
				return err
			}
		}
		before := map[string]any{"category_id": cur.CategoryID, "name": cur.Name, "sku": curSKU, "tax_rate_id": cur.TaxRate.ID, "kitchen_routed": cur.KitchenRouted, "modifier_ids": cur.ModifierIDs}
		after := map[string]any{"category_id": categoryID, "name": name, "sku": sku.String, "tax_rate_id": taxID, "kitchen_routed": kitchen, "modifier_ids": mods}
		if err := s.record(ctx, tx, actor, m, "product.updated", "product", id, before, after); err != nil {
			return err
		}
		out, err = s.getProduct(ctx, q, id)
		return err
	})
	if c := conflict(err); c != nil {
		return ProductView{}, c
	}
	return out, err
}

// SetPrice changes the price. The old price stays in price_history (immutable) and the change is audited.
func (s *Service) SetPrice(ctx context.Context, actor *auth.Principal, id int64, cents int64, m Meta) (ProductView, error) {
	if err := s.manage(actor); err != nil {
		return ProductView{}, err
	}
	if err := checkPrice("price_cents", cents); err != nil {
		return ProductView{}, err
	}
	var out ProductView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := s.getProduct(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.PriceCents == cents {
			out = cur
			return nil
		}
		if err := q.SetProductPrice(ctx, repository.SetProductPriceParams{ID: id, PriceCents: cents}); err != nil {
			return err
		}
		if err := q.InsertPriceHistory(ctx, repository.InsertPriceHistoryParams{ProductID: id, PriceCents: cents, ChangedBy: pgtype.Int8{Int64: actor.UserID, Valid: true}}); err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "product.price_changed", "product", id, map[string]any{"price_cents": cur.PriceCents}, map[string]any{"price_cents": cents}); err != nil {
			return err
		}
		out, err = s.getProduct(ctx, q, id)
		return err
	})
	return out, err
}

func (s *Service) SetAvailability(ctx context.Context, actor *auth.Principal, id int64, available bool, m Meta) (ProductView, error) {
	if err := s.manage(actor); err != nil {
		return ProductView{}, err
	}
	var out ProductView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := s.getProduct(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.IsAvailable == available {
			out = cur
			return nil
		}
		if err := q.SetProductAvailability(ctx, repository.SetProductAvailabilityParams{ID: id, IsAvailable: available}); err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "product.availability_changed", "product", id, map[string]any{"is_available": cur.IsAvailable}, map[string]any{"is_available": available}); err != nil {
			return err
		}
		out, err = s.getProduct(ctx, q, id)
		return err
	})
	return out, err
}

func (s *Service) PriceHistory(ctx context.Context, actor *auth.Principal, id int64) ([]PriceHistoryView, error) {
	if err := s.manage(actor); err != nil {
		return nil, err
	}
	q := repository.New(s.pool)
	if _, err := s.getProduct(ctx, q, id); err != nil {
		return nil, err
	}
	rows, err := q.ListPriceHistory(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]PriceHistoryView, 0, len(rows))
	for _, r := range rows {
		v := PriceHistoryView{PriceCents: r.PriceCents, EffectiveFrom: r.EffectiveFrom.Time}
		if r.ChangedBy.Valid {
			c := r.ChangedBy.Int64
			v.ChangedBy = &c
		}
		out = append(out, v)
	}
	return out, nil
}

// ---- menu (what order-taking staff may sell) ----

type MenuProduct struct {
	ID            int64          `json:"id"`
	Name          string         `json:"name"`
	PriceCents    int64          `json:"price_cents"`
	KitchenRouted bool           `json:"kitchen_routed"`
	Modifiers     []ModifierView `json:"modifiers"`
}

type MenuCategory struct {
	ID        int64         `json:"id"`
	Module    string        `json:"module"`
	Name      string        `json:"name"`
	SortOrder int32         `json:"sort_order"`
	Products  []MenuProduct `json:"products"`
}

// Menu returns active categories with their available products and active modifiers. It needs orders.manage, not
// catalogue.manage, so waiters and bartenders can read it without being able to change anything.
func (s *Service) Menu(ctx context.Context, actor *auth.Principal) ([]MenuCategory, error) {
	if actor == nil || !actor.Has("orders.manage") {
		return nil, ErrForbidden
	}
	q := repository.New(s.pool)
	cats, err := q.ListCategories(ctx)
	if err != nil {
		return nil, err
	}
	yes := pgtype.Bool{Bool: true, Valid: true}
	prods, err := q.ListProducts(ctx, repository.ListProductsParams{Available: yes})
	if err != nil {
		return nil, err
	}
	mods, err := q.ListModifiers(ctx)
	if err != nil {
		return nil, err
	}
	active := map[int64]ModifierView{}
	for _, m := range mods {
		if m.Active {
			active[m.ID] = modView(m)
		}
	}
	ids := make([]int64, 0, len(prods))
	for _, p := range prods {
		ids = append(ids, p.ID)
	}
	links, err := s.links(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	byCat := map[int64][]MenuProduct{}
	for _, p := range prods {
		mp := MenuProduct{ID: p.ID, Name: p.Name, PriceCents: p.PriceCents, KitchenRouted: p.KitchenRouted, Modifiers: []ModifierView{}}
		for _, mid := range links[p.ID] {
			if m, ok := active[mid]; ok {
				mp.Modifiers = append(mp.Modifiers, m)
			}
		}
		byCat[p.CategoryID] = append(byCat[p.CategoryID], mp)
	}
	out := make([]MenuCategory, 0, len(cats))
	for _, c := range cats {
		if !c.Active || len(byCat[c.ID]) == 0 {
			continue
		}
		out = append(out, MenuCategory{ID: c.ID, Module: c.Module, Name: c.Name, SortOrder: c.SortOrder, Products: byCat[c.ID]})
	}
	return out, nil
}
