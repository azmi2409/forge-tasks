package store

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MigrationSQL is the embedded component-1 schema (migrations/001_init.sql).
//
//go:embed migrations/001_init.sql
var MigrationSQL string

// ApplyMigration runs the embedded schema against a fresh database. The
// migration is idempotent (IF NOT EXISTS / OR REPLACE throughout), so it is
// safe to run on every boot.
func ApplyMigration(ctx context.Context, databaseURL string) error {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("store: connect for migration: %w", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, MigrationSQL); err != nil {
		return fmt.Errorf("store: apply migration: %w", err)
	}
	return nil
}
