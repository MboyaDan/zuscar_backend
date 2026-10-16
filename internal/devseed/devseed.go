// Package devseed creates fictional development and test data. It is imported only by cmd/seed-dev
// (enforced by archtest) and refuses to run in production (prompt section 8).
package devseed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"zuscar/backend/internal/identity/password"
	idrepo "zuscar/backend/internal/identity/repository"
	"zuscar/backend/internal/platform/config"
	"zuscar/backend/internal/provision"
)

// Password is the documented dev-only password for every seeded user. It is public on purpose.
const Password = provision.KnownDevPassword

var (
	ErrProduction = errors.New("seed-dev refuses to run when APP_ENV=production")
	ErrRealUsers  = errors.New("seed-dev refuses to run: this database contains users that are not dev.* accounts, so it may be a real database")
)

type devUser struct {
	username, display string
	roles             []string
}

// Users are obviously fake. Names never come from real people (prompt section 8).
var Users = []devUser{
	{"dev.owner", "Dev Owner (fake)", []string{"super_admin", "business_manager"}},
	{"dev.manager", "Dev Manager (fake)", []string{"business_manager"}},
	{"dev.cashier", "Dev Cashier (fake)", []string{"cashier"}},
	{"dev.waiter", "Dev Waiter (fake)", []string{"waiter"}},
	{"dev.bartender", "Dev Bartender (fake)", []string{"bartender"}},
	{"dev.reception", "Dev Receptionist (fake)", []string{"receptionist"}},
	{"dev.store", "Dev Storekeeper (fake)", []string{"storekeeper"}},
	{"dev.kitchen", "Dev Kitchen (fake)", []string{"kitchen_staff"}},
	{"dev.auditor", "Dev Auditor (fake)", []string{"accountant_auditor"}},
}

// Guard is checked first and also covered by a test that needs no database.
func Guard(env config.Env) error {
	if env == config.EnvProduction {
		return ErrProduction
	}
	if env != config.EnvDevelopment && env != config.EnvTest {
		return fmt.Errorf("seed-dev needs APP_ENV=development or test, got %q", env)
	}
	return nil
}

func Run(ctx context.Context, pool *pgxpool.Pool, env config.Env) (created int, err error) {
	if err := Guard(env); err != nil {
		return 0, err
	}
	hash, err := password.Hash(Password)
	if err != nil {
		return 0, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ir := idrepo.New(tx)
	if others, err := ir.ListUsernamesNotDev(ctx); err != nil {
		return 0, err
	} else if len(others) > 0 {
		return 0, ErrRealUsers
	}
	// DEV ONLY tax value so the sample data has a rate; production values come from the accountant.
	if err := provision.ApplyBaseData(ctx, tx, provision.BaseOptions{BusinessName: "Zuscar Dev Venue (fake)", TaxName: "Dev VAT (fake)", TaxRateBP: provision.ProvisionalStandardRateBP, Timezone: "Africa/Nairobi", ReceiptStart: 1}); err != nil {
		return 0, err
	}
	for _, u := range Users {
		if _, err := ir.GetUserByUsername(ctx, u.username); err == nil {
			continue
		}
		uid, err := ir.CreateUser(ctx, idrepo.CreateUserParams{Username: u.username, DisplayName: u.display, PasswordHash: hash})
		if err != nil {
			return 0, err
		}
		for _, code := range u.roles {
			rid, err := ir.GetRoleIDByCode(ctx, code)
			if err != nil {
				return 0, err
			}
			if err := ir.AssignRole(ctx, idrepo.AssignRoleParams{UserID: uid, RoleID: rid}); err != nil {
				return 0, err
			}
		}
		after, _ := json.Marshal(map[string]any{"username": u.username, "roles": u.roles})
		if err := ir.InsertAudit(ctx, idrepo.InsertAuditParams{ActorLabel: "cli:seed-dev", Action: "devseed.user_created", EntityType: "user", EntityID: pgtype.Text{String: fmt.Sprint(uid), Valid: true}, AfterJson: after}); err != nil {
			return 0, err
		}
		created++
	}
	if _, err := seedMenu(ctx, tx, ir); err != nil {
		return 0, err
	}
	return created, tx.Commit(ctx)
}
