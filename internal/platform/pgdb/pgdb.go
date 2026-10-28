// Package pgdb opens the Postgres pool and runs each module's embedded migrations.
package pgdb

import (
	"context"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var schemaName = regexp.MustCompile(`^[a-z_]{1,32}$`)

// Open connects, retrying until the database accepts connections or the context ends. A pod can start before Postgres does.
func Open(ctx context.Context, url string, log *slog.Logger) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = 20
	for {
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				return pool, nil
			}
			pool.Close()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.Warn("database not ready", "error", err)
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// ValidSchema reports whether a name can be put into SQL as an identifier.
func ValidSchema(name string) bool { return schemaName.MatchString(name) }

// Migrate applies the *.sql files in dir (sorted by name) that this schema has not seen. It takes an advisory lock, so
// several replicas starting together migrate once. Each file runs in its own transaction with the version recorded.
func Migrate(ctx context.Context, pool *pgxpool.Pool, schema string, files fs.FS, dir string) error {
	if !ValidSchema(schema) {
		return fmt.Errorf("invalid schema name %q", schema)
	}
	entries, err := fs.ReadDir(files, dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	h := fnv.New64a()
	_, _ = h.Write([]byte("migrate:" + schema))
	lock := int64(h.Sum64() >> 1)
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lock); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lock) }()

	if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, schema)); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`, schema)); err != nil {
		return err
	}
	for _, name := range names {
		var applied bool
		if err := conn.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s.schema_migrations WHERE version = $1)`, schema), name).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		body, err := fs.ReadFile(files, dir+"/"+name)
		if err != nil {
			return err
		}
		if err := applyOne(ctx, conn, schema, name, string(body)); err != nil {
			return fmt.Errorf("migration %s/%s: %w", schema, name, err)
		}
	}
	return nil
}

func applyOne(ctx context.Context, conn interface {
	Begin(context.Context) (pgx.Tx, error)
}, schema, name, body string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, body); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.schema_migrations (version) VALUES ($1)`, schema), name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Tx runs fn inside a transaction and commits it if fn returns nil.
func Tx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// IsUniqueViolation reports whether err is Postgres' unique_violation (23505).
func IsUniqueViolation(err error) bool {
	type coder interface{ SQLState() string }
	for err != nil {
		if c, ok := err.(coder); ok {
			return c.SQLState() == "23505"
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
