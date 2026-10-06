// Package cli is the admin surface (docs/CLI.md): same service layer as HTTP.
package cli

import (
	"context"
	"database/sql"
	"log/slog"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx stdlib driver for goose
	"github.com/spf13/cobra"

	migrations "github.com/gochathub/gochathub-server/db"
	"github.com/gochathub/gochathub-server/internal/config"
	pgdb "github.com/gochathub/gochathub-server/internal/db"
	"github.com/gochathub/gochathub-server/internal/store"
)

var Version = "dev"

// boot bundles everything commands need.
type boot struct {
	cfg *config.Config
	log *slog.Logger
	q   *store.Store
	db  *sql.DB // stdlib handle for migrations
}

func bootstrap() (*boot, func(), error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	log := logger(cfg.LogLevel)
	pool, err := pgdb.NewPool(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return nil, nil, err
	}
	sqlDB, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	b := &boot{cfg: cfg, log: log, q: store.New(pool), db: sqlDB}
	return b, func() { sqlDB.Close(); pool.Close() }, nil
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "chat-server",
		Short:        "Self-hosted chat server: API, WebSocket, and CLI administration",
		SilenceUsage: true,
	}
	root.AddCommand(serveCmd(), migrateCmd(), versionCmd(), userCmd(), roomCmd(), tokenCmd())
	return root
}

// Execute is the binary entrypoint.
func Execute() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func logger(level string) *slog.Logger {
	lvl := slog.LevelInfo
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// migrateUp applies embedded up-migrations (MIGRATIONS_ON_SERVE opt-in).
func migrateUp(ctx context.Context, b *boot) error {
	return migrations.Migrate(ctx, b.db)
}
