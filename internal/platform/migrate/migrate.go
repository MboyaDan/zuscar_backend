// Package migrate is a small repo-owned SQL migration runner (ADR-12).
//
// Rules: files are named NNNN_description.sql and applied in numeric order, each in its own
// transaction; applied files are never edited (their SHA-256 is recorded and checked); there are
// no down migrations (forward-fix only); a PostgreSQL advisory lock stops two runners racing.
// It is only ever run by an explicit operator command, never on API start (AC-13).
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const lockKey int64 = 724002

var fileName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

type Migration struct {
	Version  int
	Name     string
	Checksum string
	SQL      string
}

// Load reads and validates migration files from dir inside fsys.
func Load(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := fileName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration file %q must be named NNNN_description.sql", e.Name())
		}
		body, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		v, _ := strconv.Atoi(m[1])
		sum := sha256.Sum256(body)
		out = append(out, Migration{Version: v, Name: m[2], Checksum: hex.EncodeToString(sum[:]), SQL: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i, m := range out {
		if m.Version != i+1 {
			return nil, fmt.Errorf("migration versions must run 1,2,3 without gaps or duplicates; found %04d at position %d", m.Version, i+1)
		}
	}
	return out, nil
}

type Applied struct {
	Version  int
	Name     string
	Checksum string
}

const ensureTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT        NOT NULL,
  checksum   TEXT        NOT NULL,
  applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`

func applied(ctx context.Context, c *pgxpool.Conn) ([]Applied, error) {
	if _, err := c.Exec(ctx, ensureTable); err != nil {
		return nil, err
	}
	rows, err := c.Query(ctx, `SELECT version, name, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Applied
	for rows.Next() {
		var a Applied
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Up applies all pending migrations and returns the versions it applied.
func Up(ctx context.Context, pool *pgxpool.Pool, migs []Migration) ([]int, error) {
	c, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Release()
	if _, err := c.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		return nil, err
	}
	defer func() { _, _ = c.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey) }()

	done, err := applied(ctx, c)
	if err != nil {
		return nil, err
	}
	if len(done) > len(migs) {
		return nil, fmt.Errorf("database has %d migrations applied but only %d files exist: refusing to continue", len(done), len(migs))
	}
	for i, a := range done {
		if migs[i].Version != a.Version || migs[i].Checksum != a.Checksum {
			return nil, fmt.Errorf("migration %04d_%s was changed after being applied (checksum mismatch): never edit an applied migration, add a new one", a.Version, a.Name)
		}
	}
	var ran []int
	for _, m := range migs[len(done):] {
		if err := apply(ctx, c, m); err != nil {
			return ran, fmt.Errorf("migration %04d_%s failed: %w", m.Version, m.Name, err)
		}
		ran = append(ran, m.Version)
	}
	return ran, nil
}

func apply(ctx context.Context, c *pgxpool.Conn, m Migration) error {
	tx, err := c.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, m.SQL, pgx.QueryExecModeSimpleProtocol); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`, m.Version, m.Name, m.Checksum); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type Status struct {
	Version int
	Name    string
	Applied bool
}

// StatusOf reports each migration file and whether it is applied. It never changes the database
// except for creating the schema_migrations table if absent.
func StatusOf(ctx context.Context, pool *pgxpool.Pool, migs []Migration) ([]Status, error) {
	c, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Release()
	done, err := applied(ctx, c)
	if err != nil {
		return nil, err
	}
	have := map[int]bool{}
	for _, a := range done {
		have[a.Version] = true
	}
	var out []Status
	for _, m := range migs {
		out = append(out, Status{m.Version, m.Name, have[m.Version]})
	}
	if len(done) > len(migs) {
		return out, errors.New("database has more applied migrations than files")
	}
	return out, nil
}
