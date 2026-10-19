package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/platform/idempotency"
	"zuscar/backend/internal/sales/repository"
)

const maxCents = 100_000_000_00

// shiftAdmin: these roles may act on and see any shift. Everyone else with a shifts permission works on their own (rbac.md note 1, S-55).
func shiftAdmin(actor *auth.Principal) bool { return hasRole(actor, "super_admin", "business_manager") }

func hasCash(in []PaymentInput) bool {
	for _, p := range in {
		if p.Method == "cash" {
			return true
		}
	}
	return false
}

// shiftFor returns the actor's open shift, which a sale or payment is stamped with. Cash needs one (behaviour.md section 3);
// other sales are stamped with it when there is one and left without when there is not. The shift row is locked FOR SHARE,
// so a close waits for the payment being written and a payment after the close finds no open shift.
func (s *Service) shiftFor(ctx context.Context, q *repository.Queries, actor *auth.Principal, needed bool) (pgtype.Int8, error) {
	id, err := q.OpenShiftForUser(ctx, actor.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		if needed {
			return pgtype.Int8{}, &ConflictError{Code: "shift_required", Msg: "Open your shift before taking cash."}
		}
		return pgtype.Int8{}, nil
	}
	if err != nil {
		return pgtype.Int8{}, err
	}
	return pgtype.Int8{Int64: id, Valid: true}, nil
}

// ---- views ----

type ShiftView struct {
	ID                int64      `json:"id"`
	UserID            int64      `json:"user_id"`
	UserName          string     `json:"user_name"`
	Status            string     `json:"status"`
	OpenedAt          time.Time  `json:"opened_at"`
	OpeningFloatCents int64      `json:"opening_float_cents"`
	ClosedAt          *time.Time `json:"closed_at"`
	ClosedBy          *int64     `json:"closed_by"`
	DeclaredCashCents *int64     `json:"declared_cash_cents"`
	ExpectedCashCents *int64     `json:"expected_cash_cents"`
	VarianceCents     *int64     `json:"variance_cents"`
	ReconciledAt      *time.Time `json:"reconciled_at"`
	ReconciledBy      *int64     `json:"reconciled_by"`
	Notes             string     `json:"notes,omitempty"`
}

type MovementView struct {
	ID            int64     `json:"id"`
	Kind          string    `json:"kind"`
	AmountCents   int64     `json:"amount_cents"`
	Reason        string    `json:"reason"`
	CreatedBy     int64     `json:"created_by"`
	CreatedByName string    `json:"created_by_name"`
	CreatedAt     time.Time `json:"created_at"`
}

type CashBreakdown struct {
	OpeningFloatCents int64 `json:"opening_float_cents"`
	CashSalesCents    int64 `json:"cash_payments_cents"` // confirmed cash payments
	CashRefundsCents  int64 `json:"cash_refunds_cents"`  // Stage 15
	PaidInCents       int64 `json:"paid_in_cents"`
	PaidOutCents      int64 `json:"paid_out_cents"`
	ExpensesCents     int64 `json:"expenses_cents"`
	ExpectedCashCents int64 `json:"expected_cash_cents"`
}

type PaymentTotal struct {
	Method      string `json:"method"`
	Status      string `json:"status"`
	Payments    int64  `json:"payments"`
	AmountCents int64  `json:"amount_cents"`
}

type ShiftSummary struct {
	Shift     ShiftView      `json:"shift"`
	Cash      CashBreakdown  `json:"cash"`
	Sales     map[string]any `json:"sales"`
	Payments  []PaymentTotal `json:"payments"`         // everything the shift took, by method and status
	Pending   []PaymentTotal `json:"pending_not_cash"` // awaiting confirmation: listed apart because it is not cash (S-21)
	Movements []MovementView `json:"movements"`
}

func shiftView(r repository.GetShiftRow) ShiftView {
	return shiftFrom(r.ID, r.UserID, r.UserName, r.Status, r.OpenedAt, r.OpeningFloatCents, r.ClosedAt, r.ClosedBy, r.DeclaredCashCents, r.ExpectedCashCents, r.VarianceCents, r.ReconciledAt, r.ReconciledBy, r.Notes)
}

