package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	invservice "zuscar/backend/internal/inventory/service"
	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/platform/idempotency"
	"zuscar/backend/internal/purchasing/repository"
)

// Suppliers, purchases and receiving (ASSUMPTIONS.md S-81 to S-88). A purchase is written as a draft (the order) and
// received once. Receiving records what really arrived on each line and books it into stock as receipt movements, in
// one transaction with the status change.

const (
	maxPurchaseLines = 100
	maxUnitCost      = 10_000_000 // cents per entered unit (KES 100,000); keeps every total far inside BIGINT
	maxQty           = 1_000_000_000
)

type SupplierView struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Contact string `json:"contact"`
	Active  bool   `json:"active"`
}

type PurchaseLineView struct {
	ItemID          int64  `json:"item_id"`
	ItemName        string `json:"item_name"`
	BaseUnitCode    string `json:"base_unit_code"`
	EnteredQty      int64  `json:"entered_qty"` // what was ordered
	EnteredUnitID   int64  `json:"entered_unit_id"`
	EnteredUnitCode string `json:"entered_unit_code"`
	QtyBase         int64  `json:"qty_base"`
	UnitCostCents   int64  `json:"unit_cost_cents"`
	ReceivedQty     *int64 `json:"received_qty"` // what arrived; null until received
	ReceivedQtyBase *int64 `json:"received_qty_base"`
	ReceivedCost    *int64 `json:"received_unit_cost_cents"`
	LineCents       int64  `json:"line_cents"` // what arrived at the price paid, or the order until received
	ReceiptReversed bool   `json:"receipt_reversed"`
}

type PurchaseView struct {
	ID           int64              `json:"id"`
	PurchaseNo   int64              `json:"purchase_no"`
	SupplierID   int64              `json:"supplier_id"`
	SupplierName string             `json:"supplier_name"`
	Status       string             `json:"status"`
	InvoiceRef   string             `json:"invoice_ref,omitempty"`
	Note         string             `json:"note,omitempty"`
	LocationID   *int64             `json:"location_id"`
	LocationName string             `json:"location_name,omitempty"`
	CreatedAt    time.Time          `json:"created_at"`
	ReceivedAt   *time.Time         `json:"received_at"`
	LineCount    int64              `json:"line_count"`
	TotalCents   int64              `json:"total_cents"`   // what arrived at the price paid once received; the order before
	OrderedCents int64              `json:"ordered_cents"` // always what was written down
	Lines        []PurchaseLineView `json:"lines,omitempty"`
}

func (s *Service) ListSuppliers(ctx context.Context, actor *auth.Principal, onlyActive bool) ([]SupplierView, error) {
	if !canPurchase(actor) {
		return nil, ErrForbidden
	}
	rows, err := repository.New(s.pool).ListSuppliers(ctx, onlyActive)
	if err != nil {
		return nil, err
	}
	out := make([]SupplierView, 0, len(rows))
	for _, r := range rows {
		out = append(out, SupplierView{ID: r.ID, Name: r.Name, Contact: r.Contact, Active: r.Active})
	}
	return out, nil
}

func cleanSupplier(name, contact string) (string, pgtype.Text, error) {
	name, contact = strings.TrimSpace(name), strings.TrimSpace(contact)
	if runeLen(name) < 1 || runeLen(name) > 120 {
		return "", pgtype.Text{}, &ValidationError{Field: "name", Msg: "must be 1 to 120 characters"}
	}
	if runeLen(contact) > 200 {
		return "", pgtype.Text{}, &ValidationError{Field: "contact", Msg: "must be 200 characters or fewer"}
	}
	return name, pgtype.Text{String: contact, Valid: contact != ""}, nil
}

