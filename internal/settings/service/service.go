// Package service is the settings module's rules: business settings and tax categories (Stage 7b).
// Changes need system.admin and a recent password entry (docs/rbac.md rule 6) and write an audit row with
// before and after values in the same transaction. Tax rates are configuration, never constants in code.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"zuscar/backend/internal/platform/audit"
	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/settings/repository"
)

var (
	ErrForbidden      = errors.New("you do not have permission to do this")
	ErrReauthRequired = errors.New("confirm your password to continue")
	ErrTaxRateMissing = errors.New("tax category not found")
	ErrTaxCodeTaken   = errors.New("that tax category code is already in use")
)

// ValidationError is a field-level input problem.
type ValidationError struct{ Field, Msg string }

func (e *ValidationError) Error() string { return e.Field + " " + e.Msg }

// Meta carries the request details the audit row records.
type Meta struct {
	RequestID string
	IP        *netip.Addr
}

type Clock interface{ Now() time.Time }

type Service struct {
	pool   *pgxpool.Pool
	clock  Clock
	rec    audit.Recorder
	reauth func(*auth.Principal) bool
}

// New wires the service. rec writes audit rows (the identity service); reauth reports whether the caller entered
// their password recently (also the identity service), so the window is defined in one place.
func New(pool *pgxpool.Pool, clock Clock, rec audit.Recorder, reauth func(*auth.Principal) bool) *Service {
	return &Service{pool: pool, clock: clock, rec: rec, reauth: reauth}
}

func (s *Service) guard(actor *auth.Principal) error {
	if !actor.Has("system.admin") {
		return ErrForbidden
	}
	if !s.reauth(actor) {
		return ErrReauthRequired
	}
	return nil
}

func (s *Service) record(ctx context.Context, tx pgx.Tx, actor *auth.Principal, m Meta, action, entityType, entityID string, before, after map[string]any) error {
	return s.rec.Record(ctx, tx, audit.Entry{ActorID: actor.UserID, ActorLabel: actor.Username, Action: action, EntityType: entityType,
		EntityID: entityID, Before: before, After: after, RequestID: m.RequestID, IP: m.IP})
}

const (
	statusKey         = "tax_config_status"
	StatusProvisional = "provisional_unverified"
	StatusVerified    = "verified"
)

var timePattern = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

type check func(raw json.RawMessage) (any, error)

func text(max int, required bool) check {
	return func(raw json.RawMessage) (any, error) {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, errors.New("must be text")
		}
		v = strings.TrimSpace(v)
		if n := len([]rune(v)); n > max || (required && n == 0) {
			return nil, errors.New("has an invalid length")
		}
		return v, nil
	}
}

func oneOf(options ...string) check {
	return func(raw json.RawMessage) (any, error) {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, errors.New("must be text")
		}
		for _, o := range options {
			if v == o {
				return v, nil
			}
		}
		return nil, errors.New("must be one of: " + strings.Join(options, ", "))
	}
}

func clockTime(raw json.RawMessage) (any, error) {
	var v string
	if err := json.Unmarshal(raw, &v); err != nil || !timePattern.MatchString(v) {
		return nil, errors.New("must be a time such as 14:00")
	}
	return v, nil
}

func zone(raw json.RawMessage) (any, error) {
	var v string
	if err := json.Unmarshal(raw, &v); err != nil || v == "" {
		return nil, errors.New("must be a time zone such as Africa/Nairobi")
	}
	if _, err := time.LoadLocation(v); err != nil {
		return nil, errors.New("is not a known time zone")
	}
	return v, nil
}

func boolean(raw json.RawMessage) (any, error) {
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, errors.New("must be true or false")
	}
	return v, nil
}

func hours(raw json.RawMessage) (any, error) {
	var v int
	if err := json.Unmarshal(raw, &v); err != nil || v < 1 || v > 168 {
		return nil, errors.New("must be a whole number of hours from 1 to 168")
	}
	return v, nil
}

// editable lists every setting an administrator may change (docs/schema-constraints.md section 2).
// currency and the tax verification record are not editable here.
var editable = map[string]check{
	"business_name": text(120, true), "address": text(200, false), "phone": text(40, false), "receipt_footer": text(300, false),
	"timezone": zone, "tax_inclusive": boolean, "negative_stock_policy": oneOf("warn_override", "block"),
	"checkin_time": clockTime, "checkout_time": clockTime, "idempotency_ttl_hours": hours,
	statusKey: oneOf(StatusProvisional, StatusVerified),
}

