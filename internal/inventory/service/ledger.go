package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"zuscar/backend/internal/inventory/repository"
	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/platform/idempotency"
)

// Writing to the ledger. Every write takes the item rows FOR UPDATE (in id order) before it reads a balance, so two
// changes to one item take turns and a "below zero" check cannot be fooled by a race.

func movementViews(rows []repository.ListMovementsByIDsRow) []MovementView {
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
	return out
}

// negativePolicy is warn_override (default: a permitted person may take stock below zero with a reason) or block (never).
// It is read before a transaction starts, never inside one: a second pooled connection taken while the first is held can starve the pool.
func (s *Service) negativePolicy(ctx context.Context, actor *auth.Principal) string {
	raw, err := s.settings.GetSettings(ctx, actor)
	if err != nil {
		return "warn_override"
	}
	var p string
	if v, ok := raw["negative_stock_policy"]; ok && json.Unmarshal(v, &p) == nil && p == "block" {
		return "block"
	}
	return "warn_override"
}

// toBase turns a typed quantity (in the item's base unit, or in a unit with a conversion) into base units. The
// result must be a whole number (S-03).
func toBase(ctx context.Context, q *repository.Queries, item ItemView, qty int64, unitID *int64) (base int64, enteredUnit int64, err error) {
	if qty < 1 || qty > maxQty {
		return 0, 0, &ValidationError{Field: "qty", Msg: "must be a whole number from 1 to 1,000,000,000"}
	}
	if unitID == nil || *unitID == item.BaseUnit.ID {
		return qty, item.BaseUnit.ID, nil
	}
	c, err := q.GetConversion(ctx, repository.GetConversionParams{ItemID: item.ID, FromUnitID: *unitID})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, &ValidationError{Field: "unit_id", Msg: "has no conversion to " + item.BaseUnit.Code + " for " + item.Name}
	}
	if err != nil {
		return 0, 0, err
	}
	n := qty * c.FactorNum
	if n%c.FactorDen != 0 {
		return 0, 0, &ValidationError{Field: "qty", Msg: "does not come to a whole number of " + item.BaseUnit.Code + "; use a smaller unit"}
	}
	return n / c.FactorDen, *unitID, nil
}

// guardNegative decides whether a change that would leave balance+delta below zero may go ahead.
func guardNegative(actor *auth.Principal, block bool, name string, balance, delta int64, override bool, overrideReason string) (pgtype.Text, error) {
	if balance+delta >= 0 {
		return pgtype.Text{}, nil
	}
	if block {
		return pgtype.Text{}, &ConflictError{Code: "insufficient_stock", Msg: name + " would go below zero (" + itoa(balance) + " in stock) and this business does not allow negative stock."}
	}
	if !override {
		return pgtype.Text{}, &ConflictError{Code: "negative_stock", Msg: name + " would go below zero (" + itoa(balance) + " in stock). A person allowed to override can confirm it with a reason."}
	}
	if !actor.Has("inventory.override_negative") {
		return pgtype.Text{}, &ForbiddenError{Code: "override_not_permitted", Msg: "Your role cannot take stock below zero."}
	}
	r := strings.TrimSpace(overrideReason)
	if runeLen(r) < 1 || runeLen(r) > 200 {
		return pgtype.Text{}, &ValidationError{Field: "override_reason", Msg: "is required, 1 to 200 characters, to take stock below zero"}
	}
	return pgtype.Text{String: r, Valid: true}, nil
}

func (s *Service) activeLocation(ctx context.Context, q *repository.Queries, id int64) error {
	l, err := q.GetLocation(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return &ValidationError{Field: "location_id", Msg: "does not exist"}
	}
	if err != nil {
		return err
	}
	if !l.Active {
		return &ValidationError{Field: "location_id", Msg: "is not active"}
	}
	return nil
}

type OpeningLine struct {
	ItemID int64
	Qty    int64
	UnitID *int64
}

type OpeningInput struct {
	LocationID int64
	Lines      []OpeningLine
	Note       string
}

