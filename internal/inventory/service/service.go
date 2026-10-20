// Package service is the inventory module's rules (Stage 17): units, items, conversions, locations and the stock ledger.
// Stock is an append-only ledger. A balance is the sum of an item's movements at a location and is never stored
// (ADR-07, S-71). Quantities are whole numbers in each item's base unit (S-03). A correction is a compensating
// movement, never an edit.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"zuscar/backend/internal/inventory/repository"
	"zuscar/backend/internal/platform/audit"
	"zuscar/backend/internal/platform/auth"
)

var ErrForbidden = errors.New("you do not have permission to do this")

type ForbiddenError struct{ Code, Msg string }

func (e *ForbiddenError) Error() string { return e.Msg }

type NotFoundError struct{ What string }

func (e *NotFoundError) Error() string { return e.What + " not found" }

type ValidationError struct{ Field, Msg string }

func (e *ValidationError) Error() string { return e.Field + " " + e.Msg }

type ConflictError struct{ Code, Field, Msg string }

func (e *ConflictError) Error() string { return e.Msg }

type Meta struct {
	RequestID string
	IP        *netip.Addr
}

type Settings interface {
	GetSettings(ctx context.Context, actor *auth.Principal) (map[string]json.RawMessage, error)
}

type Service struct {
	pool     *pgxpool.Pool
	rec      audit.Recorder
	settings Settings
}

func New(pool *pgxpool.Pool, rec audit.Recorder, settings Settings) *Service {
	return &Service{pool: pool, rec: rec, settings: settings}
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

func (s *Service) record(ctx context.Context, tx pgx.Tx, actor *auth.Principal, m Meta, action, entity string, id int64, before, after map[string]any) error {
	return s.rec.Record(ctx, tx, audit.Entry{ActorID: actor.UserID, ActorLabel: actor.Username, Action: action, EntityType: entity,
		EntityID: itoa(id), Before: before, After: after, RequestID: m.RequestID, IP: m.IP})
}

func runeLen(s string) int { return len([]rune(s)) }

func clampLimit(n int) int32 {
	if n <= 0 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return int32(n)
}

func canView(a *auth.Principal) bool   { return a != nil && a.Has("inventory.view") }
func canAdjust(a *auth.Principal) bool { return a != nil && a.Has("inventory.adjust") }

func isUnique(err error, constraint string) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == constraint
}

// ---- views ----

type UnitView struct {
	ID   int64  `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
}

type LocationView struct {
	ID     int64  `json:"id"`
	Code   string `json:"code"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
}

type ConversionView struct {
	FromUnitID   int64  `json:"from_unit_id"`
	FromUnitCode string `json:"from_unit_code"`
	FromUnitName string `json:"from_unit_name"`
	FactorNum    int64  `json:"factor_num"`
	FactorDen    int64  `json:"factor_den"`
}

type BalanceView struct {
	LocationID   int64  `json:"location_id"`
	LocationCode string `json:"location_code"`
	LocationName string `json:"location_name"`
	QtyBase      int64  `json:"qty_base"`
}

type ItemView struct {
	ID                int64            `json:"id"`
	Name              string           `json:"name"`
	SKU               string           `json:"sku,omitempty"`
	BaseUnit          UnitView         `json:"base_unit"`
	LowStockThreshold *int64           `json:"low_stock_threshold"`
	Active            bool             `json:"active"`
	TotalBase         int64            `json:"total_base"`
	Low               bool             `json:"low"` // the total is at or below the low-stock level
	Conversions       []ConversionView `json:"conversions"`
	Balances          []BalanceView    `json:"balances"`
}

type BalanceRow struct {
	ItemID         int64      `json:"item_id"`
	ItemName       string     `json:"item_name"`
	LocationID     int64      `json:"location_id"`
	LocationCode   string     `json:"location_code"`
	LocationName   string     `json:"location_name"`
	QtyBase        int64      `json:"qty_base"`
	BaseUnitCode   string     `json:"base_unit_code"`
	Movements      int64      `json:"movements"`
	LastMovementAt *time.Time `json:"last_movement_at"`
}