func (s *Service) CreateSupplier(ctx context.Context, actor *auth.Principal, name, contact string, m Meta) (SupplierView, error) {
	if !canPurchase(actor) {
		return SupplierView{}, ErrForbidden
	}
	name, c, err := cleanSupplier(name, contact)
	if err != nil {
		return SupplierView{}, err
	}
	var out SupplierView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		id, err := repository.New(tx).InsertSupplier(ctx, repository.InsertSupplierParams{Name: name, Contact: c})
		if isUnique(err, "suppliers_name_key") {
			return &ConflictError{Code: "duplicate_name", Field: "name", Msg: "A supplier with that name already exists."}
		}
		if err != nil {
			return err
		}
		out = SupplierView{ID: id, Name: name, Contact: c.String, Active: true}
		return s.record(ctx, tx, actor, m, "purchasing.supplier_created", "supplier", id, nil, map[string]any{"name": name})
	})
	return out, err
}

type SupplierPatch struct {
	Name    *string
	Contact *string
	Active  *bool
}

func (s *Service) UpdateSupplier(ctx context.Context, actor *auth.Principal, id int64, in SupplierPatch, m Meta) (SupplierView, error) {
	if !canPurchase(actor) {
		return SupplierView{}, ErrForbidden
	}
	var out SupplierView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := q.GetSupplier(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "supplier"}
		}
		if err != nil {
			return err
		}
		name, contact, active := cur.Name, cur.Contact, cur.Active
		if in.Name != nil {
			name = *in.Name
		}
		if in.Contact != nil {
			contact = *in.Contact
		}
		if in.Active != nil {
			active = *in.Active
		}
		n, c, err := cleanSupplier(name, contact)
		if err != nil {
			return err
		}
		err = q.UpdateSupplier(ctx, repository.UpdateSupplierParams{ID: id, Name: n, Contact: c, Active: active})
		if isUnique(err, "suppliers_name_key") {
			return &ConflictError{Code: "duplicate_name", Field: "name", Msg: "A supplier with that name already exists."}
		}
		if err != nil {
			return err
		}
		out = SupplierView{ID: id, Name: n, Contact: c.String, Active: active}
		return s.record(ctx, tx, actor, m, "purchasing.supplier_updated", "supplier", id,
			map[string]any{"name": cur.Name, "contact": cur.Contact, "active": cur.Active}, map[string]any{"name": n, "contact": c.String, "active": active})
	})
	return out, err
}

func tsPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func int8Ptr(v pgtype.Int8) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

