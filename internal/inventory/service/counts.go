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

// Stock counts (Stage 21; ASSUMPTIONS.md S-89 to S-92). A count is a sheet for one place. Each line keeps what was
// counted and what the books said when the line was entered, so sales and deliveries during the count are kept.
// Approving posts each line's difference as a count_adjustment movement.

const maxCountLines = 500

type CountLineView struct {
	ItemID          int64     `json:"item_id"`
	ItemName        string    `json:"item_name"`
	BaseUnitCode    string    `json:"base_unit_code"`
	CountedBase     int64     `json:"counted_base"`
	EnteredQty      int64     `json:"entered_qty"`
	EnteredUnitID   int64     `json:"entered_unit_id"`
	EnteredUnitCode string    `json:"entered_unit_code"`
	ExpectedBase    int64     `json:"expected_base"`
	VarianceBase    int64     `json:"variance_base"` // counted minus expected; what approving will post
	BalanceNow      *int64    `json:"balance_now"`   // what the books say now (drafts only)
	SavedAt         time.Time `json:"saved_at"`
}

type CountView struct {
	ID              int64           `json:"id"`
	CountNo         int64           `json:"count_no"`
	LocationID      int64           `json:"location_id"`
	LocationName    string          `json:"location_name"`
	Status          string          `json:"status"`
	Note            string          `json:"note,omitempty"`
	CreatedBy       int64           `json:"created_by"`
	CreatedByName   string          `json:"created_by_name"`
	CreatedAt       time.Time       `json:"created_at"`
	ApprovedBy      int64           `json:"approved_by,omitempty"`
	ApprovedByName  string          `json:"approved_by_name,omitempty"`
	ApprovedAt      *time.Time      `json:"approved_at"`
	LineCount       int64           `json:"line_count"`
	DifferenceCount int64           `json:"difference_count"`
	Lines           []CountLineView `json:"lines,omitempty"`
}

func countFrom(id, no, loc int64, locName, status, note string, by int64, byName string, at pgtype.Timestamptz, apBy int64, apName string, apAt pgtype.Timestamptz, lines, diffs int64) CountView {
	v := CountView{ID: id, CountNo: no, LocationID: loc, LocationName: locName, Status: status, Note: note, CreatedBy: by, CreatedByName: byName, CreatedAt: at.Time,
		ApprovedBy: apBy, ApprovedByName: apName, LineCount: lines, DifferenceCount: diffs}
	if apAt.Valid {
		t := apAt.Time
		v.ApprovedAt = &t
	}
	return v
}

func (s *Service) loadCount(ctx context.Context, q *repository.Queries, id int64, withLines bool) (CountView, error) {
	r, err := q.GetCount(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return CountView{}, &NotFoundError{What: "count"}
	}
	if err != nil {
		return CountView{}, err
	}
	v := countFrom(r.ID, r.CountNo, r.LocationID, r.LocationName, r.Status, r.Note, r.CreatedBy, r.CreatedByName, r.CreatedAt, r.ApprovedBy, r.ApprovedByName, r.ApprovedAt, r.LineCount, r.DifferenceCount)
	if !withLines {
		return v, nil
	}
	lines, err := q.ListCountLines(ctx, id)
	if err != nil {
		return CountView{}, err
	}
	for _, l := range lines {
		lv := CountLineView{ItemID: l.ItemID, ItemName: l.ItemName, BaseUnitCode: l.BaseUnitCode, CountedBase: l.CountedBase, EnteredQty: l.EnteredQty, EnteredUnitID: l.EnteredUnitID,
			EnteredUnitCode: l.EnteredUnitCode, ExpectedBase: l.ExpectedBase, VarianceBase: l.CountedBase - l.ExpectedBase, SavedAt: l.SavedAt.Time}
		if r.Status == "draft" {
			bal, err := q.BalanceOf(ctx, repository.BalanceOfParams{ItemID: l.ItemID, LocationID: r.LocationID})
			if err != nil {
				return CountView{}, err
			}
			lv.BalanceNow = &bal
		}
		v.Lines = append(v.Lines, lv)
	}
	return v, nil
}

func (s *Service) GetCount(ctx context.Context, actor *auth.Principal, id int64) (CountView, error) {
	if !canAdjust(actor) {
		return CountView{}, ErrForbidden
	}
	return s.loadCount(ctx, repository.New(s.pool), id, true)
}

