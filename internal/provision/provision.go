// Package provision creates the minimum data a fresh database needs: permissions, role templates,
// system settings, the tax rate and document counters, plus (bootstrap only) the first Super Admin.
// Nothing else: real menus, rooms, stock and staff are entered through the application (prompt section 8).
package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // the container image may not ship zoneinfo

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"zuscar/backend/internal/identity/catalog"
	"zuscar/backend/internal/identity/password"
	idrepo "zuscar/backend/internal/identity/repository"
	setrepo "zuscar/backend/internal/settings/repository"
)

// KnownDevPassword is the documented dev-only seed password. Bootstrap refuses it, so a production
// Super Admin can never be created with the password that every developer knows (go-live checklist).
const KnownDevPassword = "DevOnly-Zuscar-2026"

var (
	ErrAlreadyBootstrapped = errors.New("the database already has users: bootstrap creates the first Super Admin on an empty system only")
	usernameRE             = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,39}$`)
	counterTypes           = []string{"order", "folio", "purchase", "reservation", "refund", "transfer", "count"}
)

type BaseOptions struct {
	BusinessName string
	TaxName      string
	TaxRateBP    int // basis points, 0..10000; supplied by the operator, never defaulted (accountant confirms)
	Timezone     string
	ReceiptStart int64 // first receipt number: continue the venue's receipt book
}

func (o BaseOptions) validate() error {
	var errs []error
	if strings.TrimSpace(o.BusinessName) == "" {
		errs = append(errs, errors.New("business name is required"))
	}
	if o.TaxRateBP < 0 || o.TaxRateBP > 10000 {
		errs = append(errs, errors.New("tax rate must be between 0 and 10000 basis points"))
	}
	if _, err := time.LoadLocation(o.Timezone); err != nil || o.Timezone == "" {
		errs = append(errs, fmt.Errorf("unknown timezone %q", o.Timezone))
	}
	if o.ReceiptStart < 1 {
		errs = append(errs, errors.New("receipt start number must be at least 1"))
	}
	return errors.Join(errs...)
}

// ApplyBaseData is idempotent: re-running it never resets a counter or removes a grant.
func ApplyBaseData(ctx context.Context, tx pgx.Tx, o BaseOptions) error {
	if err := o.validate(); err != nil {
		return err
	}
	ir, sr := idrepo.New(tx), setrepo.New(tx)

	permIDs := map[string]int64{}
	for _, p := range catalog.Permissions {
		id, err := ir.UpsertPermission(ctx, idrepo.UpsertPermissionParams{Key: p.Key, Description: p.Description, IsProtected: p.Protected})
		if err != nil {
			return fmt.Errorf("permission %s: %w", p.Key, err)
		}
		permIDs[p.Key] = id
	}
	for _, r := range catalog.Roles {
		rid, err := ir.UpsertRole(ctx, idrepo.UpsertRoleParams{Code: r.Code, Name: r.Name, Description: r.Description, IsProtected: r.Protected})
		if err != nil {
			return fmt.Errorf("role %s: %w", r.Code, err)
		}
		for _, k := range r.Permissions {
			if err := ir.GrantRolePermission(ctx, idrepo.GrantRolePermissionParams{RoleID: rid, PermissionID: permIDs[k]}); err != nil {
				return fmt.Errorf("grant %s to %s: %w", k, r.Code, err)
			}
		}
	}

	settings := map[string]any{
		"business_name": strings.TrimSpace(o.BusinessName), "timezone": o.Timezone, "currency": "KES",
		"tax_inclusive": true, "negative_stock_policy": "warn_override",
		"checkin_time": "14:00", "checkout_time": "11:00", "receipt_footer": "",
	}
	for k, v := range settings {
		b, _ := json.Marshal(v)
		if err := sr.UpsertSetting(ctx, setrepo.UpsertSettingParams{Key: k, Value: b}); err != nil {
			return fmt.Errorf("setting %s: %w", k, err)
		}
	}
	name := strings.TrimSpace(o.TaxName)
	if name == "" {
		name = "Standard rate"
	}
	if _, err := sr.UpsertTaxRate(ctx, setrepo.UpsertTaxRateParams{Code: "standard", Name: name, RateBp: int32(o.TaxRateBP)}); err != nil {
		return fmt.Errorf("tax rate: %w", err)
	}
	if err := sr.CreateCounterIfMissing(ctx, setrepo.CreateCounterIfMissingParams{DocType: "receipt", NextValue: o.ReceiptStart}); err != nil {
		return err
	}
	for _, d := range counterTypes {
		if err := sr.CreateCounterIfMissing(ctx, setrepo.CreateCounterIfMissingParams{DocType: d, NextValue: 1}); err != nil {
			return err
		}
	}
	return nil
}

type BootstrapOptions struct {
	BaseOptions
	Username    string
	DisplayName string
	Password    string
}

// Bootstrap creates base data and the first Super Admin in one transaction. The password is supplied at run
// time by the operator; the user must change it at first login.
func Bootstrap(ctx context.Context, pool *pgxpool.Pool, o BootstrapOptions) (int64, error) {
	if !usernameRE.MatchString(o.Username) {
		return 0, errors.New("username must be 3 to 40 characters: lowercase letters, digits, dot, underscore or hyphen")
	}
	if strings.TrimSpace(o.DisplayName) == "" {
		return 0, errors.New("display name is required")
	}
	if err := o.BaseOptions.validate(); err != nil {
		return 0, err
	}
	if err := password.Policy(o.Password, o.Username, KnownDevPassword); err != nil {
		return 0, err
	}
	hash, err := password.Hash(o.Password)
	if err != nil {
		return 0, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ir := idrepo.New(tx)
	n, err := ir.CountUsers(ctx)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		return 0, ErrAlreadyBootstrapped
	}
	if err := ApplyBaseData(ctx, tx, o.BaseOptions); err != nil {
		return 0, err
	}
	uid, err := ir.CreateUser(ctx, idrepo.CreateUserParams{Username: o.Username, DisplayName: strings.TrimSpace(o.DisplayName), PasswordHash: hash, MustChangePassword: true})
	if err != nil {
		return 0, err
	}
	rid, err := ir.GetRoleIDByCode(ctx, "super_admin")
	if err != nil {
		return 0, err
	}
	if err := ir.AssignRole(ctx, idrepo.AssignRoleParams{UserID: uid, RoleID: rid}); err != nil {
		return 0, err
	}
	after, _ := json.Marshal(map[string]any{"username": o.Username, "role": "super_admin", "must_change_password": true})
	if err := ir.InsertAudit(ctx, idrepo.InsertAuditParams{
		ActorLabel: "cli:bootstrap", Action: "bootstrap.super_admin_created", EntityType: "user",
		EntityID: pgtype.Text{String: fmt.Sprint(uid), Valid: true}, AfterJson: after,
	}); err != nil {
		return 0, err
	}
	return uid, tx.Commit(ctx)
}
