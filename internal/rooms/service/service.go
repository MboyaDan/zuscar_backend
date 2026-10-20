// Package service is the rooms module: room types, rooms, guests, availability and reservations (Stage 22).
// Check-in, stays, folios and housekeeping arrive in Stage 23. The overlap rule lives in the database (ADR-06);
// this package only turns its answer into a clear 409.
package service

import (
	"context"
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

	inventoryservice "zuscar/backend/internal/inventory/service"
	orderservice "zuscar/backend/internal/orders/service"
	"zuscar/backend/internal/platform/audit"
	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/rooms/repository"
	salesservice "zuscar/backend/internal/sales/service"
)

var ErrForbidden = errors.New("forbidden")

type ValidationError struct{ Field, Msg string }

func (e *ValidationError) Error() string { return e.Field + " " + e.Msg }

type NotFoundError struct{ What string }

func (e *NotFoundError) Error() string { return e.What + " not found" }

type ForbiddenError struct{ Code, Msg string }

func (e *ForbiddenError) Error() string { return e.Msg }

type ConflictError struct{ Code, Field, Msg string }

func (e *ConflictError) Error() string { return e.Msg }

type Meta struct {
	RequestID string
	IP        *netip.Addr
}

// Payments is what the folio needs from the sales module: money taken against a folio is a payment, never a folio row (NN-10).
type Payments interface {
	RecordFolioPayment(ctx context.Context, tx pgx.Tx, actor *auth.Principal, in salesservice.FolioPayment) (salesservice.PaymentRecord, error)
	FolioPaymentTotals(ctx context.Context, tx pgx.Tx, folioID int64) (confirmed, pending int64, err error)
	FolioPayments(ctx context.Context, tx pgx.Tx, folioID int64) ([]salesservice.PaymentRecord, error)
	PrepareFolioCharge(ctx context.Context, actor *auth.Principal, in salesservice.FinaliseInput) (salesservice.ChargePrep, error)
	FinaliseToFolio(ctx context.Context, tx pgx.Tx, actor *auth.Principal, folioID, orderID int64, in salesservice.FinaliseInput, prep salesservice.ChargePrep, m salesservice.Meta) (salesservice.SaleView, error)
}

type Service struct {
	pool *pgxpool.Pool
	rec  audit.Recorder
	pay  Payments
}

func New(pool *pgxpool.Pool, rec audit.Recorder, pay Payments) *Service {
	return &Service{pool: pool, rec: rec, pay: pay}
}

// translate turns the sales module's errors into this module's, so callers see one set of error types.
func translate(err error) error {
	var v *salesservice.ValidationError
	var c *salesservice.ConflictError
	var n *salesservice.NotFoundError
	var fe *salesservice.ForbiddenError
	var on *orderservice.NotFoundError
	var oc *orderservice.ConflictError
	var ic *inventoryservice.ConflictError
	var iv *inventoryservice.ValidationError
	switch {
	case errors.Is(err, salesservice.ErrForbidden):
		return ErrForbidden
	case errors.As(err, &fe):
		return &ForbiddenError{Code: fe.Code, Msg: fe.Msg}
	case errors.As(err, &on):
		return &NotFoundError{What: on.What}
	case errors.As(err, &oc):
		return &ConflictError{Code: oc.Code, Msg: oc.Msg}
	case errors.As(err, &ic):
		return &ConflictError{Code: ic.Code, Msg: ic.Msg}
	case errors.As(err, &iv):
		return &ValidationError{Field: iv.Field, Msg: iv.Msg}
	case errors.As(err, &v):
		return &ValidationError{Field: v.Field, Msg: v.Msg}
	case errors.As(err, &c):
		return &ConflictError{Code: c.Code, Field: c.Field, Msg: c.Msg}
	case errors.As(err, &n):
		return &NotFoundError{What: n.What}
	}
	return err
}

const (
	maxRate     = 10_000_000 // cents per night (KES 100,000)
	maxNights   = 90
	dateLayout  = "2006-01-02"
	defaultList = 50
)

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func can(a *auth.Principal) bool { return a != nil && a.Has("rooms.manage") }

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

func runeLen(s string) int { return len([]rune(s)) }

func (s *Service) record(ctx context.Context, tx pgx.Tx, actor *auth.Principal, m Meta, action, entity string, id int64, before, after map[string]any) error {
	return s.rec.Record(ctx, tx, audit.Entry{ActorID: actor.UserID, ActorLabel: actor.Username, Action: action, EntityType: entity,
		EntityID: itoa(id), Before: before, After: after, RequestID: m.RequestID, IP: m.IP})
}

func clampLimit(n int) int32 {
	if n <= 0 {
		return defaultList
	}
	if n > 200 {
		return 200
	}
	return int32(n)
}

func isUnique(err error, constraint string) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && strings.EqualFold(pg.ConstraintName, constraint)
}

// isOverlap is the exclusion violation: another confirmed or checked-in booking holds the room on one of those nights.
func isOverlap(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23P01"
}

func required(v, field string, max int) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || runeLen(v) > max {
		return "", &ValidationError{Field: field, Msg: "is required and must be at most " + strconv.Itoa(max) + " characters"}
	}
	return v, nil
}

func optText(v string, max int, field string) (pgtype.Text, error) {
	v = strings.TrimSpace(v)
	if runeLen(v) > max {
		return pgtype.Text{}, &ValidationError{Field: field, Msg: "is too long"}
	}
	return pgtype.Text{String: v, Valid: v != ""}, nil
}

func parseDate(v, field string) (time.Time, error) {
	t, err := time.Parse(dateLayout, strings.TrimSpace(v))
	if err != nil {
		return time.Time{}, &ValidationError{Field: field, Msg: "must be a date like 2026-12-24"}
	}
	return t, nil
}

func dateStr(d pgtype.Date) string {
	if !d.Valid {
		return ""
	}
	return d.Time.Format(dateLayout)
}

func int4Ptr(v pgtype.Int4) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int32)
	return &n
}

func (s *Service) today(ctx context.Context, q *repository.Queries) (time.Time, error) {
	d, err := q.BusinessDate(ctx)
	if err != nil {
		return time.Time{}, err
	}
	return d.Time, nil
}
