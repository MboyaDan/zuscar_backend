// Package service is the expenses module (Stage 26): a ledger of money spent that is not stock bought through purchasing.
// An entry is never edited or deleted. A wrong one is voided with a reason, and a new one is entered (S-111).
package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"zuscar/backend/internal/expenses/repository"
	"zuscar/backend/internal/platform/audit"
	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/platform/idempotency"
)

var ErrForbidden = errors.New("forbidden")

type ValidationError struct{ Field, Msg string }

func (e *ValidationError) Error() string { return e.Field + " " + e.Msg }

type NotFoundError struct{ What string }

func (e *NotFoundError) Error() string { return e.What + " not found" }

type ConflictError struct{ Code, Msg string }

func (e *ConflictError) Error() string { return e.Msg }

type Meta struct {
	RequestID string
	IP        *netip.Addr
}

type Settings interface {
	GetSettings(ctx context.Context, actor *auth.Principal) (map[string]json.RawMessage, error)
}

// Categories and Methods are the allowed values; the database holds the same lists in a CHECK.
var (
	Categories = []string{"rent", "utilities", "wages", "transport", "repairs", "supplies", "licences", "marketing", "other"}
	Methods    = []string{"cash", "mpesa", "bank", "card", "other"}
)

const (
	maxCents     = 100_000_000_000
	maxDaysBack  = 400
	maxListDays  = 366
	defaultLimit = 200
)

type Service struct {
	pool     *pgxpool.Pool
	rec      audit.Recorder
	settings Settings
	now      func() time.Time
}

func New(pool *pgxpool.Pool, rec audit.Recorder, settings Settings) *Service {
	return &Service{pool: pool, rec: rec, settings: settings, now: time.Now}
}

func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

func can(a *auth.Principal) bool { return a != nil && a.Has("expenses.manage") }

func runeLen(s string) int { return len([]rune(s)) }

func oneOf(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Service) today(ctx context.Context, actor *auth.Principal) time.Time {
	zone := "Africa/Nairobi"
	if raw, err := s.settings.GetSettings(ctx, actor); err == nil {
		var z string
		if v, ok := raw["timezone"]; ok && json.Unmarshal(v, &z) == nil && z != "" {
			zone = z
		}
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc = time.FixedZone("EAT", 3*3600)
	}
	n := s.now().In(loc)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

func parseDay(field, v string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return time.Time{}, &ValidationError{Field: field, Msg: "must be a date like 2026-10-20"}
	}
	return t, nil
}

type ExpenseView struct {
	ID          int64   `json:"id"`
	Date        string  `json:"expense_date"`
	Description string  `json:"description"`
	Category    string  `json:"category"`
	AmountCents int64   `json:"amount_cents"`
	PaidBy      string  `json:"paid_by"`
	PaidWith    string  `json:"paid_with"`
	EnteredBy   string  `json:"entered_by"`
	EnteredAt   string  `json:"entered_at"`
	Voided      bool    `json:"voided"`
	VoidReason  *string `json:"void_reason"`
}

func view(id int64, date pgtype.Date, desc, cat string, amount int64, paidBy, paidWith, enteredBy string, at, voidedAt pgtype.Timestamptz, reason pgtype.Text) ExpenseView {
	v := ExpenseView{ID: id, Date: date.Time.Format("2006-01-02"), Description: desc, Category: cat, AmountCents: amount, PaidBy: paidBy, PaidWith: paidWith,
		EnteredBy: enteredBy, EnteredAt: at.Time.UTC().Format(time.RFC3339), Voided: voidedAt.Valid}
	if reason.Valid {
		r := reason.String
		v.VoidReason = &r
	}
	return v
}

type CreateInput struct {
	Date        string
	Description string
	Category    string
	AmountCents int64
	PaidBy      string
	PaidWith    string
}

func (s *Service) record(ctx context.Context, tx pgx.Tx, actor *auth.Principal, m Meta, action string, id int64, after map[string]any) error {
	return s.rec.Record(ctx, tx, audit.Entry{ActorID: actor.UserID, ActorLabel: actor.Username, Action: action, EntityType: "expense",
		EntityID: strconv.FormatInt(id, 10), After: after, RequestID: m.RequestID, IP: m.IP})
}