type MovementView struct {
	ID                 int64     `json:"id"`
	ItemID             int64     `json:"item_id"`
	ItemName           string    `json:"item_name"`
	BaseUnitCode       string    `json:"base_unit_code"`
	LocationID         int64     `json:"location_id"`
	LocationName       string    `json:"location_name"`
	QtyDelta           int64     `json:"qty_delta"`
	MovementType       string    `json:"movement_type"`
	Reason             string    `json:"reason,omitempty"`
	EnteredQty         *int64    `json:"entered_qty"`
	EnteredUnitCode    string    `json:"entered_unit_code,omitempty"`
	RefType            string    `json:"ref_type,omitempty"`
	RefID              *int64    `json:"ref_id"`
	ReversesMovementID *int64    `json:"reverses_movement_id"`
	Reversed           bool      `json:"reversed"`
	OverrideReason     string    `json:"override_reason,omitempty"`
	CreatedBy          int64     `json:"created_by"`
	CreatedByName      string    `json:"created_by_name"`
	CreatedAt          time.Time `json:"created_at"`
}

func optID(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

// ---- units and locations ----

var codePattern = regexp.MustCompile(`^[a-z0-9_]{1,16}$`)
var locCodePattern = regexp.MustCompile(`^[a-z0-9_]{1,24}$`)

func (s *Service) ListUnits(ctx context.Context, actor *auth.Principal) ([]UnitView, error) {
	if !canView(actor) {
		return nil, ErrForbidden
	}
	rows, err := repository.New(s.pool).ListUnits(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]UnitView, 0, len(rows))
	for _, r := range rows {
		out = append(out, UnitView{ID: r.ID, Code: r.Code, Name: r.Name})
	}
	return out, nil
}

func (s *Service) CreateUnit(ctx context.Context, actor *auth.Principal, code, name string, m Meta) (UnitView, error) {
	if !canAdjust(actor) {
		return UnitView{}, ErrForbidden
	}
	code = strings.ToLower(strings.TrimSpace(code))
	name = strings.TrimSpace(name)
	if !codePattern.MatchString(code) {
		return UnitView{}, &ValidationError{Field: "code", Msg: "must be 1 to 16 lower-case letters, digits or _"}
	}
	if runeLen(name) < 1 || runeLen(name) > 40 {
		return UnitView{}, &ValidationError{Field: "name", Msg: "must be 1 to 40 characters"}
	}
	var out UnitView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := repository.New(tx).InsertUnit(ctx, repository.InsertUnitParams{Code: code, Name: name})
		if isUnique(err, "units_of_measure_code_key") {
			return &ConflictError{Code: "duplicate_code", Field: "code", Msg: "A unit with that code already exists."}
		}
		if err != nil {
			return err
		}
		out = UnitView{ID: r.ID, Code: r.Code, Name: r.Name}
		return s.record(ctx, tx, actor, m, "inventory.unit_created", "unit", r.ID, nil, map[string]any{"code": code, "name": name})
	})
	return out, err
}

func (s *Service) ListLocations(ctx context.Context, actor *auth.Principal) ([]LocationView, error) {
	if !canView(actor) {
		return nil, ErrForbidden
	}
	rows, err := repository.New(s.pool).ListLocations(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]LocationView, 0, len(rows))
	for _, r := range rows {
		out = append(out, LocationView{ID: r.ID, Code: r.Code, Name: r.Name, Active: r.Active})
	}
	return out, nil
}

func (s *Service) CreateLocation(ctx context.Context, actor *auth.Principal, code, name string, m Meta) (LocationView, error) {
	if !canAdjust(actor) {
		return LocationView{}, ErrForbidden
	}
	code = strings.ToLower(strings.TrimSpace(code))
	name = strings.TrimSpace(name)
	if !locCodePattern.MatchString(code) {
		return LocationView{}, &ValidationError{Field: "code", Msg: "must be 1 to 24 lower-case letters, digits or _"}
	}
	if runeLen(name) < 1 || runeLen(name) > 60 {
		return LocationView{}, &ValidationError{Field: "name", Msg: "must be 1 to 60 characters"}
	}
	var out LocationView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := repository.New(tx).InsertLocation(ctx, repository.InsertLocationParams{Code: code, Name: name})
		if isUnique(err, "stock_locations_code_key") {
			return &ConflictError{Code: "duplicate_code", Field: "code", Msg: "A location with that code already exists."}
		}
		if err != nil {
			return err
		}
		out = LocationView{ID: r.ID, Code: r.Code, Name: r.Name, Active: r.Active}
		return s.record(ctx, tx, actor, m, "inventory.location_created", "location", r.ID, nil, map[string]any{"code": code, "name": name})
	})
	return out, err
}

// LocationIDForModule says where a sale takes stock from: bar products from the bar, restaurant products from the
// kitchen (S-72). The caller's transaction is used so the answer is consistent with the rest of the sale.
func (s *Service) LocationIDForModule(ctx context.Context, tx pgx.Tx, module string) (int64, error) {
	code := "kitchen"
	if module == "bar" {
		code = "bar"
	}
	l, err := repository.New(tx).GetLocationByCode(ctx, code)
	if err != nil {
		return 0, err
	}
	return l.ID, nil
}