// GetSettings returns every setting. They are non-secret business configuration, readable by any signed-in user.
func (s *Service) GetSettings(ctx context.Context, actor *auth.Principal) (map[string]json.RawMessage, error) {
	if actor == nil {
		return nil, ErrForbidden
	}
	rows, err := repository.New(s.pool).ListSettings(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(rows))
	for _, r := range rows {
		out[r.Key] = json.RawMessage(r.Value)
	}
	return out, nil
}

func decodeValue(raw []byte) any {
	var v any
	_ = json.Unmarshal(raw, &v)
	return v
}

func currentStatus(ctx context.Context, q *repository.Queries) (string, error) {
	raw, err := q.GetSetting(ctx, statusKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return StatusProvisional, nil
	}
	if err != nil {
		return "", err
	}
	var v string
	_ = json.Unmarshal(raw, &v)
	return v, nil
}

func put(ctx context.Context, q *repository.Queries, actor *auth.Principal, key string, v any) error {
	b, _ := json.Marshal(v)
	return q.UpsertSetting(ctx, repository.UpsertSettingParams{Key: key, Value: b, UpdatedBy: pgtype.Int8{Int64: actor.UserID, Valid: true}})
}

// unverify returns the tax configuration to provisional when something that changes tax results is edited after
// it was verified: the verification covered the old values (S-41).
func unverify(ctx context.Context, q *repository.Queries, actor *auth.Principal) (bool, error) {
	st, err := currentStatus(ctx, q)
	if err != nil || st != StatusVerified {
		return false, err
	}
	return true, put(ctx, q, actor, statusKey, StatusProvisional)
}

// UpdateSettings applies a set of changes atomically. Unknown or read-only keys are refused.
func (s *Service) UpdateSettings(ctx context.Context, actor *auth.Principal, changes map[string]json.RawMessage, m Meta) (map[string]json.RawMessage, error) {
	if err := s.guard(actor); err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return nil, &ValidationError{Field: "settings", Msg: "must contain at least one setting"}
	}
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	clean := map[string]any{}
	for _, k := range keys {
		chk, ok := editable[k]
		if !ok {
			return nil, &ValidationError{Field: k, Msg: "is not a setting that can be changed here"}
		}
		v, err := chk(changes[k])
		if err != nil {
			return nil, &ValidationError{Field: k, Msg: err.Error()}
		}
		clean[k] = v
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		before, after := map[string]any{}, map[string]any{}
		for _, k := range keys {
			old, err := q.GetSetting(ctx, k)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			oldV := decodeValue(old)
			if b, _ := json.Marshal(oldV); string(b) == mustJSON(clean[k]) {
				continue // unchanged: no write, no audit noise
			}
			before[k], after[k] = oldV, clean[k]
			if err := put(ctx, q, actor, k, clean[k]); err != nil {
				return err
			}
		}
		if len(after) == 0 {
			return nil
		}
		now := s.clock.Now().UTC().Format(time.RFC3339)
		_, setsStatus := after[statusKey]
		if _, inclusive := after["tax_inclusive"]; inclusive && !setsStatus {
			if reset, err := unverify(ctx, q, actor); err != nil {
				return err
			} else if reset {
				before[statusKey], after[statusKey] = StatusVerified, StatusProvisional
			}
		}
		if after[statusKey] == StatusVerified {
			if err := put(ctx, q, actor, "tax_config_verified_by", actor.Username); err != nil {
				return err
			}
			if err := put(ctx, q, actor, "tax_config_verified_at", now); err != nil {
				return err
			}
			after["tax_config_verified_by"], after["tax_config_verified_at"] = actor.Username, now
			if err := s.record(ctx, tx, actor, m, "settings.tax_config_verified", "setting", statusKey, before, after); err != nil {
				return err
			}
			return nil
		}
		return s.record(ctx, tx, actor, m, "settings.updated", "setting", strings.Join(keys, ","), before, after)
	})
	if err != nil {
		return nil, err
	}
	return s.GetSettings(ctx, actor)
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

type TaxRateView struct {
	ID        int64  `json:"id"`
	Code      string `json:"code"`
	Name      string `json:"name"`
	RateBP    int32  `json:"rate_bp"`
	Treatment string `json:"treatment"`
	Active    bool   `json:"active"`
}

func (s *Service) ListTaxRates(ctx context.Context, actor *auth.Principal) ([]TaxRateView, error) {
	if actor == nil {
		return nil, ErrForbidden
	}
	rows, err := repository.New(s.pool).ListTaxRates(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]TaxRateView, 0, len(rows))
	for _, r := range rows {
		out = append(out, TaxRateView{ID: r.ID, Code: r.Code, Name: r.Name, RateBP: r.RateBp, Treatment: r.Treatment, Active: r.Active})
	}
	return out, nil
}