func (s *Service) ListPurchases(ctx context.Context, actor *auth.Principal, status string, supplierID, beforeID int64, limit int) ([]PurchaseView, error) {
	if !canPurchase(actor) {
		return nil, ErrForbidden
	}
	rows, err := repository.New(s.pool).ListPurchases(ctx, repository.ListPurchasesParams{Status: status, SupplierID: supplierID, BeforeID: beforeID, RowLimit: clampLimit(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]PurchaseView, 0, len(rows))
	for _, r := range rows {
		out = append(out, PurchaseView{ID: r.ID, PurchaseNo: r.PurchaseNo, SupplierID: r.SupplierID, SupplierName: r.SupplierName, Status: r.Status, InvoiceRef: r.InvoiceRef, Note: r.Note,
			LocationID: int8Ptr(r.LocationID), LocationName: r.LocationName, CreatedAt: r.CreatedAt.Time, ReceivedAt: tsPtr(r.ReceivedAt), LineCount: r.LineCount,
			TotalCents: r.TotalCents, OrderedCents: r.OrderedCents})
	}
	return out, nil
}

func (s *Service) loadPurchase(ctx context.Context, q *repository.Queries, id int64, withReversed bool) (PurchaseView, error) {
	r, err := q.GetPurchase(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return PurchaseView{}, &NotFoundError{What: "purchase"}
	}
	if err != nil {
		return PurchaseView{}, err
	}
	v := PurchaseView{ID: r.ID, PurchaseNo: r.PurchaseNo, SupplierID: r.SupplierID, SupplierName: r.SupplierName, Status: r.Status, InvoiceRef: r.InvoiceRef, Note: r.Note,
		LocationID: int8Ptr(r.LocationID), LocationName: r.LocationName, CreatedAt: r.CreatedAt.Time, ReceivedAt: tsPtr(r.ReceivedAt), LineCount: r.LineCount,
		TotalCents: r.TotalCents, OrderedCents: r.OrderedCents}
	reversed := map[int64]bool{}
	if withReversed && r.Status == "received" {
		if reversed, err = s.stock.ReversedReceipts(ctx, id); err != nil {
			return PurchaseView{}, err
		}
	}
	lines, err := q.ListPurchaseItems(ctx, id)
	if err != nil {
		return PurchaseView{}, err
	}
	for _, l := range lines {
		v.Lines = append(v.Lines, PurchaseLineView{ItemID: l.ItemID, ItemName: l.ItemName, BaseUnitCode: l.BaseUnitCode, EnteredQty: l.EnteredQty, EnteredUnitID: l.EnteredUnitID,
			EnteredUnitCode: l.EnteredUnitCode, QtyBase: l.QtyBase, UnitCostCents: l.UnitCostCents, ReceivedQty: int8Ptr(l.ReceivedQty), ReceivedQtyBase: int8Ptr(l.ReceivedQtyBase),
			ReceivedCost: int8Ptr(l.ReceivedUnitCostCents), LineCents: l.LineCents, ReceiptReversed: reversed[l.ItemID]})
	}
	return v, nil
}

func (s *Service) GetPurchase(ctx context.Context, actor *auth.Principal, id int64) (PurchaseView, error) {
	if !canPurchase(actor) {
		return PurchaseView{}, ErrForbidden
	}
	return s.loadPurchase(ctx, repository.New(s.pool), id, true)
}

type PurchaseLineInput struct {
	ItemID        int64
	Qty           int64
	UnitID        *int64
	UnitCostCents int64
}

type PurchaseInput struct {
	SupplierID int64
	InvoiceRef string
	Note       string
	Lines      []PurchaseLineInput
}

func optText(v string, max int, field string) (pgtype.Text, error) {
	v = strings.TrimSpace(v)
	if runeLen(v) > max {
		return pgtype.Text{}, &ValidationError{Field: field, Msg: "is too long"}
	}
	return pgtype.Text{String: v, Valid: v != ""}, nil
}

// CreatePurchase writes a draft: what was ordered or is being delivered, at the price on the supplier's invoice.
// It changes no stock.
func (s *Service) CreatePurchase(ctx context.Context, actor *auth.Principal, in PurchaseInput, idem *idempotency.Key, m Meta) (PurchaseView, *idempotency.Stored, error) {
	if !canPurchase(actor) {
		return PurchaseView{}, nil, ErrForbidden
	}
	ref, err := optText(in.InvoiceRef, 60, "invoice_ref")
	if err != nil {
		return PurchaseView{}, nil, err
	}
	note, err := optText(in.Note, 200, "note")
	if err != nil {
		return PurchaseView{}, nil, err
	}
	if len(in.Lines) < 1 || len(in.Lines) > maxPurchaseLines {
		return PurchaseView{}, nil, &ValidationError{Field: "lines", Msg: "needs 1 to 100 lines"}
	}
	seen := map[int64]bool{}
	for _, l := range in.Lines {
		if seen[l.ItemID] {
			return PurchaseView{}, nil, &ValidationError{Field: "lines", Msg: "lists the same item twice; combine them into one line"}
		}
		seen[l.ItemID] = true
		if l.UnitCostCents < 0 || l.UnitCostCents > maxUnitCost {
			return PurchaseView{}, nil, &ValidationError{Field: "unit_cost_cents", Msg: "must be from 0 to 10,000,000 cents"}
		}
	}
	var out PurchaseView
	var replay *idempotency.Stored
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if idem != nil {
			st, err := idempotency.Begin(ctx, tx, idem)
			if err != nil {
				return err
			}
			if st != nil {
				replay = st
				return nil
			}
		}
		q := repository.New(tx)
		sup, err := q.GetSupplier(ctx, in.SupplierID)
		if errors.Is(err, pgx.ErrNoRows) {
			return &ValidationError{Field: "supplier_id", Msg: "does not exist"}
		}
		if err != nil {
			return err
		}
		if !sup.Active {
			return &ConflictError{Code: "supplier_inactive", Field: "supplier_id", Msg: sup.Name + " is switched off. Switch it on to buy from them."}
		}
		no, err := q.NextCounterValue(ctx, "purchase")
		if err != nil {
			return err
		}
		pid, err := q.InsertPurchase(ctx, repository.InsertPurchaseParams{PurchaseNo: no, SupplierID: sup.ID, InvoiceRef: ref, Note: note, CreatedBy: actor.UserID})
		if err != nil {
			return err
		}
		for _, l := range in.Lines {
			rl, err := s.stock.ResolveLine(ctx, tx, invservice.LineRequest{ItemID: l.ItemID, Qty: l.Qty, UnitID: l.UnitID})
			if err != nil {
				return translate(err)
			}
			if err := q.InsertPurchaseItem(ctx, repository.InsertPurchaseItemParams{PurchaseID: pid, ItemID: rl.ItemID, EnteredQty: rl.EnteredQty, EnteredUnitID: rl.EnteredUnitID, QtyBase: rl.QtyBase, UnitCostCents: l.UnitCostCents}); err != nil {
				return err
			}
		}
		if err := s.record(ctx, tx, actor, m, "purchasing.purchase_created", "purchase", pid, nil, map[string]any{"purchase_no": no, "supplier_id": sup.ID, "lines": len(in.Lines)}); err != nil {
			return err
		}
		out, err = s.loadPurchase(ctx, q, pid, false)
		if err != nil {
			return err
		}
		if idem != nil {
			b, err := json.Marshal(map[string]any{"purchase": out})
			if err != nil {
				return err
			}
			return idempotency.Complete(ctx, tx, idem, 201, b)
		}
		return nil
	})
	return out, replay, err
}

