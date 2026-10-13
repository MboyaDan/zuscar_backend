// Package testdb gives each database test its own freshly migrated throwaway database.
// Tests skip LOUDLY when TEST_DATABASE_URL is unset, so a green run without a database is never mistaken
// for a verified run (honesty rules). Point TEST_DATABASE_URL at a server where the role may CREATE DATABASE.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zuscar/backend/internal/platform/migrate"
)

const skipMessage = "TEST_DATABASE_URL is not set: DATABASE TESTS SKIPPED, NOTHING WAS VERIFIED. Run: make test-db (or docker compose run --rm test)"

// NewEmpty returns a pool on a new empty database that is dropped when the test ends.
func NewEmpty(t testing.TB) *pgxpool.Pool {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip(skipMessage)
	}
	if os.Getenv("APP_ENV") == "production" {
		t.Fatal("refusing to run database tests with APP_ENV=production")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatalf("connect TEST_DATABASE_URL: %v", err)
	}
	defer admin.Close(ctx)
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "zuscar_test_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if a, err := pgx.Connect(c, raw); err == nil {
			_, _ = a.Exec(c, `DROP DATABASE IF EXISTS `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`)
			a.Close(c)
		}
	})
	return pool
}

// New returns a pool on a new database with every migration applied.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool := NewEmpty(t)
	migs, err := migrate.Load(MigrationsFS(t), ".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Up(context.Background(), pool, migs); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return pool
}

// MigrationsFS finds database/migrations by walking up from the test's working directory.
func MigrationsFS(t testing.TB) fs.FS {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		cand := filepath.Join(dir, "database", "migrations")
		if st, err := os.Stat(cand); err == nil && st.IsDir() {
			return os.DirFS(cand)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("database/migrations not found above the test directory")
		}
		dir = parent
	}
}