func shiftFrom(id, uid int64, name, status string, opened pgtype.Timestamptz, float int64, closed pgtype.Timestamptz, closedBy int64, declared, expected, variance pgtype.Int8,
	rec pgtype.Timestamptz, recBy int64, notes string) ShiftView {
	i8 := func(v pgtype.Int8) *int64 {
		if !v.Valid {
			return nil
		}
		x := v.Int64
		return &x
	}
	return ShiftView{ID: id, UserID: uid, UserName: name, Status: status, OpenedAt: opened.Time, OpeningFloatCents: float, ClosedAt: optTime(closed), ClosedBy: optID(closedBy),
		DeclaredCashCents: i8(declared), ExpectedCashCents: i8(expected), VarianceCents: i8(variance), ReconciledAt: optTime(rec), ReconciledBy: optID(recBy), Notes: notes}
}

// cashBreakdown derives the expected cash from rows (AC-09): float + confirmed cash payments + paid-in - paid-out - expenses
// (- cash refunds, once Stage 15 records them). Pending non-cash money is never part of it (AC-19).
func cashBreakdown(ctx context.Context, q *repository.Queries, shiftID, float int64) (CashBreakdown, []PaymentTotal, error) {
	b := CashBreakdown{OpeningFloatCents: float}
	pays, err := q.ShiftPaymentTotals(ctx, pgtype.Int8{Int64: shiftID, Valid: true})
	if err != nil {
		return b, nil, err
	}
	var all []PaymentTotal
	for _, p := range pays {
		all = append(all, PaymentTotal{Method: p.Method, Status: p.Status, Payments: p.Payments, AmountCents: p.AmountCents})
		if p.Method == "cash" && p.Status == "confirmed" {
			b.CashSalesCents += p.AmountCents
		}
	}
	mv, err := q.ShiftMovementTotals(ctx, shiftID)
	if err != nil {
		return b, nil, err
	}
	for _, m := range mv {
		switch m.Kind {
		case "paid_in":
			b.PaidInCents = m.AmountCents
		case "paid_out":
			b.PaidOutCents = m.AmountCents
		case "expense":
			b.ExpensesCents = m.AmountCents
		}
	}
	b.ExpectedCashCents = b.OpeningFloatCents + b.CashSalesCents - b.CashRefundsCents + b.PaidInCents - b.PaidOutCents - b.ExpensesCents
	return b, all, nil
}

// ---- running one write with an idempotency key ----

func (s *Service) runIdem(ctx context.Context, idem *idempotency.Key, status int, work func(tx pgx.Tx, q *repository.Queries) (any, error)) (replay *idempotency.Stored, out any, err error) {
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
		o, err := work(tx, repository.New(tx))
		if err != nil {
			return err
		}
		out = o
		if idem != nil {
			b, err := json.Marshal(o)
			if err != nil {
				return err
			}
			return idempotency.Complete(ctx, tx, idem, status, b)
		}
		return nil
	})
	return replay, out, err
}

func (s *Service) loadShift(ctx context.Context, q *repository.Queries, id int64) (ShiftView, error) {
	r, err := q.GetShift(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ShiftView{}, &NotFoundError{What: "shift"}
	}
	if err != nil {
		return ShiftView{}, err
	}
	return shiftView(r), nil
}

// ownShift hides another person's shift from a role that is not a shift admin: it answers 404, not 403 (S-55).
func ownShift(actor *auth.Principal, owner int64) error {
	if !shiftAdmin(actor) && owner != actor.UserID {
		return &NotFoundError{What: "shift"}
	}
	return nil
}

// ---- open ----

type OpenShiftInput struct {
	OpeningFloatCents int64
	Notes             string
}