func (s *Service) ListCounts(ctx context.Context, actor *auth.Principal, status string, beforeID int64, limit int) ([]CountView, error) {
	if !canAdjust(actor) {
		return nil, ErrForbidden
	}
	rows, err := repository.New(s.pool).ListCounts(ctx, repository.ListCountsParams{Status: status, BeforeID: beforeID, RowLimit: clampLimit(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]CountView, 0, len(rows))
	for _, r := range rows {
		out = append(out, countFrom(r.ID, r.CountNo, r.LocationID, r.LocationName, r.Status, r.Note, r.CreatedBy, r.CreatedByName, r.CreatedAt, r.ApprovedBy, r.ApprovedByName, r.ApprovedAt, r.LineCount, r.DifferenceCount))
	}
	return out, nil
}

// CreateCount starts a sheet for one place. It changes no stock.
func (s *Service) CreateCount(ctx context.Context, actor *auth.Principal, locationID int64, note string, idem *idempotency.Key, m Meta) (CountView, *idempotency.Stored, error) {
	if !canAdjust(actor) {
		return CountView{}, nil, ErrForbidden
	}
	note = strings.TrimSpace(note)
	if runeLen(note) > 200 {
		return CountView{}, nil, &ValidationError{Field: "note", Msg: "must be 200 characters or fewer"}
	}
	var out CountView
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
		if err := s.activeLocation(ctx, q, locationID); err != nil {
			return err
		}
		no, err := q.NextCounterValue(ctx, "count")
		if err != nil {
			return err
		}
		id, err := q.InsertCount(ctx, repository.InsertCountParams{CountNo: no, LocationID: locationID, Note: pgtype.Text{String: note, Valid: note != ""}, CreatedBy: actor.UserID})
		if err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "inventory.count_started", "stock_count", id, nil, map[string]any{"count_no": no, "location_id": locationID}); err != nil {
			return err
		}
		out, err = s.loadCount(ctx, q, id, true)
		if err != nil {
			return err
		}
		if idem != nil {
			b, err := json.Marshal(map[string]any{"count": out})
			if err != nil {
				return err
			}
			return idempotency.Complete(ctx, tx, idem, 201, b)
		}
		return nil
	})
	return out, replay, err
}

type CountLineInput struct {
	ItemID int64
	Qty    int64 // what was counted, in UnitID (or the base unit); 0 is allowed: the shelf is empty
	UnitID *int64
}

// SetCountLines replaces the sheet's lines with the ones given. A line whose counted amount did not change keeps the
// books figure it was entered against; a new or changed line is entered against the books as they are now.
func (s *Service) SetCountLines(ctx context.Context, actor *auth.Principal, id int64, in []CountLineInput, m Meta) (CountView, error) {
	if !canAdjust(actor) {
		return CountView{}, ErrForbidden
	}
	if len(in) > maxCountLines {
		return CountView{}, &ValidationError{Field: "lines", Msg: "has too many items (500 at most)"}
	}
	seen := map[int64]bool{}
	ids := make([]int64, 0, len(in))
	for _, l := range in {
		if seen[l.ItemID] {
			return CountView{}, &ValidationError{Field: "lines", Msg: "lists the same item twice"}
		}
		seen[l.ItemID] = true
		ids = append(ids, l.ItemID)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	var out CountView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		c, err := q.LockCount(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "count"}
		}
		if err != nil {
			return err
		}
		if c.Status != "draft" {
			return &ConflictError{Code: "count_not_open", Msg: "This count is already " + c.Status + "."}
		}
		if len(ids) > 0 {
			locked, err := q.LockItems(ctx, ids)
			if err != nil {
				return err
			}
			if len(locked) != len(ids) {
				return &ValidationError{Field: "item_id", Msg: "refers to an item that does not exist"}
			}
		}
		for _, l := range in {
			if l.Qty < 0 || l.Qty > maxQty {
				return &ValidationError{Field: "qty", Msg: "must be a whole number from 0 to 1,000,000,000"}
			}
			item, err := s.loadItem(ctx, q, l.ItemID)
			if err != nil {
				return err
			}
			if !item.Active {
				return &ConflictError{Code: "item_inactive", Msg: item.Name + " is switched off. Switch it on to count it."}
			}
			var base, unit int64
			if l.Qty == 0 {
				unit = item.BaseUnit.ID
			} else if base, unit, err = toBase(ctx, q, item, l.Qty, l.UnitID); err != nil {
				return err
			}
			expected := int64(0)
			prev, err := q.GetCountLine(ctx, repository.GetCountLineParams{CountID: id, ItemID: l.ItemID})
			switch {
			case err == nil && prev.CountedBase == base:
				expected = prev.ExpectedBase
			case err == nil || errors.Is(err, pgx.ErrNoRows):
				if expected, err = q.BalanceOf(ctx, repository.BalanceOfParams{ItemID: l.ItemID, LocationID: c.LocationID}); err != nil {
					return err
				}
			default:
				return err
			}
			if err := q.UpsertCountLine(ctx, repository.UpsertCountLineParams{CountID: id, ItemID: l.ItemID, CountedBase: base, EnteredQty: l.Qty, EnteredUnitID: unit, ExpectedBase: expected}); err != nil {
				return err
			}
		}
		if err := q.DeleteCountLinesExcept(ctx, repository.DeleteCountLinesExceptParams{CountID: id, Column2: ids}); err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "inventory.count_lines_saved", "stock_count", id, nil, map[string]any{"count_no": c.CountNo, "lines": len(in)}); err != nil {
			return err
		}
		out, err = s.loadCount(ctx, q, id, true)
		return err
	})
	return out, err
}

