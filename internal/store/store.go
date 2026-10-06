// Package store holds explicit, reviewable SQL for all domains. One package,
// one file per domain; services call store methods directly.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound wraps pgx.ErrNoRows; callers map it to 404.
var ErrNotFound = pgx.ErrNoRows

// Q is implemented by both *pgxpool.Pool and pgx.Tx, letting every query run
// either on the pool or inside a transaction.
type Q interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store carries the execution context for queries.
type Store struct {
	Q Q
}

func New(pool *pgxpool.Pool) *Store { return &Store{Q: pool} }

// Ping exposes pool liveness for readiness checks.
func (s *Store) Ping(ctx context.Context) error {
	return s.Q.(pgxping).Ping(ctx)
}

type pgxping interface {
	Ping(ctx context.Context) error
}

// WithTx runs fn against a transaction-bound Store. Nested WithTx reuses the
// outer transaction instead of creating one.
func (s *Store) WithTx(ctx context.Context, fn func(tx *Store) error) error {
	pool, ok := s.Q.(*pgxpool.Pool)
	if !ok {
		return fn(s) // already inside a transaction
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(&Store{Q: tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ErrUnique wraps a unique-constraint violation; callers map it to 409.
type ErrUnique struct{ Constraint string }

func (e ErrUnique) Error() string { return fmt.Sprintf("constraint %s: duplicate", e.Constraint) }

// IsUnique classifies pg unique violation codes.
func IsUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
