package service

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"zuscar/backend/internal/inventory/repository"
	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/platform/idempotency"
)

// Suppliers, purchases and receiving (Stage 19; ASSUMPTIONS.md S-81 to S-84). A purchase is written as a draft and
// received once. Receiving adds one 'receipt' movement per line, in one transaction with the status change.

const (
	maxPurchaseLines = 100
	maxUnitCost      = 10_000_000 // cents per entered unit (KES 100,000); keeps every total far inside BIGINT
)

func canPurchase(a *auth.Principal) bool { return a != nil && a.Has("purchasing.manage") }

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
	EnteredQty      int64  `json:"entered_qty"`
	EnteredUnitID   int64  `json:"entered_unit_id"`
	EnteredUnitCode string `json:"entered_unit_code"`
	QtyBase         int64  `json:"qty_base"`
	UnitCostCents   int64  `json:"unit_cost_cents"`
	LineCents       int64  `json:"line_cents"`
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
	TotalCents   int64              `json:"total_cents"`
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
			LocationID: int8Ptr(r.LocationID), LocationName: r.LocationName, CreatedAt: r.CreatedAt.Time, ReceivedAt: tsPtr(r.ReceivedAt), LineCount: r.LineCount, TotalCents: r.TotalCents})
	}
	return out, nil
}

func (s *Service) loadPurchase(ctx context.Context, q *repository.Queries, id int64) (PurchaseView, error) {
	r, err := q.GetPurchase(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return PurchaseView{}, &NotFoundError{What: "purchase"}
	}
	if err != nil {
		return PurchaseView{}, err
	}
	v := PurchaseView{ID: r.ID, PurchaseNo: r.PurchaseNo, SupplierID: r.SupplierID, SupplierName: r.SupplierName, Status: r.Status, InvoiceRef: r.InvoiceRef, Note: r.Note,
		LocationID: int8Ptr(r.LocationID), LocationName: r.LocationName, CreatedAt: r.CreatedAt.Time, ReceivedAt: tsPtr(r.ReceivedAt), LineCount: r.LineCount, TotalCents: r.TotalCents}
	lines, err := q.ListPurchaseItems(ctx, id)
	if err != nil {
		return PurchaseView{}, err
	}
	for _, l := range lines {
		v.Lines = append(v.Lines, PurchaseLineView{ItemID: l.ItemID, ItemName: l.ItemName, BaseUnitCode: l.BaseUnitCode, EnteredQty: l.EnteredQty, EnteredUnitID: l.EnteredUnitID,
			EnteredUnitCode: l.EnteredUnitCode, QtyBase: l.QtyBase, UnitCostCents: l.UnitCostCents, LineCents: l.LineCents})
	}
	return v, nil
}

func (s *Service) GetPurchase(ctx context.Context, actor *auth.Principal, id int64) (PurchaseView, error) {
	if !canPurchase(actor) {
		return PurchaseView{}, ErrForbidden
	}
	return s.loadPurchase(ctx, repository.New(s.pool), id)
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
			item, err := s.loadItem(ctx, q, l.ItemID)
			var nf *NotFoundError
			if errors.As(err, &nf) {
				return &ValidationError{Field: "item_id", Msg: "refers to an item that does not exist"}
			}
			if err != nil {
				return err
			}
			if !item.Active {
				return &ConflictError{Code: "item_inactive", Msg: item.Name + " is switched off. Switch it on to buy it."}
			}
			base, unit, err := toBase(ctx, q, item, l.Qty, l.UnitID)
			if err != nil {
				return err
			}
			if err := q.InsertPurchaseItem(ctx, repository.InsertPurchaseItemParams{PurchaseID: pid, ItemID: item.ID, EnteredQty: l.Qty, EnteredUnitID: unit, QtyBase: base, UnitCostCents: l.UnitCostCents}); err != nil {
				return err
			}
		}
		if err := s.record(ctx, tx, actor, m, "purchasing.purchase_created", "purchase", pid, nil, map[string]any{"purchase_no": no, "supplier_id": sup.ID, "lines": len(in.Lines)}); err != nil {
			return err
		}
		out, err = s.loadPurchase(ctx, q, pid)
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

// ReceivePurchase books a draft's lines into a place as receipts, once. Whoever receives it is on the record, and so is
// the cost (S-82). A purchase already received or cancelled answers 409 purchase_not_open.
func (s *Service) ReceivePurchase(ctx context.Context, actor *auth.Principal, id, locationID int64, invoiceRef string, idem *idempotency.Key, m Meta) (PurchaseView, *idempotency.Stored, error) {
	if !canPurchase(actor) {
		return PurchaseView{}, nil, ErrForbidden
	}
	ref, err := optText(invoiceRef, 60, "invoice_ref")
	if err != nil {
		return PurchaseView{}, nil, err
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
		if err := s.activeLocation(ctx, q, locationID); err != nil {
			return &ValidationError{Field: "location_id", Msg: "is not an active place"}
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
		lines, err := q.ListPurchaseItems(ctx, id)
		if err != nil {
			return err
		}
		ids := make([]int64, 0, len(lines))
		for _, l := range lines {
			ids = append(ids, l.ItemID)
		}
		sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
		locked, err := q.LockItems(ctx, ids)
		if err != nil {
			return err
		}
		for _, it := range locked {
			if !it.Active {
				return &ConflictError{Code: "item_inactive", Msg: it.Name + " is switched off. Switch it on to receive it."}
			}
		}
		for _, l := range lines {
			if _, err := q.InsertMovement(ctx, repository.InsertMovementParams{ItemID: l.ItemID, LocationID: locationID, QtyDelta: l.QtyBase, MovementType: "receipt",
				EnteredQty: pgtype.Int8{Int64: l.EnteredQty, Valid: true}, EnteredUnitID: pgtype.Int8{Int64: l.EnteredUnitID, Valid: true},
				UnitCostCents: pgtype.Int8{Int64: l.UnitCostCents, Valid: true}, RefType: pgtype.Text{String: "purchase", Valid: true}, RefID: pgtype.Int8{Int64: id, Valid: true},
				CreatedBy: actor.UserID}); err != nil {
				return err
			}
		}
		if err := q.MarkPurchaseReceived(ctx, repository.MarkPurchaseReceivedParams{ID: id, LocationID: pgtype.Int8{Int64: locationID, Valid: true}, ReceivedBy: pgtype.Int8{Int64: actor.UserID, Valid: true}, InvoiceRef: ref}); err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "purchasing.purchase_received", "purchase", id, map[string]any{"status": "draft"}, map[string]any{"status": "received", "location_id": locationID, "lines": len(lines)}); err != nil {
			return err
		}
		out, err = s.loadPurchase(ctx, q, id)
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
		out, err = s.loadPurchase(ctx, q, id)
		return err
	})
	return out, err
}
