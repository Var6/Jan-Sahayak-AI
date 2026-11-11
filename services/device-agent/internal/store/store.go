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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite" // also registers the "sqlite" driver

	"github.com/janmanindia/jan-sahayak/internal/syncproto"
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

// ErrUnreadable is returned by Open when the existing database file could
// not be checked for a reason that is NOT corruption (permissions, I/O
// error, lock held by another process, ...). The file is left exactly where
// it is and startup must fail loudly so systemd retries and a technician can
// fix the cause; quarantining a healthy database here would silently drop
// every pending outbox event from the live database.
var ErrUnreadable = errors.New("store: database file could not be checked (not treated as corruption)")

// Open opens (creating if needed) and migrates the database.
//
// If the existing file is genuinely corrupt (PRAGMA quick_check returns
// anything other than a single "ok" row, or SQLite reports SQLITE_CORRUPT /
// SQLITE_NOTADB) it is quarantined (never deleted), the newest verified
// backup is restored (or a fresh database created), and unacknowledged
// outbox rows that can still be read from the quarantined file are salvaged
// into the new live database. Any other error opening or checking the file
// is returned wrapped in ErrUnreadable and nothing is moved.
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
	switch _, err := os.Stat(o.Path); {
	case err == nil:
		corrupt, detail, cerr := checkFile(ctx, o.Path)
		if cerr != nil {
			return nil, nil, fmt.Errorf("%w: %s: %v", ErrUnreadable, o.Path, cerr)
		}
		if corrupt {
			r, err := recoverFrom(ctx, o, detail)
			if err != nil {
				return nil, r, err
			}
			rec = r
		}
	case errors.Is(err, os.ErrNotExist):
		// first boot: a fresh database is created below
	default:
		return nil, nil, fmt.Errorf("%w: %s: %v", ErrUnreadable, o.Path, err)
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
	if rec != nil {
		s.salvage(ctx, rec)
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
// corrupt is true only for genuine corruption; err is set (and corrupt
// false) when the file could not be checked for any other reason.
func checkFile(ctx context.Context, path string) (corrupt bool, detail string, err error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rw")
	if err != nil {
		return false, "", err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	return quickCheck(ctx, db)
}

func quickCheck(ctx context.Context, db *sql.DB) (corrupt bool, detail string, err error) {
	rows, err := db.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return classify(err)
	}
	defer rows.Close()
	var msgs []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return classify(err)
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return classify(err)
	}
	if len(msgs) == 1 && msgs[0] == "ok" {
		return false, "ok", nil
	}
	if len(msgs) == 0 {
		return true, "quick_check returned no rows", nil
	}
	return true, strings.Join(msgs, "; "), nil
}

// classify turns an error from opening/checking a database into either
// corruption (SQLITE_CORRUPT, SQLITE_NOTADB) or a plain error.
func classify(err error) (bool, string, error) {
	if IsCorruptionError(err) {
		return true, err.Error(), nil
	}
	return false, "", err
}

// SQLite primary result codes that mean the file itself is damaged.
const (
	sqliteCorrupt = 11 // SQLITE_CORRUPT
	sqliteNotADB  = 26 // SQLITE_NOTADB
)

// IsCorruptionError reports whether err is SQLite reporting a damaged file
// (SQLITE_CORRUPT or SQLITE_NOTADB, including extended codes). Permission,
// I/O, locking and "cannot open" errors are NOT corruption.
func IsCorruptionError(err error) bool {
	if err == nil {
		return false
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff {
		case sqliteCorrupt, sqliteNotADB:
			return true
		}
	}
	// Some corruption is surfaced as SQLITE_ERROR carrying the canonical
	// corruption message; match only those exact SQLite strings.
	msg := err.Error()
	return strings.Contains(msg, "database disk image is malformed") || strings.Contains(msg, "file is not a database")
}

// QuickCheck runs PRAGMA quick_check on the open database (health check).
// It returns false with a detail for corruption and for check errors.
func (s *Store) QuickCheck(ctx context.Context) (bool, string) {
	corrupt, detail, err := quickCheck(ctx, s.DB)
	if err != nil {
		return false, err.Error()
	}
	return !corrupt, detail
}

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
// newest `keep` backups. Returns the new backup path. The copy and its
// directory entry are fsynced so a power cut cannot leave a newest backup
// that exists by name but not on disk.
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
	if err := syncFile(tmp); err != nil { // VACUUM INTO does not fsync
		_ = os.Remove(tmp)
		return "", fmt.Errorf("store: backup fsync: %w", err)
	}
	if corrupt, detail, err := checkFile(ctx, tmp); corrupt || err != nil {
		_ = os.Remove(tmp)
		if err != nil {
			detail = err.Error()
		}
		return "", fmt.Errorf("store: backup failed verification: %s", detail)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	if err := syncDir(dir); err != nil {
		return "", fmt.Errorf("store: backup dir fsync: %w", err)
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
	// FreshDatabase is true when no usable backup existed.
	FreshDatabase bool `json:"fresh_database"`
	// SalvagedEvents is how many unacknowledged outbox rows were copied from
	// the quarantined file into the new live database (rows already present
	// in the restored backup are not counted). SalvageSkipped counts rows
	// that were readable but failed verification (payload hash mismatch or
	// invalid JSON) and were left in the quarantined file. SalvageError is
	// set when reading the quarantined file failed part-way or entirely; a
	// technician may still recover more from it. The quarantined file is
	// never deleted automatically.
	SalvagedEvents int    `json:"salvaged_events"`
	SalvageSkipped int    `json:"salvage_skipped,omitempty"`
	SalvageError   string `json:"salvage_error,omitempty"`
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
	_ = syncDir(o.BackupDir)
	_ = syncDir(filepath.Dir(o.Path))
	backups := listBackups(o.BackupDir)
	for i := len(backups) - 1; i >= 0; i-- {
		if corrupt, _, err := checkFile(ctx, backups[i]); corrupt || err != nil {
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

// salvageTimeout bounds how long Open spends reading a quarantined file.
const salvageTimeout = 60 * time.Second

// salvage copies unacknowledged outbox rows (PENDING, UPLOADING, FAILED,
// CONFLICT) that can still be read from the quarantined file into the live
// database. Rows already present (same id, e.g. from the restored backup)
// are left alone. UPLOADING rows come back as PENDING: the upload may or may
// not have reached the server, and the server is idempotent on event id.
// Every salvaged payload must still match its stored SHA-256, so damaged
// rows are never sent. Errors are recorded in rec, never returned: Open
// succeeds either way and the quarantined file stays for a technician.
func (s *Store) salvage(ctx context.Context, rec *Recovery) {
	ctx, cancel := context.WithTimeout(ctx, salvageTimeout)
	defer cancel()
	if _, err := os.Stat(rec.QuarantinedPath); err != nil {
		rec.SalvageError = err.Error()
		return
	}
	src, err := sql.Open("sqlite", "file:"+rec.QuarantinedPath+"?mode=ro")
	if err != nil {
		rec.SalvageError = err.Error()
		return
	}
	defer src.Close()
	src.SetMaxOpenConns(1)

	type row struct {
		id, typ, payload, hash, createdAt, state, nextAt string
		schemaVersion, attempts                          int
		lastErr                                          sql.NullString
	}
	var rows []row
	// NOT INDEXED: a damaged index must not hide rows that the table itself
	// still holds.
	q, err := src.QueryContext(ctx, `SELECT id, type, schema_version, payload, payload_sha256, created_at,
		state, attempts, next_attempt_at, last_error FROM outbox_events NOT INDEXED`)
	if err != nil {
		rec.SalvageError = err.Error()
		return
	}
	for q.Next() {
		var r row
		if err := q.Scan(&r.id, &r.typ, &r.schemaVersion, &r.payload, &r.hash, &r.createdAt,
			&r.state, &r.attempts, &r.nextAt, &r.lastErr); err != nil {
			rec.SalvageError = err.Error()
			break
		}
		switch r.state {
		case "PENDING", "UPLOADING", "FAILED", "CONFLICT":
			rows = append(rows, r)
		}
	}
	if err := q.Err(); err != nil && rec.SalvageError == "" {
		rec.SalvageError = err.Error() // keep what was read before the damage
	}
	q.Close()
	if len(rows) == 0 {
		return
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		rec.SalvageError = err.Error()
		return
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	inserted := 0
	for _, r := range rows {
		if syncproto.PayloadHash([]byte(r.payload)) != r.hash {
			rec.SalvageSkipped++
			continue
		}
		state, nextAt := r.state, r.nextAt
		if state == "UPLOADING" || state == "PENDING" {
			state, nextAt = "PENDING", now
		}
		lastErr := "salvaged from quarantined database"
		if r.lastErr.Valid && r.lastErr.String != "" {
			lastErr += ": " + r.lastErr.String
		}
		if len(lastErr) > syncproto.MaxReasonLength {
			lastErr = lastErr[:syncproto.MaxReasonLength]
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO outbox_events
			(id, type, schema_version, payload, payload_sha256, created_at, state, attempts, next_attempt_at, last_error)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.id, r.typ, r.schemaVersion, r.payload, r.hash, r.createdAt, state, r.attempts, nextAt, lastErr)
		if err != nil {
			rec.SalvageSkipped++
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}
	if err := tx.Commit(); err != nil {
		rec.SalvageError = err.Error()
		return
	}
	rec.SalvagedEvents = inserted
}

// copyFile streams src to dst via a temporary file, fsyncing the data and
// the directory entry (no whole-file read into RAM: the agent runs under a
// memory cap and the database grows over the years).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".restore"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, in); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}

// syncFile fsyncs an existing file.
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// syncDir fsyncs a directory so a rename or create inside it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}
