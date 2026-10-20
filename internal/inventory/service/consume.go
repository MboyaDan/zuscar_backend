package service

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"zuscar/backend/internal/inventory/repository"
	"zuscar/backend/internal/platform/auth"
)

// StockLine is stock a sale line took (or gives back): base units of one item at one place.
type StockLine struct {
	ItemID     int64
	LocationID int64
	Units      int64 // positive, base units
}

// merge adds up lines for the same item and place, in a stable order (item id, then place), so locks are taken in id order.
func merge(lines []StockLine) []StockLine {
	type k struct{ i, l int64 }
	sum := map[k]int64{}
	for _, l := range lines {
		sum[k{l.ItemID, l.LocationID}] += l.Units
	}
	out := make([]StockLine, 0, len(sum))
	for key, u := range sum {
		out = append(out, StockLine{ItemID: key.i, LocationID: key.l, Units: u})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].ItemID != out[b].ItemID {
			return out[a].ItemID < out[b].ItemID
		}
		return out[a].LocationID < out[b].LocationID
	})
	return out
}

func lockAll(ctx context.Context, q *repository.Queries, lines []StockLine) error {
	seen := map[int64]bool{}
	ids := []int64{}
	for _, l := range lines {
		if !seen[l.ItemID] {
			seen[l.ItemID] = true
			ids = append(ids, l.ItemID)
		}
	}
	got, err := q.LockItems(ctx, ids)
	if err != nil {
		return err
	}
	if len(got) != len(ids) {
		return &ValidationError{Field: "inventory_item_id", Msg: "refers to an item that does not exist"}
	}
	return nil
}

// Consume takes stock for a sale inside the sale's own transaction (S-72, S-74). Each item and place is taken once per
// sale, so a retry cannot deduct twice (the unique index also guarantees it). A sale is never held up by an empty shelf
// under the default policy: the movement is recorded with an automatic reason and shows in the below-zero list. Under
// the "block" policy (read by the caller before its transaction starts) the sale is refused instead.
func (s *Service) Consume(ctx context.Context, tx pgx.Tx, actor *auth.Principal, saleID int64, lines []StockLine, block bool) error {
	lines = merge(lines)
	if len(lines) == 0 {
		return nil
	}
	q := repository.New(tx)
	if err := lockAll(ctx, q, lines); err != nil {
		return err
	}
	for _, l := range lines {
		if l.Units <= 0 {
			return &ValidationError{Field: "stock_units_per_sale", Msg: "must be greater than 0"}
		}
		bal, err := q.BalanceOf(ctx, repository.BalanceOfParams{ItemID: l.ItemID, LocationID: l.LocationID})
		if err != nil {
			return err
		}
		ov := pgtype.Text{}
		if bal-l.Units < 0 {
			item, err := s.loadItem(ctx, q, l.ItemID)
			if err != nil {
				return err
			}
			if block {
				return &ConflictError{Code: "insufficient_stock", Msg: item.Name + " is out of stock (" + itoa(bal) + " left); this sale needs " + itoa(l.Units) + "."}
			}
			ov = pgtype.Text{String: "sold with " + itoa(bal) + " in stock", Valid: true}
		}
		if _, err := q.InsertMovement(ctx, repository.InsertMovementParams{ItemID: l.ItemID, LocationID: l.LocationID, QtyDelta: -l.Units, MovementType: "sale_consumption",
			RefType: pgtype.Text{String: "sale", Valid: true}, RefID: pgtype.Int8{Int64: saleID, Valid: true}, OverrideReason: ov, CreatedBy: actor.UserID}); err != nil {
			return err
		}
	}
	return nil
}

// Return puts stock back for a void (ref_type sale... uses the refund's id) or a refund the person chose to restock.
func (s *Service) Return(ctx context.Context, tx pgx.Tx, actor *auth.Principal, refundID int64, lines []StockLine, reason string) error {
	lines = merge(lines)
	if len(lines) == 0 {
		return nil
	}
	q := repository.New(tx)
	if err := lockAll(ctx, q, lines); err != nil {
		return err
	}
	for _, l := range lines {
		if l.Units <= 0 {
			continue
		}
		if _, err := q.InsertMovement(ctx, repository.InsertMovementParams{ItemID: l.ItemID, LocationID: l.LocationID, QtyDelta: l.Units, MovementType: "sale_return",
			Reason: pgtype.Text{String: reason, Valid: reason != ""}, RefType: pgtype.Text{String: "refund", Valid: true}, RefID: pgtype.Int8{Int64: refundID, Valid: true}, CreatedBy: actor.UserID}); err != nil {
			return err
		}
	}
	return nil
}

// BlocksNegative says whether this business forbids stock below zero. Callers read it before they begin a transaction.
func (s *Service) BlocksNegative(ctx context.Context, actor *auth.Principal) bool {
	return s.negativePolicy(ctx, actor) == "block"
}
