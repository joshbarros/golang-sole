// Package testutil gives integration tests a real, migrated Postgres. Point TEST_DATABASE_URL at a server; the tests create
// and use the database named in it (sole_test by default) and never touch any other.
package testutil

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-sole/internal/claims"
	"github.com/joshbarros/golang-sole/internal/identity"
	"github.com/joshbarros/golang-sole/internal/orders"
	"github.com/joshbarros/golang-sole/internal/platform/pgdb"
	"github.com/joshbarros/golang-sole/internal/raffles"
)

const defaultURL = "postgres://sole:sole@localhost:5488/sole_test?sslmode=disable"

var (
	once sync.Once
	pool *pgxpool.Pool
	err  error
)

// Pool returns the shared test pool, creating the database and applying every module's migrations on first use.
func Pool(tb testing.TB) *pgxpool.Pool {
	tb.Helper()
	once.Do(func() { pool, err = open() })
	if err != nil {
		tb.Fatalf("test database: %v (is Postgres up? try: make deps)", err)
	}
	return pool
}

func open() (*pgxpool.Pool, error) {
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		raw = defaultURL
	}
	u, perr := url.Parse(raw)
	if perr != nil {
		return nil, perr
	}
	name := u.Path[1:]
	admin := *u
	admin.Path = "/postgres"
	ctx := context.Background()
	conn, cerr := pgx.Connect(ctx, admin.String())
	if cerr != nil {
		return nil, cerr
	}
	var exists bool
	if qerr := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); qerr != nil {
		_ = conn.Close(ctx)
		return nil, qerr
	}
	if !exists {
		if _, xerr := conn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, pgx.Identifier{name}.Sanitize())); xerr != nil {
			_ = conn.Close(ctx)
			return nil, xerr
		}
	}
	_ = conn.Close(ctx)

	p, oerr := pgdb.Open(ctx, raw, slog.New(slog.DiscardHandler))
	if oerr != nil {
		return nil, oerr
	}
	for schema, files := range map[string]func() error{
		"identity": func() error { return pgdb.Migrate(ctx, p, "identity", identity.Migrations, "migrations") },
		"orders":   func() error { return pgdb.Migrate(ctx, p, "orders", orders.Migrations, "migrations") },
		"raffles":  func() error { return pgdb.Migrate(ctx, p, "raffles", raffles.Migrations, "migrations") },
		"claims":   func() error { return pgdb.Migrate(ctx, p, "claims", claims.Migrations, "migrations") },
	} {
		if merr := files(); merr != nil {
			return nil, fmt.Errorf("migrate %s: %w", schema, merr)
		}
	}
	return p, nil
}

// Reset empties the business tables (orders, raffles, claims and their outboxes). The identity schema is left alone: the
// API tests share it, and every test registers its own users with unique emails.
func Reset(tb testing.TB) *pgxpool.Pool {
	tb.Helper()
	p := Pool(tb)
	_, e := p.Exec(context.Background(), `TRUNCATE
		orders.orders, orders.outbox_messages, orders.inbox_messages,
		raffles.pair_holds, raffles.raffle_entries, raffles.raffles, raffles.outbox_messages, raffles.inbox_messages,
		claims.claims, claims.outbox_messages, claims.inbox_messages RESTART IDENTITY CASCADE`)
	if e != nil {
		tb.Fatalf("reset: %v", e)
	}
	return p
}
