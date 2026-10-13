package identity_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zuscar/backend/internal/platform/testdb"
	"zuscar/backend/internal/provision"
)

func seeded(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := provision.ApplyBaseData(ctx, tx, provision.BaseOptions{BusinessName: "Test Venue", TaxRateBP: 1600, Timezone: "Africa/Nairobi", ReceiptStart: 1}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return pool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func wantFail(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q, got none", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("expected error containing %q, got: %v", substr, err)
	}
}

func addUser(t *testing.T, pool *pgxpool.Pool, name, role string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO users (username, display_name, password_hash) VALUES ($1, $1, 'x') RETURNING id`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `INSERT INTO user_roles (user_id, role_id) SELECT $1, id FROM roles WHERE code = $2`, id, role)
	return id
}

// NN-08 / DQ-18: audit_logs is append-only.
func TestNN08_AuditLogsRejectUpdateDeleteTruncate(t *testing.T) {
	pool := seeded(t)
	ctx := context.Background()
	mustExec(t, pool, `INSERT INTO audit_logs (actor_label, action, entity_type) VALUES ('test', 'x.y', 'thing')`)
	_, err := pool.Exec(ctx, `UPDATE audit_logs SET action = 'tampered'`)
	wantFail(t, err, "append-only")
	_, err = pool.Exec(ctx, `DELETE FROM audit_logs`)
	wantFail(t, err, "append-only")
	_, err = pool.Exec(ctx, `TRUNCATE audit_logs`)
	wantFail(t, err, "append-only")
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs`).Scan(&n)
	if n != 1 {
		t.Fatalf("audit row count = %d, want 1", n)
	}
}

