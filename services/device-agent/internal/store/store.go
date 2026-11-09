// Package store owns the device's SQLite database: opening it with
// power-loss-safe settings, applying embedded migrations, integrity checks,
// online backups and recovery from corruption.
//
// The driver is modernc.org/sqlite (pure Go, no cgo), so the agent
// cross-compiles to linux/arm64 with plain `GOOS=linux GOARCH=arm64 go build`.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store wraps the database handle.
type Store struct {
	DB   *sql.DB
	path string
}

// Options configure Open.
type Options struct {
	// Path to the database file. Its directory is created if missing.
	Path string
	// BackupDir receives online backups and quarantined corrupt files.
	BackupDir string
}

// pragmas applied to every connection.
//
// synchronous=FULL: the outbox holds records we promised never to lose; write
// volume is tiny (a few rows per citizen session) so the fsync cost is fine.
// WAL keeps readers (health checks, UI) from blocking the writer.
var pragmas = []string{
	"journal_mode(WAL)",
	"synchronous(FULL)",
	"foreign_keys(ON)",
	"busy_timeout(5000)",
	"temp_store(MEMORY)",
	"cache_size(-8000)", // ~8 MB page cache, bounded
	"wal_autocheckpoint(1000)",
}

func dsn(path string) string {
	var b strings.Builder
	b.WriteString("file:")
	b.WriteString(path)
	b.WriteString("?_txlock=immediate")
	for _, p := range pragmas {
		b.WriteString("&_pragma=")
		b.WriteString(p)
	}
	return b.String()
}

// ErrCorrupt is returned by Open when the integrity check fails and no
// backup could be restored.
var ErrCorrupt = errors.New("store: database failed integrity check")

// Open opens (creating if needed) and migrates the database. If the existing
// file fails PRAGMA quick_check it is quarantined (never deleted) and the
// newest backup is restored; if there is no backup a fresh database is
// created and the returned Recovery describes what happened.
func Open(ctx context.Context, o Options) (*Store, *Recovery, error) {
	if o.Path == "" {
		return nil, nil, errors.New("store: empty path")
	}
	if err := os.MkdirAll(filepath.Dir(o.Path), 0o750); err != nil {
		return nil, nil, fmt.Errorf("store: create dir: %w", err)
	}
	if o.BackupDir == "" {
		o.BackupDir = filepath.Join(filepath.Dir(o.Path), "backups")
	}

	var rec *Recovery
	if _, err := os.Stat(o.Path); err == nil {
		ok, detail := checkFile(ctx, o.Path)
		if !ok {
			r, err := recoverFrom(ctx, o, detail)
			if err != nil {
				return nil, r, err
			}
			rec = r
		}
	}

	db, err := sql.Open("sqlite", dsn(o.Path))
	if err != nil {
		return nil, rec, fmt.Errorf("store: open: %w", err)
	}
	// One writer; a few readers. SQLite serialises writers anyway and a
	// single connection pool entry for writes avoids SQLITE_BUSY storms.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)

	s := &Store{DB: db, path: o.Path}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, rec, err
	}
	return s, rec, nil
}

// Close closes the database after a final WAL checkpoint.
func (s *Store) Close() error {
	_, _ = s.DB.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return s.DB.Close()
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// checkFile runs quick_check on a database file using a throwaway handle.
func checkFile(ctx context.Context, path string) (bool, string) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rw")
	if err != nil {
		return false, err.Error()
	}
	defer db.Close()
	return quickCheck(ctx, db)
}

func quickCheck(ctx context.Context, db *sql.DB) (bool, string) {
	rows, err := db.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return false, err.Error()
	}
	defer rows.Close()
	var msgs []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return false, err.Error()
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return false, err.Error()
	}
	if len(msgs) == 1 && msgs[0] == "ok" {
		return true, "ok"
	}
	return false, strings.Join(msgs, "; ")
}

// QuickCheck runs PRAGMA quick_check on the open database (health check).
func (s *Store) QuickCheck(ctx context.Context) (bool, string) { return quickCheck(ctx, s.DB) }