// ApproveCount posts each line's difference as a count_adjustment movement and closes the count, once. A count with no
// lines cannot be approved. Approving is the correction itself, so it is not held back by the negative-stock rule; a
// place that ends below zero shows in the below-zero list (S-91).
func (s *Service) ApproveCount(ctx context.Context, actor *auth.Principal, id int64, idem *idempotency.Key, m Meta) (CountView, *idempotency.Stored, error) {
	if !canAdjust(actor) {
		return CountView{}, nil, ErrForbidden
	}
	var out CountView
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
		c, err := q.LockCount(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "count"}
		}
		if err != nil {
			return err
		}
		if c.Status != "draft" {
			return &ConflictError{Code: "count_not_open", Msg: "This count is already " + c.Status + "."}
		}
		lines, err := q.ListCountLines(ctx, id)
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			return &ValidationError{Field: "lines", Msg: "has nothing counted yet"}
		}
		ids := make([]int64, 0, len(lines))
		for _, l := range lines {
			ids = append(ids, l.ItemID)
		}
		locked, err := q.LockItems(ctx, ids)
		if err != nil {
			return err
		}
		for _, it := range locked {
			if !it.Active {
				return &ConflictError{Code: "item_inactive", Msg: it.Name + " is switched off. Switch it on to approve this count."}
			}
		}
		reason := pgtype.Text{String: "Stock count #" + itoa(c.CountNo), Valid: true}
		posted, net := 0, int64(0)
		for _, l := range lines {
			delta := l.CountedBase - l.ExpectedBase
			if delta == 0 {
				continue
			}
			if _, err := q.InsertMovement(ctx, repository.InsertMovementParams{ItemID: l.ItemID, LocationID: c.LocationID, QtyDelta: delta, MovementType: "count_adjustment", Reason: reason,
				RefType: pgtype.Text{String: "stock_count", Valid: true}, RefID: pgtype.Int8{Int64: id, Valid: true}, CreatedBy: actor.UserID}); err != nil {
				return err
			}
			posted++
			net += delta
		}
		if err := q.MarkCountApproved(ctx, repository.MarkCountApprovedParams{ID: id, ApprovedBy: pgtype.Int8{Int64: actor.UserID, Valid: true}}); err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "inventory.count_approved", "stock_count", id, map[string]any{"status": "draft"},
			map[string]any{"status": "approved", "count_no": c.CountNo, "location_id": c.LocationID, "lines": len(lines), "adjustments": posted, "net_base": net}); err != nil {
			return err
		}
		out, err = s.loadCount(ctx, q, id, true)
		if err != nil {
			return err
		}
		if idem != nil {
			b, err := json.Marshal(map[string]any{"count": out})
			if err != nil {
				return err
			}
			return idempotency.Complete(ctx, tx, idem, 200, b)
		}
		return nil
	})
	return out, replay, err
}

// CancelCount abandons a draft sheet. Nothing was posted.
func (s *Service) CancelCount(ctx context.Context, actor *auth.Principal, id int64, m Meta) (CountView, error) {
	if !canAdjust(actor) {
		return CountView{}, ErrForbidden
	}
	var out CountView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		c, err := q.LockCount(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "count"}
		}
		if err != nil {
			return err
		}
		if c.Status != "draft" {
			return &ConflictError{Code: "count_not_open", Msg: "This count is already " + c.Status + "."}
		}
		if err := q.MarkCountCancelled(ctx, repository.MarkCountCancelledParams{ID: id, CancelledBy: pgtype.Int8{Int64: actor.UserID, Valid: true}}); err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "inventory.count_cancelled", "stock_count", id, map[string]any{"status": "draft"}, map[string]any{"status": "cancelled", "count_no": c.CountNo}); err != nil {
			return err
		}
		out, err = s.loadCount(ctx, q, id, true)
		return err
	})
	return out, err
}

// LowStock lists the active items whose total, over every place, is at or under their low-stock level.
func (s *Service) LowStock(ctx context.Context, actor *auth.Principal) ([]ItemView, error) {
	items, err := s.ListItems(ctx, actor, ItemFilter{OnlyActive: true})
	if err != nil {
		return nil, err
	}
	out := make([]ItemView, 0)
	for _, i := range items {
		if i.Low {
			out = append(out, i)
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].TotalBase < out[b].TotalBase })
	return out, nil
}