// ---- items ----

type ItemFilter struct {
	Query      string
	OnlyActive bool
}

func (s *Service) fill(ctx context.Context, q *repository.Queries, items []ItemView) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]int64, len(items))
	pos := map[int64]int{}
	for i := range items {
		ids[i] = items[i].ID
		pos[items[i].ID] = i
		items[i].Conversions = []ConversionView{}
		items[i].Balances = []BalanceView{}
	}
	convs, err := q.ListConversions(ctx, ids)
	if err != nil {
		return err
	}
	for _, c := range convs {
		i := pos[c.ItemID]
		items[i].Conversions = append(items[i].Conversions, ConversionView{FromUnitID: c.FromUnitID, FromUnitCode: c.FromUnitCode, FromUnitName: c.FromUnitName, FactorNum: c.FactorNum, FactorDen: c.FactorDen})
	}
	bals, err := q.ListBalancesForItems(ctx, ids)
	if err != nil {
		return err
	}
	locs, err := q.ListLocations(ctx)
	if err != nil {
		return err
	}
	byLoc := map[int64]repository.ListLocationsRow{}
	for _, l := range locs {
		byLoc[l.ID] = l
	}
	for _, b := range bals {
		i := pos[b.ItemID]
		l := byLoc[b.LocationID]
		items[i].Balances = append(items[i].Balances, BalanceView{LocationID: b.LocationID, LocationCode: l.Code, LocationName: l.Name, QtyBase: b.QtyBase})
	}
	return nil
}

func itemFrom(id int64, name, sku string, unitID int64, unitCode, unitName string, thr pgtype.Int8, active bool, total int64) ItemView {
	v := ItemView{ID: id, Name: name, SKU: sku, BaseUnit: UnitView{ID: unitID, Code: unitCode, Name: unitName}, Active: active, TotalBase: total}
	if thr.Valid {
		t := thr.Int64
		v.LowStockThreshold = &t
		v.Low = total <= t
	}
	return v
}

func (s *Service) ListItems(ctx context.Context, actor *auth.Principal, f ItemFilter) ([]ItemView, error) {
	if !canView(actor) {
		return nil, ErrForbidden
	}
	q := repository.New(s.pool)
	rows, err := q.ListItems(ctx, repository.ListItemsParams{OnlyActive: f.OnlyActive, Q: strings.TrimSpace(f.Query)})
	if err != nil {
		return nil, err
	}
	out := make([]ItemView, 0, len(rows))
	for _, r := range rows {
		out = append(out, itemFrom(r.ID, r.Name, r.Sku, r.BaseUnitID, r.BaseUnitCode, r.BaseUnitName, r.LowStockThreshold, r.Active, r.TotalBase))
	}
	if err := s.fill(ctx, q, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) loadItem(ctx context.Context, q *repository.Queries, id int64) (ItemView, error) {
	r, err := q.GetItem(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ItemView{}, &NotFoundError{What: "item"}
	}
	if err != nil {
		return ItemView{}, err
	}
	out := []ItemView{itemFrom(r.ID, r.Name, r.Sku, r.BaseUnitID, r.BaseUnitCode, r.BaseUnitName, r.LowStockThreshold, r.Active, r.TotalBase)}
	if err := s.fill(ctx, q, out); err != nil {
		return ItemView{}, err
	}
	return out[0], nil
}

func (s *Service) GetItem(ctx context.Context, actor *auth.Principal, id int64) (ItemView, error) {
	if !canView(actor) {
		return ItemView{}, ErrForbidden
	}
	return s.loadItem(ctx, repository.New(s.pool), id)
}

const maxQty = 1_000_000_000

type NewItem struct {
	Name              string
	SKU               string
	BaseUnitID        int64
	LowStockThreshold *int64
}

func checkItemText(name, sku string) (string, pgtype.Text, error) {
	name = strings.TrimSpace(name)
	if runeLen(name) < 1 || runeLen(name) > 120 {
		return "", pgtype.Text{}, &ValidationError{Field: "name", Msg: "must be 1 to 120 characters"}
	}
	sku = strings.TrimSpace(sku)
	if runeLen(sku) > 40 {
		return "", pgtype.Text{}, &ValidationError{Field: "sku", Msg: "must be 40 characters or fewer"}
	}
	t := pgtype.Text{}
	if sku != "" {
		t = pgtype.Text{String: sku, Valid: true}
	}
	return name, t, nil
}

func checkThreshold(v *int64) (pgtype.Int8, error) {
	if v == nil {
		return pgtype.Int8{}, nil
	}
	if *v < 0 || *v > maxQty {
		return pgtype.Int8{}, &ValidationError{Field: "low_stock_threshold", Msg: "must be 0 or more"}
	}
	return pgtype.Int8{Int64: *v, Valid: true}, nil
}

func dupItem(err error) error {
	switch {
	case isUnique(err, "inventory_items_name_key"):
		return &ConflictError{Code: "duplicate_name", Field: "name", Msg: "An item with that name already exists."}
	case isUnique(err, "inventory_items_sku_key"):
		return &ConflictError{Code: "duplicate_sku", Field: "sku", Msg: "An item with that SKU already exists."}
	}
	return err
}

func (s *Service) CreateItem(ctx context.Context, actor *auth.Principal, in NewItem, m Meta) (ItemView, error) {
	if !canAdjust(actor) {
		return ItemView{}, ErrForbidden
	}
	name, sku, err := checkItemText(in.Name, in.SKU)
	if err != nil {
		return ItemView{}, err
	}
	thr, err := checkThreshold(in.LowStockThreshold)
	if err != nil {
		return ItemView{}, err
	}
	var out ItemView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		if _, err := q.GetUnit(ctx, in.BaseUnitID); errors.Is(err, pgx.ErrNoRows) {
			return &ValidationError{Field: "base_unit_id", Msg: "does not exist"}
		} else if err != nil {
			return err
		}
		id, err := q.InsertItem(ctx, repository.InsertItemParams{Name: name, Sku: sku, BaseUnitID: in.BaseUnitID, LowStockThreshold: thr})
		if err != nil {
			return dupItem(err)
		}
		if err := s.record(ctx, tx, actor, m, "inventory.item_created", "inventory_item", id, nil, map[string]any{"name": name, "base_unit_id": in.BaseUnitID}); err != nil {
			return err
		}
		out, err = s.loadItem(ctx, q, id)
		return err
	})
	return out, err
}