// Ping is a cheap liveness probe.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.DB.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

// SchemaVersion returns PRAGMA user_version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.DB.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		name := e.Name()
		num, _, ok := strings.Cut(name, "_")
		if !ok || !strings.HasSuffix(name, ".sql") {
			return nil, fmt.Errorf("store: bad migration file name %q", name)
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("store: bad migration number in %q", name)
		}
		b, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: name, sql: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("store: migrations must be numbered 1..n without gaps; got %s", m.name)
		}
	}
	return out, nil
}

// LatestSchemaVersion is the schema version this binary expects.
func LatestSchemaVersion() int {
	ms, err := loadMigrations()
	if err != nil {
		return 0
	}
	return len(ms)
}

// migrate applies pending migrations, each in its own transaction together
// with the user_version bump, so a power cut mid-migration leaves the
// database at the previous version.
func (s *Store) migrate(ctx context.Context) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	cur, err := s.SchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if cur > len(ms) {
		// A newer binary migrated this DB and we were rolled back. Refuse to
		// run rather than corrupt data; the updater must restore a matching
		// backup or roll forward.
		return fmt.Errorf("store: database schema v%d is newer than this binary (v%d)", cur, len(ms))
	}
	for _, m := range ms[cur:] {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit migration %s: %w", m.name, err)
		}
	}
	return nil
}

// Backup writes a consistent online copy with VACUUM INTO and keeps the
// newest `keep` backups. Returns the new backup path.
func (s *Store) Backup(ctx context.Context, dir string, keep int) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	name := "device-" + time.Now().UTC().Format("20060102T150405Z") + ".db"
	dst := filepath.Join(dir, name)
	tmp := dst + ".partial"
	_ = os.Remove(tmp)
	if _, err := s.DB.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("store: backup: %w", err)
	}
	if ok, detail := checkFile(ctx, tmp); !ok {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("store: backup failed verification: %s", detail)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	pruneBackups(dir, keep)
	return dst, nil
}

func listBackups(dir string) []string {
	matches, _ := filepath.Glob(filepath.Join(dir, "device-*.db"))
	sort.Strings(matches) // timestamped names sort chronologically
	return matches
}

func pruneBackups(dir string, keep int) {
	if keep < 1 {
		keep = 1
	}
	b := listBackups(dir)
	for len(b) > keep {
		_ = os.Remove(b[0])
		b = b[1:]
	}
}

// Recovery describes an automatic recovery performed by Open.
type Recovery struct {
	Reason          string `json:"reason"`
	QuarantinedPath string `json:"quarantined_path"`
	RestoredFrom    string `json:"restored_from,omitempty"`
	// FreshDatabase is true when no usable backup existed. Pending outbox
	// events may still be recoverable from the quarantined file by a
	// technician; it is never deleted automatically.
	FreshDatabase bool `json:"fresh_database"`
}

func recoverFrom(ctx context.Context, o Options, detail string) (*Recovery, error) {
	if err := os.MkdirAll(o.BackupDir, 0o750); err != nil {
		return nil, err
	}
	q := filepath.Join(o.BackupDir, "corrupt-"+time.Now().UTC().Format("20060102T150405Z")+"-"+filepath.Base(o.Path))
	rec := &Recovery{Reason: detail, QuarantinedPath: q}
	if err := os.Rename(o.Path, q); err != nil {
		return rec, fmt.Errorf("%w: and could not quarantine it: %v", ErrCorrupt, err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(o.Path + suffix); err == nil {
			_ = os.Rename(o.Path+suffix, q+suffix)
		}
	}
	backups := listBackups(o.BackupDir)
	for i := len(backups) - 1; i >= 0; i-- {
		if ok, _ := checkFile(ctx, backups[i]); !ok {
			continue
		}
		if err := copyFile(backups[i], o.Path); err != nil {
			continue
		}
		rec.RestoredFrom = backups[i]
		return rec, nil
	}
	rec.FreshDatabase = true
	return rec, nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".restore"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