// ReceivedLine says what really arrived for one line of the purchase. Qty is in the unit the line was written in; zero
// means it did not arrive. A nil UnitCostCents keeps the price written on the order.
type ReceivedLine struct {
	ItemID        int64
	Qty           int64
	UnitCostCents *int64
}

type ReceiveInput struct {
	LocationID int64
	InvoiceRef string
	Lines      []ReceivedLine // empty means everything arrived as ordered
}

// ReceivePurchase records what arrived on each line and books it into a place as receipts, once (S-82, S-85). Whoever
// receives it, the quantities and the prices paid are on the record. A purchase already received or cancelled answers
// 409 purchase_not_open.
func (s *Service) ReceivePurchase(ctx context.Context, actor *auth.Principal, id int64, in ReceiveInput, idem *idempotency.Key, m Meta) (PurchaseView, *idempotency.Stored, error) {
	if !canPurchase(actor) {
		return PurchaseView{}, nil, ErrForbidden
	}
	ref, err := optText(in.InvoiceRef, 60, "invoice_ref")
	if err != nil {
		return PurchaseView{}, nil, err
	}
	given := map[int64]ReceivedLine{}
	for _, l := range in.Lines {
		if _, dup := given[l.ItemID]; dup {
			return PurchaseView{}, nil, &ValidationError{Field: "lines", Msg: "lists the same item twice"}
		}
		if l.Qty < 0 || l.Qty > maxQty {
			return PurchaseView{}, nil, &ValidationError{Field: "qty", Msg: "must be a whole number from 0 to 1,000,000,000"}
		}
		if l.UnitCostCents != nil && (*l.UnitCostCents < 0 || *l.UnitCostCents > maxUnitCost) {
			return PurchaseView{}, nil, &ValidationError{Field: "unit_cost_cents", Msg: "must be from 0 to 10,000,000 cents"}
		}
		given[l.ItemID] = l
	}
	var out PurchaseView
	var replay *idempotency.Stored
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if idem != nil {
			st, err := idempotency.Begin(ctx, tx, idem)
			if err != nil {
				return err
			}
			if st != nil {
				replay = st
				return nil
			}
		}
		q := repository.New(tx)
		if err := s.stock.CheckLocation(ctx, tx, in.LocationID); err != nil {
			return translate(err)
		}
		p, err := q.LockPurchase(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "purchase"}
		}
		if err != nil {
			return err
		}
		if p.Status != "draft" {
			return &ConflictError{Code: "purchase_not_open", Msg: "This purchase is already " + p.Status + "."}
		}
		ordered, err := q.ListPurchaseItems(ctx, id)
		if err != nil {
			return err
		}
		onOrder := map[int64]bool{}
		for _, o := range ordered {
			onOrder[o.ItemID] = true
		}
		for item := range given {
			if !onOrder[item] {
				return &ValidationError{Field: "item_id", Msg: "is not on this delivery"}
			}
		}
		var receipts []invservice.ReceiptLine
		var arrivedCents, orderedCents int64
		for _, o := range ordered {
			qty, cost := o.EnteredQty, o.UnitCostCents
			if g, ok := given[o.ItemID]; ok {
				qty = g.Qty
				if g.UnitCostCents != nil {
					cost = *g.UnitCostCents
				}
			}
			orderedCents += o.EnteredQty * o.UnitCostCents
			var base int64
			if qty > 0 {
				rl, err := s.stock.ResolveLine(ctx, tx, invservice.LineRequest{ItemID: o.ItemID, Qty: qty, UnitID: &o.EnteredUnitID})
				if err != nil {
					return translate(err)
				}
				base = rl.QtyBase
				receipts = append(receipts, invservice.ReceiptLine{ItemID: o.ItemID, EnteredQty: qty, EnteredUnitID: o.EnteredUnitID, QtyBase: base, UnitCostCents: cost})
			}
			arrivedCents += qty * cost
			if err := q.RecordPurchaseItemReceived(ctx, repository.RecordPurchaseItemReceivedParams{PurchaseID: id, ItemID: o.ItemID,
				ReceivedQty: pgtype.Int8{Int64: qty, Valid: true}, ReceivedQtyBase: pgtype.Int8{Int64: base, Valid: true}, ReceivedUnitCostCents: pgtype.Int8{Int64: cost, Valid: true}}); err != nil {
				return err
			}
		}
		if len(receipts) == 0 {
			return &ValidationError{Field: "lines", Msg: "nothing arrived. Cancel the delivery instead"}
		}
		if err := s.stock.Receive(ctx, tx, actor, id, in.LocationID, receipts); err != nil {
			return translate(err)
		}
		if err := q.MarkPurchaseReceived(ctx, repository.MarkPurchaseReceivedParams{ID: id, LocationID: pgtype.Int8{Int64: in.LocationID, Valid: true}, ReceivedBy: pgtype.Int8{Int64: actor.UserID, Valid: true}, InvoiceRef: ref}); err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "purchasing.purchase_received", "purchase", id, map[string]any{"status": "draft", "ordered_cents": orderedCents},
			map[string]any{"status": "received", "location_id": in.LocationID, "lines": len(ordered), "arrived_lines": len(receipts), "arrived_cents": arrivedCents}); err != nil {
			return err
		}
		out, err = s.loadPurchase(ctx, q, id, false)
		if err != nil {
			return err
		}
		if idem != nil {
			b, err := json.Marshal(map[string]any{"purchase": out})
			if err != nil {
				return err
			}
			return idempotency.Complete(ctx, tx, idem, 200, b)
		}
		return nil
	})
	return out, replay, err
}

// CancelPurchase abandons a draft. Nothing was booked, so nothing needs undoing.
func (s *Service) CancelPurchase(ctx context.Context, actor *auth.Principal, id int64, m Meta) (PurchaseView, error) {
	if !canPurchase(actor) {
		return PurchaseView{}, ErrForbidden
	}
	var out PurchaseView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		p, err := q.LockPurchase(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "purchase"}
		}
		if err != nil {
			return err
		}
		if p.Status != "draft" {
			return &ConflictError{Code: "purchase_not_open", Msg: "This purchase is already " + p.Status + "."}
		}
		if err := q.MarkPurchaseCancelled(ctx, repository.MarkPurchaseCancelledParams{ID: id, CancelledBy: pgtype.Int8{Int64: actor.UserID, Valid: true}}); err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "purchasing.purchase_cancelled", "purchase", id, map[string]any{"status": "draft"}, map[string]any{"status": "cancelled"}); err != nil {
			return err
		}
		out, err = s.loadPurchase(ctx, q, id, false)
		return err
	})
	return out, err
}
