package devseed_test

import (
	"context"
	"testing"

	"zuscar/backend/internal/devseed"
	"zuscar/backend/internal/identity/password"
	"zuscar/backend/internal/platform/config"
	"zuscar/backend/internal/platform/testdb"
	"zuscar/backend/internal/provision"
)

func TestSeedCreatesFakeUsersOnlyAndIsRepeatable(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	created, err := devseed.Run(ctx, pool, config.EnvDevelopment)
	if err != nil || created != len(devseed.Users) {
		t.Fatalf("created=%d err=%v", created, err)
	}
	var total, nonDev, mustChange int
	_ = pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE username NOT LIKE 'dev.%'), count(*) FILTER (WHERE must_change_password) FROM users`).Scan(&total, &nonDev, &mustChange)
	if total != 9 || nonDev != 0 || mustChange != 0 {
		t.Fatalf("total=%d nonDev=%d mustChange=%d", total, nonDev, mustChange)
	}
	var hash string
	_ = pool.QueryRow(ctx, `SELECT password_hash FROM users WHERE username = 'dev.cashier'`).Scan(&hash)
	if password.Verify(devseed.Password, hash) != nil {
		t.Fatal("dev password should verify for seeded users")
	}
	var ownerRoles int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM user_roles ur JOIN users u ON u.id = ur.user_id WHERE u.username = 'dev.owner'`).Scan(&ownerRoles)
	if ownerRoles != 2 {
		t.Fatalf("dev.owner roles = %d, want 2 (Super Admin + Business Manager)", ownerRoles)
	}
	again, err := devseed.Run(ctx, pool, config.EnvDevelopment)
	if err != nil || again != 0 {
		t.Fatalf("second run created=%d err=%v", again, err)
	}
}

// Defence in depth for a forgotten APP_ENV: a database holding real users is never seeded.
func TestSeedRefusesDatabaseWithRealUsers(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	if _, err := provision.Bootstrap(ctx, pool, provision.BootstrapOptions{Username: "owner.real", DisplayName: "Real Owner", Password: "correct-horse-battery-9",
		BaseOptions: provision.BaseOptions{BusinessName: "Real Venue", TaxRateBP: 1600, Timezone: "Africa/Nairobi", ReceiptStart: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := devseed.Run(ctx, pool, config.EnvDevelopment); err != devseed.ErrRealUsers {
		t.Fatalf("want ErrRealUsers, got %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	if n != 1 {
		t.Fatalf("users = %d: seed must not have added anyone", n)
	}
}

// Go-live clean-up: a seeded (test) database can never become production by bootstrapping on top of it.
func TestBootstrapRefusesSeededDatabase(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	if _, err := devseed.Run(ctx, pool, config.EnvTest); err != nil {
		t.Fatal(err)
	}
	_, err := provision.Bootstrap(ctx, pool, provision.BootstrapOptions{Username: "owner.real", DisplayName: "Real Owner", Password: "correct-horse-battery-9",
		BaseOptions: provision.BaseOptions{BusinessName: "Real Venue", TaxRateBP: 1600, Timezone: "Africa/Nairobi", ReceiptStart: 1}})
	if err != provision.ErrAlreadyBootstrapped {
		t.Fatalf("want ErrAlreadyBootstrapped, got %v", err)
	}
}
