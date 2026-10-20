package service

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"zuscar/backend/internal/inventory/repository"
	"zuscar/backend/internal/platform/auth"
)

// What the purchasing module needs from stock: turn a typed quantity into base units, check a place, and book receipts
// inside the purchase's own transaction (docs/architecture.md section 3, rule 4). Stock rules stay in this module.

// LineRequest is what a person typed for one item: a quantity in the item's base unit, or in a unit it has a conversion for.
type LineRequest struct {
	ItemID int64
	Qty    int64
	UnitID *int64
}

// ResolvedLine is a LineRequest worked out: the item is real and switched on, and the quantity is in base units too.
type ResolvedLine struct {
	ItemID        int64
	ItemName      string
	EnteredQty    int64
	EnteredUnitID int64
	QtyBase       int64
}

func (s *Service) ResolveLine(ctx context.Context, tx pgx.Tx, in LineRequest) (ResolvedLine, error) {
	q := repository.New(tx)
	item, err := s.loadItem(ctx, q, in.ItemID)
	if err != nil {
		if _, ok := err.(*NotFoundError); ok {
			return ResolvedLine{}, &ValidationError{Field: "item_id", Msg: "refers to an item that does not exist"}
		}
		return ResolvedLine{}, err
	}
	if !item.Active {
		return ResolvedLine{}, &ConflictError{Code: "item_inactive", Msg: item.Name + " is switched off. Switch it on to buy it."}
	}
	base, unit, err := toBase(ctx, q, item, in.Qty, in.UnitID)
	if err != nil {
		return ResolvedLine{}, err
	}
	return ResolvedLine{ItemID: item.ID, ItemName: item.Name, EnteredQty: in.Qty, EnteredUnitID: unit, QtyBase: base}, nil
}

// CheckLocation says whether a place exists and is switched on.
func (s *Service) CheckLocation(ctx context.Context, tx pgx.Tx, id int64) error {
	if err := s.activeLocation(ctx, repository.New(tx), id); err != nil {
		return &ValidationError{Field: "location_id", Msg: "is not an active place"}
	}
	return nil
}

// ReceiptLine is stock that arrived on a purchase: base units into one place, with the price of one entered unit.
type ReceiptLine struct {
	ItemID        int64
	EnteredQty    int64
	EnteredUnitID int64
	QtyBase       int64
	UnitCostCents int64
}

// Receive books a purchase's lines into a place as receipt movements. The purchase module holds the purchase's row lock,
// so this runs once per purchase; the unique index on (purchase, item, place) is the second guard.
func (s *Service) Receive(ctx context.Context, tx pgx.Tx, actor *auth.Principal, purchaseID, locationID int64, lines []ReceiptLine) error {
	if len(lines) == 0 {
		return nil
	}
	sort.Slice(lines, func(a, b int) bool { return lines[a].ItemID < lines[b].ItemID })
	ids := make([]int64, 0, len(lines))
	for _, l := range lines {
		ids = append(ids, l.ItemID)
	}
	q := repository.New(tx)
	locked, err := q.LockItems(ctx, ids)
	if err != nil {
		return err
	}
	if len(locked) != len(ids) {
		return &ValidationError{Field: "item_id", Msg: "refers to an item that does not exist"}
	}
	for _, it := range locked {
		if !it.Active {
			return &ConflictError{Code: "item_inactive", Msg: it.Name + " is switched off. Switch it on to receive it."}
		}
	}
	for _, l := range lines {
		if _, err := q.InsertMovement(ctx, repository.InsertMovementParams{ItemID: l.ItemID, LocationID: locationID, QtyDelta: l.QtyBase, MovementType: "receipt",
			EnteredQty: pgtype.Int8{Int64: l.EnteredQty, Valid: true}, EnteredUnitID: pgtype.Int8{Int64: l.EnteredUnitID, Valid: true},
			UnitCostCents: pgtype.Int8{Int64: l.UnitCostCents, Valid: true}, RefType: pgtype.Text{String: "purchase", Valid: true}, RefID: pgtype.Int8{Int64: purchaseID, Valid: true},
			CreatedBy: actor.UserID}); err != nil {
			return err
		}
	}
	return nil
}

// ReversedReceipts lists the items of a purchase whose receipt has since been undone by a reversal.
func (s *Service) ReversedReceipts(ctx context.Context, purchaseID int64) (map[int64]bool, error) {
	ids, err := repository.New(s.pool).ReversedReceiptItems(ctx, pgtype.Int8{Int64: purchaseID, Valid: true})
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}
