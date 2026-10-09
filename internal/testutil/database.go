package testutil

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Database returns a TEST_DATABASE_URL DSN pointing at a fresh, disposable
// database. go test runs packages in parallel, and two packages migrating one
// shared database race on CREATE TYPE (pg_type 23505) — so every caller gets
// its own database, dropped at test end. Skips when TEST_DATABASE_URL is not
// set; requires a DSN that has a database name and a CREATE DATABASE role.
func Database(t *testing.T) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || len(u.Path) < 2 {
		t.Fatalf("TEST_DATABASE_URL not parseable as a postgres URL: %v", err)
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	db := u.Path[1:] + "_" + hex.EncodeToString(suffix)
	u.Path = "/" + db

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := admin.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %q", db)); err != nil {
		t.Fatalf("create %s: %v", db, err)
	}
	t.Cleanup(func() {
		admin, err := sql.Open("pgx", base)
		if err != nil {
			return
		}
		defer admin.Close()
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = admin.ExecContext(cctx, fmt.Sprintf("DROP DATABASE %q WITH (FORCE)", db))
	})
	return u.String()
}