func (s *Service) OpenShift(ctx context.Context, actor *auth.Principal, in OpenShiftInput, idem *idempotency.Key, m Meta) (*idempotency.Stored, map[string]any, error) {
	if actor == nil || !actor.Has("shifts.manage") {
		return nil, nil, ErrForbidden
	}
	notes := strings.TrimSpace(in.Notes)
	if in.OpeningFloatCents < 0 || in.OpeningFloatCents > maxCents {
		return nil, nil, &ValidationError{Field: "opening_float_cents", Msg: "must be 0 or more"}
	}
	if runeLen(notes) > 500 {
		return nil, nil, &ValidationError{Field: "notes", Msg: "must be 500 characters or fewer"}
	}
	rp, out, err := s.runIdem(ctx, idem, 201, func(tx pgx.Tx, q *repository.Queries) (any, error) {
		id, err := q.InsertShift(ctx, repository.InsertShiftParams{UserID: actor.UserID, OpeningFloatCents: in.OpeningFloatCents, Notes: notes})
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "cashier_shifts_one_open_key" {
			return nil, &ConflictError{Code: "shift_already_open", Msg: "You already have an open shift. Close it before opening another."}
		}
		if err != nil {
			return nil, err
		}
		if err := s.record(ctx, tx, actor, m, "shift.opened", "shift", id, nil, map[string]any{"opening_float_cents": in.OpeningFloatCents}); err != nil {
			return nil, err
		}
		v, err := s.loadShift(ctx, q, id)
		return map[string]any{"shift": v}, err
	})
	if rp != nil || err != nil {
		return rp, nil, err
	}
	return nil, out.(map[string]any), nil
}