// OpeningBalances sets the starting stock of items at a place, once per item and place (S-73). After that, stock moves
// by receipts, counts, adjustments, wastage and sales.
func (s *Service) OpeningBalances(ctx context.Context, actor *auth.Principal, in OpeningInput, idem *idempotency.Key, m Meta) ([]MovementView, *idempotency.Stored, error) {
	if !canAdjust(actor) {
		return nil, nil, ErrForbidden
	}
	if len(in.Lines) == 0 || len(in.Lines) > 200 {
		return nil, nil, &ValidationError{Field: "lines", Msg: "needs 1 to 200 lines"}
	}
	note := strings.TrimSpace(in.Note)
	if runeLen(note) > 200 {
		return nil, nil, &ValidationError{Field: "note", Msg: "must be 200 characters or fewer"}
	}
	seen := map[int64]bool{}
	ids := make([]int64, 0, len(in.Lines))
	for _, l := range in.Lines {
		if l.ItemID <= 0 || seen[l.ItemID] {
			return nil, nil, &ValidationError{Field: "lines", Msg: "needs a different item on every line"}
		}
		seen[l.ItemID] = true
		ids = append(ids, l.ItemID)
	}
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
		if err := s.activeLocation(ctx, q, in.LocationID); err != nil {
			return err
		}
		locked, err := q.LockItems(ctx, ids)
		if err != nil {
			return err
		}
		if len(locked) != len(ids) {
			return &ValidationError{Field: "lines", Msg: "refers to an item that does not exist"}
		}
		var made []int64
		for _, l := range in.Lines {
			item, err := s.loadItem(ctx, q, l.ItemID)
			if err != nil {
				return err
			}
			base, unit, err := toBase(ctx, q, item, l.Qty, l.UnitID)
			if err != nil {
				return err
			}
			reason := pgtype.Text{}
			if note != "" {
				reason = pgtype.Text{String: note, Valid: true}
			}
			id, err := q.InsertMovement(ctx, repository.InsertMovementParams{ItemID: item.ID, LocationID: in.LocationID, QtyDelta: base, MovementType: "opening_balance", Reason: reason,
				EnteredQty: pgtype.Int8{Int64: l.Qty, Valid: true}, EnteredUnitID: pgtype.Int8{Int64: unit, Valid: true}, CreatedBy: actor.UserID})
			if isUnique(err, "inventory_movements_one_opening") {
				return &ConflictError{Code: "opening_exists", Field: "lines", Msg: item.Name + " already has an opening balance here. Use an adjustment or a stock count to change it."}
			}
			if err != nil {
				return err
			}
			made = append(made, id)
			if err := s.record(ctx, tx, actor, m, "inventory.opening_balance", "inventory_movement", id, nil, map[string]any{"item_id": item.ID, "location_id": in.LocationID, "qty_base": base}); err != nil {
				return err
			}
		}
		rows, err := q.ListMovementsByIDs(ctx, made)
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

type AdjustInput struct {
	ItemID, LocationID int64
	Qty                int64 // positive adds, negative takes away, in the unit given
	UnitID             *int64
	Reason             string
	OverrideNegative   bool
	OverrideReason     string
}

// Adjust adds or removes stock with a reason (a correction found outside a count). Taking stock below zero is a
// separate permission with its own reason (A-06).
func (s *Service) Adjust(ctx context.Context, actor *auth.Principal, in AdjustInput, idem *idempotency.Key, m Meta) (MovementView, *idempotency.Stored, error) {
	if !canAdjust(actor) {
		return MovementView{}, nil, ErrForbidden
	}
	reason := strings.TrimSpace(in.Reason)
	if runeLen(reason) < 1 || runeLen(reason) > 200 {
		return MovementView{}, nil, &ValidationError{Field: "reason", Msg: "is required, 1 to 200 characters"}
	}
	if in.Qty == 0 {
		return MovementView{}, nil, &ValidationError{Field: "qty", Msg: "cannot be 0"}
	}
	mag := in.Qty
	if mag < 0 {
		mag = -mag
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
			return &ConflictError{Code: "item_inactive", Msg: item.Name + " is switched off. Switch it on to adjust its stock."}
		}
		base, unit, err := toBase(ctx, q, item, mag, in.UnitID)
		if err != nil {
			return err
		}
		delta := base
		if in.Qty < 0 {
			delta = -base
		}
		bal, err := q.BalanceOf(ctx, repository.BalanceOfParams{ItemID: item.ID, LocationID: in.LocationID})
		if err != nil {
			return err
		}
		ov, err := guardNegative(actor, block, item.Name, bal, delta, in.OverrideNegative, in.OverrideReason)
		if err != nil {
			return err
		}
		id, err := q.InsertMovement(ctx, repository.InsertMovementParams{ItemID: item.ID, LocationID: in.LocationID, QtyDelta: delta, MovementType: "adjustment",
			Reason: pgtype.Text{String: reason, Valid: true}, EnteredQty: pgtype.Int8{Int64: in.Qty, Valid: true}, EnteredUnitID: pgtype.Int8{Int64: unit, Valid: true},
			OverrideReason: ov, CreatedBy: actor.UserID})
		if err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "inventory.adjusted", "inventory_movement", id, map[string]any{"balance_base": bal},
			map[string]any{"item_id": item.ID, "location_id": in.LocationID, "qty_delta": delta, "reason": reason, "override": ov.Valid}); err != nil {
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

type ReverseInput struct {
	MovementID       int64
	Reason           string
	OverrideNegative bool
	OverrideReason   string
}

// Reverse undoes one earlier movement with an exact opposite movement that points at it (AC-06). Sales, transfers
// and earlier reversals are not undone this way: a sale is corrected by a void or refund, a transfer by its pair.
func (s *Service) Reverse(ctx context.Context, actor *auth.Principal, in ReverseInput, idem *idempotency.Key, m Meta) (MovementView, *idempotency.Stored, error) {
	if !canAdjust(actor) {
		return MovementView{}, nil, ErrForbidden
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
		orig, err := q.GetMovement(ctx, in.MovementID)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "movement"}
		}
		if err != nil {
			return err
		}
		switch orig.MovementType {
		case "reversal", "transfer_in", "transfer_out", "sale_consumption", "sale_return", "recipe_consumption":
			return &ConflictError{Code: "not_reversible", Msg: "This kind of movement is not undone here: a sale is corrected by a void or refund, a transfer by a new transfer."}
		}
		if _, err := q.LockItems(ctx, []int64{orig.ItemID}); err != nil {
			return err
		}
		done, err := q.MovementReversed(ctx, pgtype.Int8{Int64: orig.ID, Valid: true})
		if err != nil {
			return err
		}
		if done {
			return &ConflictError{Code: "already_reversed", Msg: "This movement was already reversed."}
		}
		item, err := s.loadItem(ctx, q, orig.ItemID)
		if err != nil {
			return err
		}
		delta := -orig.QtyDelta
		bal, err := q.BalanceOf(ctx, repository.BalanceOfParams{ItemID: orig.ItemID, LocationID: orig.LocationID})
		if err != nil {
			return err
		}
		ov, err := guardNegative(actor, block, item.Name, bal, delta, in.OverrideNegative, in.OverrideReason)
		if err != nil {
			return err
		}
		id, err := q.InsertMovement(ctx, repository.InsertMovementParams{ItemID: orig.ItemID, LocationID: orig.LocationID, QtyDelta: delta, MovementType: "reversal",
			Reason: pgtype.Text{String: reason, Valid: true}, ReversesMovementID: pgtype.Int8{Int64: orig.ID, Valid: true}, OverrideReason: ov, CreatedBy: actor.UserID})
		if err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "inventory.reversed", "inventory_movement", id, nil,
			map[string]any{"reverses": orig.ID, "item_id": orig.ItemID, "location_id": orig.LocationID, "qty_delta": delta, "reason": reason, "override": ov.Valid}); err != nil {
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