// Create writes one expense. The same Idempotency-Key replays the first answer, so a double tap on a slow line adds it once.
func (s *Service) Create(ctx context.Context, actor *auth.Principal, in CreateInput, idem *idempotency.Key, m Meta) (ExpenseView, *idempotency.Stored, error) {
	if !can(actor) {
		return ExpenseView{}, nil, ErrForbidden
	}
	day, err := parseDay("expense_date", strings.TrimSpace(in.Date))
	if err != nil {
		return ExpenseView{}, nil, err
	}
	today := s.today(ctx, actor)
	if day.After(today) {
		return ExpenseView{}, nil, &ValidationError{Field: "expense_date", Msg: "cannot be in the future"}
	}
	if today.Sub(day).Hours()/24 > maxDaysBack {
		return ExpenseView{}, nil, &ValidationError{Field: "expense_date", Msg: "is too far back (at most 400 days)"}
	}
	desc, paidBy := strings.TrimSpace(in.Description), strings.TrimSpace(in.PaidBy)
	if runeLen(desc) < 1 || runeLen(desc) > 200 {
		return ExpenseView{}, nil, &ValidationError{Field: "description", Msg: "is required (1 to 200 characters)"}
	}
	if runeLen(paidBy) < 1 || runeLen(paidBy) > 80 {
		return ExpenseView{}, nil, &ValidationError{Field: "paid_by", Msg: "is required: who paid (1 to 80 characters)"}
	}
	if !oneOf(Categories, in.Category) {
		return ExpenseView{}, nil, &ValidationError{Field: "category", Msg: "must be one of " + strings.Join(Categories, ", ")}
	}
	if !oneOf(Methods, in.PaidWith) {
		return ExpenseView{}, nil, &ValidationError{Field: "paid_with", Msg: "must be one of " + strings.Join(Methods, ", ")}
	}
	if in.AmountCents <= 0 || in.AmountCents > maxCents {
		return ExpenseView{}, nil, &ValidationError{Field: "amount_cents", Msg: "must be greater than 0"}
	}
	var out ExpenseView
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
		id, err := q.InsertExpense(ctx, repository.InsertExpenseParams{ExpenseDate: pgtype.Date{Time: day, Valid: true}, Description: desc, Category: in.Category,
			AmountCents: in.AmountCents, PaidBy: paidBy, PaidWith: in.PaidWith, CreatedBy: actor.UserID})
		if err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "expense.created", id, map[string]any{"expense_date": in.Date, "description": desc, "category": in.Category,
			"amount_cents": in.AmountCents, "paid_by": paidBy, "paid_with": in.PaidWith}); err != nil {
			return err
		}
		out, err = s.load(ctx, q, id)
		if err != nil {
			return err
		}
		if idem != nil {
			b, err := json.Marshal(map[string]any{"expense": out})
			if err != nil {
				return err
			}
			return idempotency.Complete(ctx, tx, idem, 201, b)
		}
		return nil
	})
	if err != nil {
		return ExpenseView{}, nil, err
	}
	return out, replay, nil
}

func (s *Service) load(ctx context.Context, q *repository.Queries, id int64) (ExpenseView, error) {
	r, err := q.GetExpense(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ExpenseView{}, &NotFoundError{What: "expense"}
	}
	if err != nil {
		return ExpenseView{}, err
	}
	return view(r.ID, r.ExpenseDate, r.Description, r.Category, r.AmountCents, r.PaidBy, r.PaidWith, r.CreatedByName, r.CreatedAt, r.VoidedAt, r.VoidReason), nil
}

// Void marks an expense as entered by mistake. It keeps the row, who voided it and why, and takes it out of every total.
func (s *Service) Void(ctx context.Context, actor *auth.Principal, id int64, reason string, m Meta) (ExpenseView, error) {
	if !can(actor) {
		return ExpenseView{}, ErrForbidden
	}
	reason = strings.TrimSpace(reason)
	if runeLen(reason) < 1 || runeLen(reason) > 200 {
		return ExpenseView{}, &ValidationError{Field: "reason", Msg: "is required (1 to 200 characters)"}
	}
	var out ExpenseView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		row, err := q.LockExpense(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "expense"}
		}
		if err != nil {
			return err
		}
		if row.VoidedAt.Valid {
			return &ConflictError{Code: "already_voided", Msg: "That expense was already voided."}
		}
		if err := q.VoidExpense(ctx, repository.VoidExpenseParams{ID: id, VoidedBy: pgtype.Int8{Int64: actor.UserID, Valid: true}, VoidReason: pgtype.Text{String: reason, Valid: true}}); err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "expense.voided", id, map[string]any{"reason": reason}); err != nil {
			return err
		}
		out, err = s.load(ctx, q, id)
		return err
	})
	return out, err
}

// List returns the expenses for a range of business dates, newest first. Without dates it is the last 30 days.
func (s *Service) List(ctx context.Context, actor *auth.Principal, from, to string, includeVoided bool, limit int) ([]ExpenseView, bool, error) {
	if !can(actor) {
		return nil, false, ErrForbidden
	}
	today := s.today(ctx, actor)
	f, t := today.AddDate(0, 0, -29), today
	var err error
	if from != "" {
		if f, err = parseDay("from", from); err != nil {
			return nil, false, err
		}
		if to == "" {
			t = f
		}
	}
	if to != "" {
		if t, err = parseDay("to", to); err != nil {
			return nil, false, err
		}
		if from == "" {
			f = t
		}
	}
	if t.Before(f) {
		return nil, false, &ValidationError{Field: "to", Msg: "must not be before from"}
	}
	if int(t.Sub(f).Hours()/24)+1 > maxListDays {
		return nil, false, &ValidationError{Field: "to", Msg: "the range can be at most 366 days"}
	}
	if limit <= 0 || limit > defaultLimit {
		limit = defaultLimit
	}
	rows, err := repository.New(s.pool).ListExpenses(ctx, repository.ListExpensesParams{FromDay: pgtype.Date{Time: f, Valid: true}, ToDay: pgtype.Date{Time: t, Valid: true},
		IncludeVoided: includeVoided, RowLimit: int32(limit) + 1})
	if err != nil {
		return nil, false, err
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	out := make([]ExpenseView, 0, len(rows))
	for _, r := range rows {
		out = append(out, view(r.ID, r.ExpenseDate, r.Description, r.Category, r.AmountCents, r.PaidBy, r.PaidWith, r.CreatedByName, r.CreatedAt, r.VoidedAt, r.VoidReason))
	}
	return out, more, nil
}