// CurrentShift is the caller's open shift with its summary, or nil when none is open.
func (s *Service) CurrentShift(ctx context.Context, actor *auth.Principal) (*ShiftSummary, error) {
	if actor == nil || !actor.Has("shifts.manage") {
		return nil, ErrForbidden
	}
	q := repository.New(s.pool)
	id, err := q.GetOpenShiftIDForUser(ctx, actor.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sum, err := s.summary(ctx, q, id)
	return &sum, err
}

func (s *Service) summary(ctx context.Context, q *repository.Queries, id int64) (ShiftSummary, error) {
	sh, err := s.loadShift(ctx, q, id)
	if err != nil {
		return ShiftSummary{}, err
	}
	b, all, err := cashBreakdown(ctx, q, id, sh.OpeningFloatCents)
	if err != nil {
		return ShiftSummary{}, err
	}
	if sh.ExpectedCashCents != nil { // a closed shift shows the figure it was closed with
		b.ExpectedCashCents = *sh.ExpectedCashCents
	}
	sc, err := q.ShiftSaleCount(ctx, pgtype.Int8{Int64: id, Valid: true})
	if err != nil {
		return ShiftSummary{}, err
	}
	mv, err := q.ListCashMovements(ctx, id)
	if err != nil {
		return ShiftSummary{}, err
	}
	out := ShiftSummary{Shift: sh, Cash: b, Sales: map[string]any{"count": sc.Sales, "total_cents": sc.TotalCents}, Payments: []PaymentTotal{}, Pending: []PaymentTotal{}, Movements: []MovementView{}}
	for _, p := range all {
		out.Payments = append(out.Payments, p)
		if p.Method != "cash" && p.Status == "pending_confirmation" {
			out.Pending = append(out.Pending, p)
		}
	}
	for _, m := range mv {
		out.Movements = append(out.Movements, MovementView{ID: m.ID, Kind: m.Kind, AmountCents: m.AmountCents, Reason: m.Reason, CreatedBy: m.CreatedBy, CreatedByName: m.CreatedByName, CreatedAt: m.CreatedAt.Time})
	}
	return out, nil
}

func (s *Service) ShiftSummary(ctx context.Context, actor *auth.Principal, id int64) (ShiftSummary, error) {
	if actor == nil || !actor.Has("shifts.manage") {
		return ShiftSummary{}, ErrForbidden
	}
	q := repository.New(s.pool)
	sh, err := s.loadShift(ctx, q, id)
	if err != nil {
		return ShiftSummary{}, err
	}
	if err := ownShift(actor, sh.UserID); err != nil {
		return ShiftSummary{}, err
	}
	return s.summary(ctx, q, id)
}

type ShiftFilter struct {
	Status   string
	UserID   int64
	BeforeID int64
	Limit    int
}

func (s *Service) ListShifts(ctx context.Context, actor *auth.Principal, f ShiftFilter) ([]ShiftView, bool, error) {
	if actor == nil || !actor.Has("shifts.close") {
		return nil, false, ErrForbidden
	}
	if !shiftAdmin(actor) {
		f.UserID = actor.UserID
	}
	limit := clampLimit(f.Limit)
	rows, err := repository.New(s.pool).ListShifts(ctx, repository.ListShiftsParams{Status: f.Status, UserID: f.UserID, BeforeID: f.BeforeID, RowLimit: limit + 1})
	if err != nil {
		return nil, false, err
	}
	more := int32(len(rows)) > limit
	if more {
		rows = rows[:limit]
	}
	out := make([]ShiftView, 0, len(rows))
	for _, r := range rows {
		out = append(out, shiftFrom(r.ID, r.UserID, r.UserName, r.Status, r.OpenedAt, r.OpeningFloatCents, r.ClosedAt, r.ClosedBy, r.DeclaredCashCents, r.ExpectedCashCents, r.VarianceCents,
			r.ReconciledAt, r.ReconciledBy, r.Notes))
	}
	return out, more, nil
}

// ---- cash movements ----

type MovementInput struct {
	Kind        string
	AmountCents int64
	Reason      string
}

func (s *Service) AddCashMovement(ctx context.Context, actor *auth.Principal, shiftID int64, in MovementInput, idem *idempotency.Key, m Meta) (*idempotency.Stored, map[string]any, error) {
	if actor == nil || !actor.Has("shifts.manage") {
		return nil, nil, ErrForbidden
	}
	reason := strings.TrimSpace(in.Reason)
	if in.Kind != "paid_in" && in.Kind != "paid_out" && in.Kind != "expense" {
		return nil, nil, &ValidationError{Field: "kind", Msg: "must be paid_in, paid_out or expense"}
	}
	if in.AmountCents <= 0 || in.AmountCents > maxCents {
		return nil, nil, &ValidationError{Field: "amount_cents", Msg: "must be greater than 0"}
	}
	if runeLen(reason) < 1 || runeLen(reason) > 200 {
		return nil, nil, &ValidationError{Field: "reason", Msg: "is required (1 to 200 characters)"}
	}
	rp, out, err := s.runIdem(ctx, idem, 201, func(tx pgx.Tx, q *repository.Queries) (any, error) {
		sh, err := q.LockShift(ctx, shiftID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, &NotFoundError{What: "shift"}
		}
		if err != nil {
			return nil, err
		}
		if err := ownShift(actor, sh.UserID); err != nil {
			return nil, err
		}
		if sh.Status != "open" {
			return nil, &ConflictError{Code: "shift_closed", Msg: "That shift is closed; cash can no longer be added to it."}
		}
		id, err := q.InsertCashMovement(ctx, repository.InsertCashMovementParams{ShiftID: shiftID, Kind: in.Kind, AmountCents: in.AmountCents, Reason: reason, CreatedBy: actor.UserID})
		if err != nil {
			return nil, err
		}
		if err := s.record(ctx, tx, actor, m, "shift.cash_movement", "shift", shiftID, nil, map[string]any{"movement_id": id, "kind": in.Kind, "amount_cents": in.AmountCents, "reason": reason}); err != nil {
			return nil, err
		}
		b, _, err := cashBreakdown(ctx, q, shiftID, sh.OpeningFloatCents)
		return map[string]any{"movement": map[string]any{"id": id, "kind": in.Kind, "amount_cents": in.AmountCents, "reason": reason}, "expected_cash_cents": b.ExpectedCashCents}, err
	})
	if rp != nil || err != nil {
		return rp, nil, err
	}
	return nil, out.(map[string]any), nil
}

// ---- close and reconcile ----

type CloseInput struct {
	DeclaredCashCents int64
	Notes             string
}

// CloseShift snapshots the expected cash and stores variance = declared - expected. The shift row is locked first, so a
// payment being written waits, and one that comes after finds no open shift. Pending non-cash payments do not block
// the close (S-21); they appear on the summary.
func (s *Service) CloseShift(ctx context.Context, actor *auth.Principal, shiftID int64, in CloseInput, idem *idempotency.Key, m Meta) (*idempotency.Stored, *ShiftSummary, error) {
	if actor == nil || !actor.Has("shifts.close") {
		return nil, nil, ErrForbidden
	}
	notes := strings.TrimSpace(in.Notes)
	if in.DeclaredCashCents < 0 || in.DeclaredCashCents > maxCents {
		return nil, nil, &ValidationError{Field: "declared_cash_cents", Msg: "must be 0 or more"}
	}
	if runeLen(notes) > 500 {
		return nil, nil, &ValidationError{Field: "notes", Msg: "must be 500 characters or fewer"}
	}
	var res ShiftSummary
	rp, _, err := s.runIdem(ctx, idem, 200, func(tx pgx.Tx, q *repository.Queries) (any, error) {
		sh, err := q.LockShift(ctx, shiftID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, &NotFoundError{What: "shift"}
		}
		if err != nil {
			return nil, err
		}
		if err := ownShift(actor, sh.UserID); err != nil {
			return nil, err
		}
		if sh.Status != "open" {
			return nil, &ConflictError{Code: "invalid_transition", Msg: "That shift is already closed."}
		}
		b, _, err := cashBreakdown(ctx, q, shiftID, sh.OpeningFloatCents)
		if err != nil {
			return nil, err
		}
		if err := q.CloseShift(ctx, repository.CloseShiftParams{ID: shiftID, ClosedBy: actor.UserID,
			Declared: in.DeclaredCashCents, Expected: b.ExpectedCashCents, Notes: notes}); err != nil {
			return nil, err
		}
		if err := s.record(ctx, tx, actor, m, "shift.closed", "shift", shiftID, map[string]any{"status": "open"},
			map[string]any{"status": "closed", "declared_cash_cents": in.DeclaredCashCents, "expected_cash_cents": b.ExpectedCashCents, "variance_cents": in.DeclaredCashCents - b.ExpectedCashCents}); err != nil {
			return nil, err
		}
		res, err = s.summary(ctx, q, shiftID)
		return res, err
	})
	if rp != nil || err != nil {
		return rp, nil, err
	}
	return nil, &res, nil
}

func (s *Service) ReconcileShift(ctx context.Context, actor *auth.Principal, shiftID int64, note string, idem *idempotency.Key, m Meta) (*idempotency.Stored, *ShiftView, error) {
	if actor == nil || !actor.Has("payments.reconcile") {
		return nil, nil, ErrForbidden
	}
	note = strings.TrimSpace(note)
	if runeLen(note) > 500 {
		return nil, nil, &ValidationError{Field: "note", Msg: "must be 500 characters or fewer"}
	}
	var res ShiftView
	rp, _, err := s.runIdem(ctx, idem, 200, func(tx pgx.Tx, q *repository.Queries) (any, error) {
		sh, err := q.LockShift(ctx, shiftID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, &NotFoundError{What: "shift"}
		}
		if err != nil {
			return nil, err
		}
		if sh.Status != "closed" {
			return nil, &ConflictError{Code: "invalid_transition", Msg: "Only a closed shift can be reconciled (this one is " + sh.Status + ")."}
		}
		if err := q.ReconcileShift(ctx, repository.ReconcileShiftParams{ID: shiftID, ReconciledBy: actor.UserID, Note: note}); err != nil {
			return nil, err
		}
		if err := s.record(ctx, tx, actor, m, "shift.reconciled", "shift", shiftID, map[string]any{"status": "closed"}, map[string]any{"status": "reconciled", "note": note}); err != nil {
			return nil, err
		}
		res, err = s.loadShift(ctx, q, shiftID)
		return map[string]any{"shift": res}, err
	})
	if rp != nil || err != nil {
		return rp, nil, err
	}
	return nil, &res, nil
}