// NullableInt tells "not sent" from "sent as null" for the low-stock level.
type NullableInt struct {
	Set   bool
	Value *int64
}

type ItemChange struct {
	Name              *string
	SKU               *string // empty text clears it
	LowStockThreshold NullableInt
	Active            *bool
}

func (s *Service) UpdateItem(ctx context.Context, actor *auth.Principal, id int64, ch ItemChange, m Meta) (ItemView, error) {
	if !canAdjust(actor) {
		return ItemView{}, ErrForbidden
	}
	var out ItemView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		if _, err := q.LockItems(ctx, []int64{id}); err != nil {
			return err
		}
		cur, err := s.loadItem(ctx, q, id)
		if err != nil {
			return err
		}
		name, sku, active := cur.Name, cur.SKU, cur.Active
		thr := cur.LowStockThreshold
		if ch.Name != nil {
			name = *ch.Name
		}
		if ch.SKU != nil {
			sku = *ch.SKU
		}
		if ch.Active != nil {
			active = *ch.Active
		}
		if ch.LowStockThreshold.Set {
			thr = ch.LowStockThreshold.Value
		}
		name, skuT, err := checkItemText(name, sku)
		if err != nil {
			return err
		}
		thrT, err := checkThreshold(thr)
		if err != nil {
			return err
		}
		if err := q.UpdateItem(ctx, repository.UpdateItemParams{ID: id, Name: name, Sku: skuT, LowStockThreshold: thrT, Active: active}); err != nil {
			return dupItem(err)
		}
		out, err = s.loadItem(ctx, q, id)
		if err != nil {
			return err
		}
		return s.record(ctx, tx, actor, m, "inventory.item_updated", "inventory_item", id,
			map[string]any{"name": cur.Name, "sku": cur.SKU, "active": cur.Active, "low_stock_threshold": cur.LowStockThreshold},
			map[string]any{"name": out.Name, "sku": out.SKU, "active": out.Active, "low_stock_threshold": out.LowStockThreshold})
	})
	return out, err
}

type ConversionInput struct {
	UnitID    int64
	FactorNum int64
	FactorDen int64
}

