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

	"zuscar/backend/internal/platform/audit"
	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/rooms/repository"
)

var ErrForbidden = errors.New("forbidden")

type ValidationError struct{ Field, Msg string }

func (e *ValidationError) Error() string { return e.Field + " " + e.Msg }

type NotFoundError struct{ What string }

func (e *NotFoundError) Error() string { return e.What + " not found" }

type ConflictError struct{ Code, Field, Msg string }

func (e *ConflictError) Error() string { return e.Msg }

type Meta struct {
	RequestID string
	IP        *netip.Addr
}

type Service struct {
	pool *pgxpool.Pool
	rec  audit.Recorder
}

func New(pool *pgxpool.Pool, rec audit.Recorder) *Service { return &Service{pool: pool, rec: rec} }

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