func taxView(r repository.GetTaxRateRow) TaxRateView {
	return TaxRateView{ID: r.ID, Code: r.Code, Name: r.Name, RateBP: r.RateBp, Treatment: r.Treatment, Active: r.Active}
}

var taxCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,39}$`)

func checkTax(name string, rateBP int32, treatment string) (string, error) {
	name = strings.TrimSpace(name)
	if n := len([]rune(name)); n < 1 || n > 80 {
		return "", &ValidationError{Field: "name", Msg: "must be 1 to 80 characters"}
	}
	if rateBP < 0 || rateBP > 10000 {
		return "", &ValidationError{Field: "rate_bp", Msg: "must be between 0 and 10000 basis points"}
	}
	if treatment != "standard" && rateBP != 0 {
		return "", &ValidationError{Field: "rate_bp", Msg: "must be 0 for zero_rated and exempt categories"}
	}
	return name, nil
}

// CreateTaxRate adds a category. The treatment (standard, zero_rated, exempt) is fixed at creation (S-41).
func (s *Service) CreateTaxRate(ctx context.Context, actor *auth.Principal, code, name string, rateBP int32, treatment string, m Meta) (TaxRateView, error) {
	if err := s.guard(actor); err != nil {
		return TaxRateView{}, err
	}
	code = strings.TrimSpace(code)
	if !taxCodePattern.MatchString(code) {
		return TaxRateView{}, &ValidationError{Field: "code", Msg: "must be 2 to 40 characters: lower-case letters, digits and underscore, starting with a letter"}
	}
	if _, err := oneOf("standard", "zero_rated", "exempt")(json.RawMessage(mustJSON(treatment))); err != nil {
		return TaxRateView{}, &ValidationError{Field: "treatment", Msg: err.Error()}
	}
	name, err := checkTax(name, rateBP, treatment)
	if err != nil {
		return TaxRateView{}, err
	}
	var out TaxRateView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		id, err := q.CreateTaxRate(ctx, repository.CreateTaxRateParams{Code: code, Name: name, RateBp: rateBP, Treatment: treatment})
		if err != nil {
			return err
		}
		after := map[string]any{"code": code, "name": name, "rate_bp": rateBP, "treatment": treatment}
		if reset, err := unverify(ctx, q, actor); err != nil {
			return err
		} else if reset {
			after["tax_config_status"] = StatusProvisional
		}
		if err := s.record(ctx, tx, actor, m, "tax_rate.created", "tax_rate", itoa(id), nil, after); err != nil {
			return err
		}
		row, err := q.GetTaxRate(ctx, id)
		out = taxView(row)
		return err
	})
	if isUnique(err) {
		return TaxRateView{}, ErrTaxCodeTaken
	}
	return out, err
}

// TaxRateChange holds the fields of a PATCH; nil means unchanged.
type TaxRateChange struct {
	Name   *string
	RateBP *int32
	Active *bool
}

// UpdateTaxRate changes the name, rate or active flag. Changing the rate or the active flag after the tax
// configuration was verified returns it to provisional (S-41).
func (s *Service) UpdateTaxRate(ctx context.Context, actor *auth.Principal, id int64, ch TaxRateChange, m Meta) (TaxRateView, error) {
	if err := s.guard(actor); err != nil {
		return TaxRateView{}, err
	}
	var out TaxRateView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := q.GetTaxRate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTaxRateMissing
		}
		if err != nil {
			return err
		}
		name, rate, active := cur.Name, cur.RateBp, cur.Active
		if ch.Name != nil {
			name = *ch.Name
		}
		if ch.RateBP != nil {
			rate = *ch.RateBP
		}
		if ch.Active != nil {
			active = *ch.Active
		}
		if name, err = checkTax(name, rate, cur.Treatment); err != nil {
			return err
		}
		if name == cur.Name && rate == cur.RateBp && active == cur.Active {
			out = taxView(cur)
			return nil
		}
		if err := q.UpdateTaxRate(ctx, repository.UpdateTaxRateParams{ID: id, Name: name, RateBp: rate, Active: active}); err != nil {
			return err
		}
		before := map[string]any{"name": cur.Name, "rate_bp": cur.RateBp, "active": cur.Active}
		after := map[string]any{"name": name, "rate_bp": rate, "active": active}
		if rate != cur.RateBp || active != cur.Active {
			if reset, err := unverify(ctx, q, actor); err != nil {
				return err
			} else if reset {
				before["tax_config_status"], after["tax_config_status"] = StatusVerified, StatusProvisional
			}
		}
		if err := s.record(ctx, tx, actor, m, "tax_rate.updated", "tax_rate", itoa(id), before, after); err != nil {
			return err
		}
		row, err := q.GetTaxRate(ctx, id)
		out = taxView(row)
		return err
	})
	return out, err
}
