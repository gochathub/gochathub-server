// Package db carries the embedded SQL migrations and exposes the migration
// runner. Migrations never run implicitly at server startup: apply them via
// `chat-server migrate`, or opt in explicitly with MIGRATIONS_ON_SERVE=1.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies all pending up-migrations to the given database handle.
func Migrate(ctx context.Context, conn *sql.DB) error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}
	goose.SetVerbose(false)
	if err := goose.UpContext(ctx, conn, "migrations"); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}
