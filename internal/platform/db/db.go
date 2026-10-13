// Package db opens the PostgreSQL pool. Only the Go API connects to the database (ADR-02).
// Migrations are never run here: they are an explicit operator command (AC-13).
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// New builds a pool. It does not dial eagerly, so the process can start and /readyz can
// report a database outage instead of the container crash-looping.
func New(ctx context.Context, url string, simpleProtocol bool) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", redact(err))
	}
	if simpleProtocol {
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// redact drops the original error text, which for URL parse failures can echo the URL
// (and so the password).
func redact(error) error { return fmt.Errorf("invalid connection string") }