// SetConversions replaces an item's conversions ("1 crate = 24 pieces"). A conversion ends in the item's base unit.
func (s *Service) SetConversions(ctx context.Context, actor *auth.Principal, id int64, in []ConversionInput, m Meta) (ItemView, error) {
	if !canAdjust(actor) {
		return ItemView{}, ErrForbidden
	}
	if len(in) > 20 {
		return ItemView{}, &ValidationError{Field: "conversions", Msg: "has more than 20 entries"}
	}
	seen := map[int64]bool{}
	for _, c := range in {
		if c.FactorNum < 1 || c.FactorNum > 1_000_000_000 || c.FactorDen < 1 || c.FactorDen > 1_000_000_000 {
			return ItemView{}, &ValidationError{Field: "conversions", Msg: "factors must be whole numbers from 1 to 1,000,000,000"}
		}
		if seen[c.UnitID] {
			return ItemView{}, &ValidationError{Field: "conversions", Msg: "lists the same unit twice"}
		}
		seen[c.UnitID] = true
	}
	var out ItemView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		if _, err := q.LockItems(ctx, []int64{id}); err != nil {
			return err
		}
		cur, err := s.loadItem(ctx, q, id)
		if err != nil {
			return err
		}
		for _, c := range in {
			if c.UnitID == cur.BaseUnit.ID {
				return &ValidationError{Field: "conversions", Msg: "cannot convert the base unit into itself"}
			}
			if _, err := q.GetUnit(ctx, c.UnitID); errors.Is(err, pgx.ErrNoRows) {
				return &ValidationError{Field: "conversions", Msg: "refers to a unit that does not exist"}
			} else if err != nil {
				return err
			}
		}
		if err := q.DeleteConversions(ctx, id); err != nil {
			return err
		}
		for _, c := range in {
			if err := q.InsertConversion(ctx, repository.InsertConversionParams{ItemID: id, FromUnitID: c.UnitID, ToUnitID: cur.BaseUnit.ID, FactorNum: c.FactorNum, FactorDen: c.FactorDen}); err != nil {
				return err
			}
		}
		out, err = s.loadItem(ctx, q, id)
		if err != nil {
			return err
		}
		return s.record(ctx, tx, actor, m, "inventory.conversions_set", "inventory_item", id, map[string]any{"conversions": len(cur.Conversions)}, map[string]any{"conversions": len(in)})
	})
	return out, err
}

// ---- reading the ledger ----

type BalanceFilter struct{ ItemID, LocationID int64 }

func (s *Service) Balances(ctx context.Context, actor *auth.Principal, f BalanceFilter) ([]BalanceRow, error) {
	if !canView(actor) {
		return nil, ErrForbidden
	}
	rows, err := repository.New(s.pool).ListBalances(ctx, repository.ListBalancesParams{ItemID: f.ItemID, LocationID: f.LocationID})
	if err != nil {
		return nil, err
	}
	out := make([]BalanceRow, 0, len(rows))
	for _, r := range rows {
		b := BalanceRow{ItemID: r.ItemID, ItemName: r.ItemName, LocationID: r.LocationID, LocationCode: r.LocationCode, LocationName: r.LocationName, QtyBase: r.QtyBase,
			BaseUnitCode: r.BaseUnitCode, Movements: r.Movements}
		if r.LastMovementAt.Valid {
			t := r.LastMovementAt.Time
			b.LastMovementAt = &t
		}
		out = append(out, b)
	}
	return out, nil
}

type MovementFilter struct {
	ItemID, LocationID int64
	Type               string
	BeforeID           int64
	Limit              int
}

func (s *Service) Movements(ctx context.Context, actor *auth.Principal, f MovementFilter) ([]MovementView, bool, error) {
	if !canView(actor) {
		return nil, false, ErrForbidden
	}
	limit := clampLimit(f.Limit)
	rows, err := repository.New(s.pool).ListMovements(ctx, repository.ListMovementsParams{ItemID: f.ItemID, LocationID: f.LocationID, MovementType: f.Type, BeforeID: f.BeforeID, RowLimit: limit + 1})
	if err != nil {
		return nil, false, err
	}
	more := int32(len(rows)) > limit
	if more {
		rows = rows[:limit]
	}
	out := make([]MovementView, 0, len(rows))
	for _, r := range rows {
		mv := MovementView{ID: r.ID, ItemID: r.ItemID, ItemName: r.ItemName, BaseUnitCode: r.BaseUnitCode, LocationID: r.LocationID, LocationName: r.LocationName, QtyDelta: r.QtyDelta,
			MovementType: r.MovementType, Reason: r.Reason, EnteredUnitCode: r.EnteredUnitCode, RefType: r.RefType, RefID: optID(r.RefID), ReversesMovementID: optID(r.ReversesMovementID),
			Reversed: r.Reversed, OverrideReason: r.OverrideReason, CreatedBy: r.CreatedBy, CreatedByName: r.CreatedByName, CreatedAt: r.CreatedAt.Time}
		if r.EnteredQty.Valid {
			e := r.EnteredQty.Int64
			mv.EnteredQty = &e
		}
		out = append(out, mv)
	}
	return out, more, nil
}
