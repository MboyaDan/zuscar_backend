package service

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"zuscar/backend/internal/inventory/repository"
	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/platform/idempotency"
)

type TransferInput struct {
	ItemID, FromLocationID, ToLocationID int64
	Qty                                  int64
	UnitID                               *int64
	Note                                 string
	OverrideNegative                     bool
	OverrideReason                       string
}

// Transfer moves stock from one place to another as two ledger rows that share a group: out of the source and into
// the destination, for the same amount, in one transaction (S-78). The source may not go below zero without the
// override, the same rule as an adjustment.
func (s *Service) Transfer(ctx context.Context, actor *auth.Principal, in TransferInput, idem *idempotency.Key, m Meta) ([]MovementView, *idempotency.Stored, error) {
	if actor == nil || !actor.Has("inventory.transfer") {
		return nil, nil, ErrForbidden
	}
	note := strings.TrimSpace(in.Note)
	if runeLen(note) > 200 {
		return nil, nil, &ValidationError{Field: "note", Msg: "must be 200 characters or fewer"}
	}
	if in.FromLocationID == in.ToLocationID {
		return nil, nil, &ValidationError{Field: "to_location_id", Msg: "must be a different place from where the stock leaves"}
	}
	block := s.negativePolicy(ctx, actor) == "block"
	var out []MovementView
	var replay *idempotency.Stored
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
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
		if err := s.activeLocation(ctx, q, in.FromLocationID); err != nil {
			return &ValidationError{Field: "from_location_id", Msg: "is not an active place"}
		}
		if err := s.activeLocation(ctx, q, in.ToLocationID); err != nil {
			return &ValidationError{Field: "to_location_id", Msg: "is not an active place"}
		}
		if l, err := q.LockItems(ctx, []int64{in.ItemID}); err != nil {
			return err
		} else if len(l) == 0 {
			return &ValidationError{Field: "item_id", Msg: "does not exist"}
		}
		item, err := s.loadItem(ctx, q, in.ItemID)
		if err != nil {
			return err
		}
		if !item.Active {
			return &ConflictError{Code: "item_inactive", Msg: item.Name + " is switched off. Switch it on to move its stock."}
		}
		base, unit, err := toBase(ctx, q, item, in.Qty, in.UnitID)
		if err != nil {
			return err
		}
		bal, err := q.BalanceOf(ctx, repository.BalanceOfParams{ItemID: item.ID, LocationID: in.FromLocationID})
		if err != nil {
			return err
		}
		ov, err := guardNegative(actor, block, item.Name, bal, -base, in.OverrideNegative, in.OverrideReason)
		if err != nil {
			return err
		}
		gid, err := q.NewTransferGroup(ctx)
		if err != nil {
			return err
		}
		reason := pgtype.Text{String: note, Valid: note != ""}
		entered := pgtype.Int8{Int64: in.Qty, Valid: true}
		eu := pgtype.Int8{Int64: unit, Valid: true}
		outID, err := q.InsertMovement(ctx, repository.InsertMovementParams{ItemID: item.ID, LocationID: in.FromLocationID, QtyDelta: -base, MovementType: "transfer_out", Reason: reason,
			EnteredQty: entered, EnteredUnitID: eu, OverrideReason: ov, CreatedBy: actor.UserID, TransferGroup: gid})
		if err != nil {
			return err
		}
		inID, err := q.InsertMovement(ctx, repository.InsertMovementParams{ItemID: item.ID, LocationID: in.ToLocationID, QtyDelta: base, MovementType: "transfer_in", Reason: reason,
			EnteredQty: entered, EnteredUnitID: eu, CreatedBy: actor.UserID, TransferGroup: gid})
		if err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "inventory.transferred", "inventory_movement", outID, map[string]any{"from_balance_base": bal},
			map[string]any{"item_id": item.ID, "from_location_id": in.FromLocationID, "to_location_id": in.ToLocationID, "qty_base": base, "in_movement_id": inID, "override": ov.Valid}); err != nil {
			return err
		}
		rows, err := q.ListMovementsByIDs(ctx, []int64{outID, inID})
		if err != nil {
			return err
		}
		out = movementViews(rows)
		if idem != nil {
			b, err := json.Marshal(map[string]any{"movements": out})
			if err != nil {
				return err
			}
			return idempotency.Complete(ctx, tx, idem, 201, b)
		}
		return nil
	})
	return out, replay, err
}

