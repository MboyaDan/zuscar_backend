// Package service is the purchasing module: suppliers, purchases (deliveries) and receiving them into stock.
// Stock rules stay in the inventory module; this module reaches them only through the Stock interface (docs/architecture.md section 3).
package service

import (
	"context"
	"errors"
	"net/netip"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	invservice "zuscar/backend/internal/inventory/service"
	"zuscar/backend/internal/platform/audit"
	"zuscar/backend/internal/platform/auth"
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

// Stock is what purchasing needs from the inventory module.
type Stock interface {
	ResolveLine(ctx context.Context, tx pgx.Tx, in invservice.LineRequest) (invservice.ResolvedLine, error)
	CheckLocation(ctx context.Context, tx pgx.Tx, id int64) error
	Receive(ctx context.Context, tx pgx.Tx, actor *auth.Principal, purchaseID, locationID int64, lines []invservice.ReceiptLine) error
	ReversedReceipts(ctx context.Context, purchaseID int64) (map[int64]bool, error)
}

type Service struct {
	pool  *pgxpool.Pool
	rec   audit.Recorder
	stock Stock
}

func New(pool *pgxpool.Pool, rec audit.Recorder, stock Stock) *Service {
	return &Service{pool: pool, rec: rec, stock: stock}
}

// translate turns the inventory module's errors into this module's, so callers see one set of error types.
func translate(err error) error {
	var v *invservice.ValidationError
	var c *invservice.ConflictError
	var n *invservice.NotFoundError
	switch {
	case errors.As(err, &v):
		return &ValidationError{Field: v.Field, Msg: v.Msg}
	case errors.As(err, &c):
		return &ConflictError{Code: c.Code, Field: c.Field, Msg: c.Msg}
	case errors.As(err, &n):
		return &NotFoundError{What: n.What}
	}
	return err
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

func runeLen(s string) int { return len([]rune(s)) }

func (s *Service) record(ctx context.Context, tx pgx.Tx, actor *auth.Principal, m Meta, action, entity string, id int64, before, after map[string]any) error {
	return s.rec.Record(ctx, tx, audit.Entry{ActorID: actor.UserID, ActorLabel: actor.Username, Action: action, EntityType: entity,
		EntityID: itoa(id), Before: before, After: after, RequestID: m.RequestID, IP: m.IP})
}

func clampLimit(n int) int32 {
	if n <= 0 {
		return 50
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

func canPurchase(a *auth.Principal) bool { return a != nil && a.Has("purchasing.manage") }
