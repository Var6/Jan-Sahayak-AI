// Package db opens the PostgreSQL pool and applies the embedded migrations.
//
// Migration runner rules:
//   - files are migrations/NNNN_name.sql, applied in numeric order;
//   - each file runs in its own transaction together with its
//     schema_migrations row, so a file is applied fully or not at all;
//   - a session-level advisory lock serialises concurrent runners;
//   - the SHA-256 of every applied file is recorded and re-checked, so an
//     edited migration is detected instead of silently diverging;
//   - a database with a version newer than this binary knows is refused
//     (protects against running an old binary after a rollback).
package db

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// advisoryLockKey is an arbitrary constant identifying the migration lock.
const advisoryLockKey int64 = 0x4a53_4d49_4752 // "JSMIGR"

// Options tune the pool.
type Options struct {
	MaxConns         int32
	StatementTimeout time.Duration
	// Schema, if set, is used as search_path (tests use a fresh schema).
	Schema string
}

// Open creates a pool and pings it.
func Open(ctx context.Context, url string, o Options) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("db: parse url: %w", err)
	}
	if o.MaxConns <= 0 {
		o.MaxConns = 10
	}
	if o.StatementTimeout <= 0 {
		o.StatementTimeout = 15 * time.Second
	}
	cfg.MaxConns = o.MaxConns
	cfg.MinConns = 0
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = time.Minute
	if cfg.ConnConfig.ConnectTimeout == 0 || cfg.ConnConfig.ConnectTimeout > 10*time.Second {
		cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	}
	rp := cfg.ConnConfig.RuntimeParams
	rp["application_name"] = "jan-sahayak-central"
	rp["statement_timeout"] = strconv.FormatInt(o.StatementTimeout.Milliseconds(), 10)
	rp["idle_in_transaction_session_timeout"] = "30000"
	rp["timezone"] = "UTC"
	if o.Schema != "" {
		rp["search_path"] = o.Schema
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: open: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// Migration is one embedded SQL file.
type Migration struct {
	Version int
	Name    string
	SQL     string
	SHA256  string
}

// Migrations returns the embedded migrations in order.
func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []Migration
	seen := map[int]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		num, _, ok := strings.Cut(name, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v < 1 {
			return nil, fmt.Errorf("db: bad migration file name %q", name)
		}
		if seen[v] {
			return nil, fmt.Errorf("db: duplicate migration version %d", v)
		}
		seen[v] = true
		b, err := migrationsFS.ReadFile(path.Join("migrations", name))
		if err != nil {
			return nil, err
		}
		h := sha256.Sum256(b)
		out = append(out, Migration{Version: v, Name: name, SQL: string(b), SHA256: hex.EncodeToString(h[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i, m := range out {
		if m.Version != i+1 {
			return nil, fmt.Errorf("db: migration versions must be contiguous from 1, got %d at position %d", m.Version, i+1)
		}
	}
	return out, nil
}

// LatestVersion is the schema version this binary expects.
func LatestVersion() int {
	ms, err := Migrations()
	if err != nil || len(ms) == 0 {
		return 0
	}
	return ms[len(ms)-1].Version
}

// ErrNewerSchema means the database was migrated by a newer binary.
var ErrNewerSchema = errors.New("db: database schema is newer than this binary")

// ErrChecksum means an applied migration differs from the embedded file.
var ErrChecksum = errors.New("db: applied migration checksum mismatch")

// Migrate applies pending migrations and returns the versions applied.
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]int, error) {
	ms, err := Migrations()
	if err != nil {
		return nil, err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		return nil, fmt.Errorf("db: advisory lock: %w", err)
	}
	defer func() {
		// Use a fresh context: the lock must be released even if ctx ended.
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, advisoryLockKey); err != nil {
			conn.Conn().Close(uctx) // drop the session so the lock is freed
		}
	}()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    integer PRIMARY KEY,
		name       text NOT NULL,
		sha256     text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, fmt.Errorf("db: create schema_migrations: %w", err)
	}
	applied, err := appliedMigrations(ctx, conn.Conn())
	if err != nil {
		return nil, err
	}
	byVersion := map[int]Migration{}
	for _, m := range ms {
		byVersion[m.Version] = m
	}
	for v, sum := range applied {
		m, ok := byVersion[v]
		if !ok {
			return nil, fmt.Errorf("%w (database has version %d)", ErrNewerSchema, v)
		}
		if m.SHA256 != sum {
			return nil, fmt.Errorf("%w: version %d", ErrChecksum, v)
		}
	}
	var done []int
	for _, m := range ms {
		if _, ok := applied[m.Version]; ok {
			continue
		}
		if err := applyOne(ctx, conn.Conn(), m); err != nil {
			return done, err
		}
		done = append(done, m.Version)
	}
	return done, nil
}

func appliedMigrations(ctx context.Context, c *pgx.Conn) (map[int]string, error) {
	rows, err := c.Query(ctx, `SELECT version, sha256 FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var v int
		var s string
		if err := rows.Scan(&v, &s); err != nil {
			return nil, err
		}
		out[v] = s
	}
	return out, rows.Err()
}

func applyOne(ctx context.Context, c *pgx.Conn, m Migration) error {
	tx, err := c.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	// No arguments → simple protocol, which allows multiple statements.
	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return fmt.Errorf("db: migration %s: %w", m.Name, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name, sha256) VALUES ($1, $2, $3)`,
		m.Version, m.Name, m.SHA256); err != nil {
		return fmt.Errorf("db: record migration %s: %w", m.Name, err)
	}
	return tx.Commit(ctx)
}

// SchemaVersion returns the highest applied migration (0 if none).
func SchemaVersion(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var v int
	err := pool.QueryRow(ctx, `SELECT COALESCE(max(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}

// CheckSchema returns an error unless the database is at exactly the
// version this binary expects.
func CheckSchema(ctx context.Context, pool *pgxpool.Pool) error {
	v, err := SchemaVersion(ctx, pool)
	if err != nil {
		return err
	}
	want := LatestVersion()
	switch {
	case v > want:
		return fmt.Errorf("%w (database %d, binary %d)", ErrNewerSchema, v, want)
	case v < want:
		return fmt.Errorf("db: database schema version %d < %d: run `central-admin migrate`", v, want)
	}
	return nil
}