type WastageInput struct {
	ItemID, LocationID int64
	Kind               string // wastage, breakage or complimentary
	Qty                int64
	UnitID             *int64
	Reason             string
	OverrideNegative   bool
	OverrideReason     string
}

// Wastage takes stock out for a reason that is not a sale: spoiled (wastage), dropped or broken (breakage), or given
// away (complimentary). Always a reduction, always with a reason (S-79).
func (s *Service) Wastage(ctx context.Context, actor *auth.Principal, in WastageInput, idem *idempotency.Key, m Meta) (MovementView, *idempotency.Stored, error) {
	if actor == nil || !actor.Has("inventory.wastage") {
		return MovementView{}, nil, ErrForbidden
	}
	switch in.Kind {
	case "wastage", "breakage", "complimentary":
	default:
		return MovementView{}, nil, &ValidationError{Field: "kind", Msg: "must be wastage, breakage or complimentary"}
	}
	reason := strings.TrimSpace(in.Reason)
	if runeLen(reason) < 1 || runeLen(reason) > 200 {
		return MovementView{}, nil, &ValidationError{Field: "reason", Msg: "is required, 1 to 200 characters"}
	}
	block := s.negativePolicy(ctx, actor) == "block"
	var out MovementView
	var replay *idempotency.Stored
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
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
		if err := s.activeLocation(ctx, q, in.LocationID); err != nil {
			return err
		}
		if l, err := q.LockItems(ctx, []int64{in.ItemID}); err != nil {
			return err
		} else if len(l) == 0 {
			return &ValidationError{Field: "item_id", Msg: "does not exist"}
		}
		item, err := s.loadItem(ctx, q, in.ItemID)
		if err != nil {
			return err
		}
		if !item.Active {
			return &ConflictError{Code: "item_inactive", Msg: item.Name + " is switched off. Switch it on to record wastage."}
		}
		base, unit, err := toBase(ctx, q, item, in.Qty, in.UnitID)
		if err != nil {
			return err
		}
		bal, err := q.BalanceOf(ctx, repository.BalanceOfParams{ItemID: item.ID, LocationID: in.LocationID})
		if err != nil {
			return err
		}
		ov, err := guardNegative(actor, block, item.Name, bal, -base, in.OverrideNegative, in.OverrideReason)
		if err != nil {
			return err
		}
		id, err := q.InsertMovement(ctx, repository.InsertMovementParams{ItemID: item.ID, LocationID: in.LocationID, QtyDelta: -base, MovementType: in.Kind,
			Reason: pgtype.Text{String: reason, Valid: true}, EnteredQty: pgtype.Int8{Int64: in.Qty, Valid: true}, EnteredUnitID: pgtype.Int8{Int64: unit, Valid: true},
			OverrideReason: ov, CreatedBy: actor.UserID})
		if err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "inventory."+in.Kind, "inventory_movement", id, map[string]any{"balance_base": bal},
			map[string]any{"item_id": item.ID, "location_id": in.LocationID, "qty_base": base, "reason": reason, "override": ov.Valid}); err != nil {
			return err
		}
		rows, err := q.ListMovementsByIDs(ctx, []int64{id})
		if err != nil {
			return err
		}
		out = movementViews(rows)[0]
		if idem != nil {
			b, err := json.Marshal(map[string]any{"movement": out})
			if err != nil {
				return err
			}
			return idempotency.Complete(ctx, tx, idem, 201, b)
		}
		return nil
	})
	return out, replay, err
}
