package migrate_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"zuscar/backend/internal/platform/migrate"
	"zuscar/backend/internal/platform/testdb"
)

func memFS(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for k, v := range files {
		m["m/"+k] = &fstest.MapFile{Data: []byte(v)}
	}
	return m
}

// AC-13: migrations apply to an empty database, are repeatable as a no-op, and are explicit.
func TestAC13_RealMigrationsApplyToEmptyDatabaseAndRerunIsNoOp(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()
	migs, err := migrate.Load(testdb.MigrationsFS(t), ".")
	if err != nil {
		t.Fatal(err)
	}
	ran, err := migrate.Up(ctx, pool, migs)
	if err != nil || len(ran) != len(migs) {
		t.Fatalf("first run: ran=%v err=%v", ran, err)
	}
	ran, err = migrate.Up(ctx, pool, migs)
	if err != nil || len(ran) != 0 {
		t.Fatalf("second run should be a no-op: ran=%v err=%v", ran, err)
	}
	st, err := migrate.StatusOf(ctx, pool, migs)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range st {
		if !s.Applied {
			t.Errorf("migration %04d_%s not applied", s.Version, s.Name)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('users','roles','permissions','user_roles','role_permissions','sessions','user_pins','login_attempts','audit_logs','system_settings','tax_rates','document_counters')`).Scan(&n); err != nil || n != 12 {
		t.Fatalf("expected 12 Stage 5 tables, got %d (%v)", n, err)
	}
}

func TestEditedAppliedMigrationIsRefused(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()
	v1, _ := migrate.Load(memFS(map[string]string{"0001_a.sql": "CREATE TABLE a (id int);"}), "m")
	if _, err := migrate.Up(ctx, pool, v1); err != nil {
		t.Fatal(err)
	}
	edited, _ := migrate.Load(memFS(map[string]string{"0001_a.sql": "CREATE TABLE a (id bigint);"}), "m")
	if _, err := migrate.Up(ctx, pool, edited); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("edited migration must be refused, got %v", err)
	}
}

func TestFailedMigrationRollsBackCompletely(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()
	migs, _ := migrate.Load(memFS(map[string]string{"0001_ok.sql": "CREATE TABLE ok (id int);", "0002_bad.sql": "CREATE TABLE half (id int); SELECT 1/0;"}), "m")
	ran, err := migrate.Up(ctx, pool, migs)
	if err == nil || len(ran) != 1 {
		t.Fatalf("want failure after one success: ran=%v err=%v", ran, err)
	}
	var exists bool
	_ = pool.QueryRow(ctx, `SELECT to_regclass('half') IS NOT NULL`).Scan(&exists)
	if exists {
		t.Fatal("half-applied migration left a table behind")
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n)
	if n != 1 {
		t.Fatalf("only the first migration should be recorded, got %d", n)
	}
}

func TestConcurrentRunnersApplyEachMigrationOnce(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()
	migs, _ := migrate.Load(testdb.MigrationsFS(t), ".")
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = migrate.Up(ctx, pool, migs)
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Errorf("runner %d: %v", i, e)
		}
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n)
	if n != len(migs) {
		t.Fatalf("schema_migrations has %d rows, want %d", n, len(migs))
	}
}