// AC-11: the database refuses system.admin on a non-protected role even if the service has a bug.
func TestAC11_DBRejectsSystemAdminOnNonProtectedRole(t *testing.T) {
	pool := seeded(t)
	_, err := pool.Exec(context.Background(), `INSERT INTO role_permissions (role_id, permission_id)
		SELECT r.id, p.id FROM roles r, permissions p WHERE r.code = 'business_manager' AND p.key = 'system.admin'`)
	wantFail(t, err, "protected permission can only be held by a protected role")
	var holders int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM role_permissions rp JOIN permissions p ON p.id = rp.permission_id AND p.key = 'system.admin'`).Scan(&holders)
	if holders != 1 {
		t.Fatalf("system.admin should be held by exactly one role (super_admin), found %d", holders)
	}
	// a brand-new custom role is not protected either
	mustExec(t, pool, `INSERT INTO roles (code, name) VALUES ('custom_role', 'Custom')`)
	_, err = pool.Exec(context.Background(), `INSERT INTO role_permissions (role_id, permission_id)
		SELECT r.id, p.id FROM roles r, permissions p WHERE r.code = 'custom_role' AND p.key = 'system.admin'`)
	wantFail(t, err, "protected permission")
}

func TestAC11_BusinessManagerTemplateHasNoRoleAdministration(t *testing.T) {
	pool := seeded(t)
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM role_permissions rp
		JOIN roles r ON r.id = rp.role_id AND r.code = 'business_manager'
		JOIN permissions p ON p.id = rp.permission_id AND p.key IN ('roles.manage', 'permissions.assign', 'system.admin')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("business_manager holds %d role-administration permissions (%v)", n, err)
	}
}

func TestProtectedRoleCannotBeDeletedRenamedOrUnprotected(t *testing.T) {
	pool := seeded(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `DELETE FROM roles WHERE code = 'super_admin'`)
	wantFail(t, err, "cannot be deleted")
	_, err = pool.Exec(ctx, `UPDATE roles SET code = 'owner' WHERE code = 'super_admin'`)
	wantFail(t, err, "cannot be renamed")
	_, err = pool.Exec(ctx, `UPDATE roles SET is_protected = false WHERE code = 'super_admin'`)
	wantFail(t, err, "unprotected")
	mustExec(t, pool, `UPDATE roles SET description = 'edited text is fine' WHERE code = 'super_admin'`)
}

func TestAssignedRoleCannotBeDeleted(t *testing.T) {
	pool := seeded(t)
	addUser(t, pool, "dev.cashier1", "cashier")
	_, err := pool.Exec(context.Background(), `DELETE FROM roles WHERE code = 'cashier'`)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23503" {
		t.Fatalf("want foreign key violation 23503, got %v", err)
	}
}

// AC-12: the last active Super Admin cannot be removed by any route.
func TestAC12_LastSuperAdminCannotBeRemoved(t *testing.T) {
	pool := seeded(t)
	ctx := context.Background()
	a := addUser(t, pool, "owner.a", "super_admin")
	b := addUser(t, pool, "owner.b", "super_admin")
	addUser(t, pool, "dev.cashier1", "cashier")

	mustExec(t, pool, `UPDATE users SET status = 'inactive', deactivated_at = now() WHERE id = $1`, a) // one of two: allowed
	_, err := pool.Exec(ctx, `UPDATE users SET status = 'inactive', deactivated_at = now() WHERE id = $1`, b)
	wantFail(t, err, "no active Super Admin")
	_, err = pool.Exec(ctx, `DELETE FROM user_roles WHERE user_id = $1`, b)
	wantFail(t, err, "no active Super Admin")
	_, err = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, b)
	if err == nil {
		t.Fatal("deleting the last Super Admin must fail")
	}
	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, b).Scan(&status)
	if status != "active" {
		t.Fatalf("last Super Admin status = %q, want active", status)
	}
	// reactivating the first gives two again, so removing one is allowed again
	mustExec(t, pool, `UPDATE users SET status = 'active', deactivated_at = NULL WHERE id = $1`, a)
	mustExec(t, pool, `UPDATE users SET status = 'inactive', deactivated_at = now() WHERE id = $1`, b)
}

// AC-12: two admins deactivating each other at the same moment must not both succeed.
func TestAC12_ConcurrentDeactivationLeavesOne(t *testing.T) {
	pool := seeded(t)
	ctx := context.Background()
	a := addUser(t, pool, "owner.a", "super_admin")
	b := addUser(t, pool, "owner.b", "super_admin")

	txA, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback(ctx)
	if _, err := txA.Exec(ctx, `UPDATE users SET status = 'inactive', deactivated_at = now() WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := pool.Exec(ctx, `UPDATE users SET status = 'inactive', deactivated_at = now() WHERE id = $1`, b)
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("second deactivation should wait for the first transaction, but returned: %v", err)
	case <-time.After(500 * time.Millisecond): // blocked on the advisory lock, as designed
	}
	if err := txA.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	wantFail(t, <-result, "no active Super Admin")
	var active int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE status = 'active'`).Scan(&active)
	if active != 1 {
		t.Fatalf("exactly one Super Admin must remain active, have %d", active)
	}
}

// AC-12: deactivation keeps history; users with history cannot be deleted.
func TestAC12_DeactivationKeepsHistory(t *testing.T) {
	pool := seeded(t)
	ctx := context.Background()
	addUser(t, pool, "owner.a", "super_admin")
	c := addUser(t, pool, "dev.cashier1", "cashier")
	mustExec(t, pool, `INSERT INTO audit_logs (actor_user_id, actor_label, action, entity_type) VALUES ($1, 'dev.cashier1', 'sale.create', 'sale')`, c)
	mustExec(t, pool, `UPDATE users SET status = 'inactive', deactivated_at = now() WHERE id = $1`, c)
	mustExec(t, pool, `DELETE FROM user_roles WHERE user_id = $1`, c)
	_, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, c)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23503" {
		t.Fatalf("deleting a user with audit history must hit the foreign key (23503), got %v", err)
	}
}

func TestUserTableRules(t *testing.T) {
	pool := seeded(t)
	ctx := context.Background()
	ins := func(name string) error {
		_, err := pool.Exec(ctx, `INSERT INTO users (username, display_name, password_hash) VALUES ($1, 'N', 'x')`, name)
		return err
	}
	wantFail(t, ins("Upper.Case"), "users_username_check")
	wantFail(t, ins("ab"), "users_username_check")
	wantFail(t, ins("has space"), "users_username_check")
	if err := ins("fine.name"); err != nil {
		t.Fatal(err)
	}
	wantFail(t, ins("fine.name"), "users_username_key")
	_, err := pool.Exec(ctx, `UPDATE users SET status = 'inactive' WHERE username = 'fine.name'`)
	wantFail(t, err, "users_deactivated_consistent")
}

func TestSessionTokenHashRules(t *testing.T) {
	pool := seeded(t)
	ctx := context.Background()
	u := addUser(t, pool, "dev.cashier1", "cashier")
	ins := func(hash []byte) error {
		_, err := pool.Exec(ctx, `INSERT INTO sessions (user_id, token_hash, idle_expires_at, absolute_expires_at) VALUES ($1, $2, now() + interval '1 hour', now() + interval '2 hours')`, u, hash)
		return err
	}
	wantFail(t, ins(make([]byte, 31)), "sessions_token_hash_check")
	h := make([]byte, 32)
	h[0] = 1
	if err := ins(h); err != nil {
		t.Fatal(err)
	}
	wantFail(t, ins(h), "sessions_token_hash_key")
}

var _ = pgx.ErrNoRows
