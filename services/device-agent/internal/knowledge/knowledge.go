// Package knowledge is the device's scheme knowledge store: scheme records
// (internal/scheme) loaded into the SQLite `schemes` table and the
// `schemes_fts` full-text index, plus the presentation policy that decides
// whether a record's facts may be shown.
//
// Rules enforced here:
//   - every record passes scheme.Record.Validate() or is skipped and reported;
//   - a load replaces the whole knowledge base atomically (one transaction)
//     and records the bundle version in device_meta;
//   - a load that would leave the device with zero valid records when the
//     input had records is refused (the previous knowledge base stays);
//   - only records for which Record.IsFactual(now) is true are presented as
//     fact. Everything else yields the scheme name and a generic "please
//     confirm at the office" message, except on dev-* device profiles with
//     AllowUnverifiedForDev, where the content is shown under a prominent
//     "not yet verified" disclaimer.
package knowledge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/textnorm"
)

// Limits keep loading bounded.
const (
	MaxRecords        = 2000
	MaxRecordFileSize = 512 << 10 // one record JSON file
	MaxBundleFileSize = 32 << 20  // one bundle JSON file
	MaxRecordJSON     = 256 << 10 // one record after re-marshalling
	MaxSearchResults  = 20
	maxFTSRows        = 64
	maxQueryTerms     = 32
)

// device_meta keys written by a load.
const (
	MetaBundleVersion = "knowledge.bundle_version"
	MetaLoadedAt      = "knowledge.loaded_at"
	MetaRecordCount   = "knowledge.record_count"
)

// ErrNotFound is returned by Get for an unknown id.
var ErrNotFound = errors.New("knowledge: record not found")

// ErrNothingValid is returned when a load contains records but none valid.
var ErrNothingValid = errors.New("knowledge: no valid records in input; previous knowledge kept")

// Config controls presentation policy.
type Config struct {
	// DeviceProfile is the hardware/config profile ("dev-mac", "pi5" …).
	DeviceProfile string
	// AllowUnverifiedForDev lets UNVERIFIED / NEEDS_REVIEW / overdue
	// content be shown with a disclaimer. Honoured only when DeviceProfile
	// starts with "dev-"; ignored (forced off) on every other profile.
	AllowUnverifiedForDev bool
	// Now is the clock (tests); defaults to time.Now.
	Now func() time.Time
}

// UnverifiedAllowed reports whether the dev override is in effect.
func (c Config) UnverifiedAllowed() bool {
	return c.AllowUnverifiedForDev && strings.HasPrefix(c.DeviceProfile, "dev-")
}

// Store is the knowledge store. Safe for concurrent use.
type Store struct {
	db  *sql.DB
	cfg Config

	mu    sync.RWMutex
	names *textnorm.Vocab // record names + aliases → record id
	terms *textnorm.Vocab // every indexed term → itself (spelling correction)
}

// New wraps an open device database (migrated by internal/store) and
// builds the in-memory matching indexes from the current schemes table.
func New(ctx context.Context, db *sql.DB, cfg Config) (*Store, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Store{db: db, cfg: cfg}
	recs, err := s.all(ctx)
	if err != nil {
		return nil, err
	}
	s.rebuild(recs)
	return s, nil
}

// Config returns the store's policy configuration.
func (s *Store) Config() Config { return s.cfg }

// Skip describes a record that was not loaded.
type Skip struct {
	Source string `json:"source"` // file name or bundle index
	ID     string `json:"id,omitempty"`
	Reason string `json:"reason"`
}

// Report summarises a load.
type Report struct {
	BundleVersion string `json:"bundle_version"`
	Loaded        int    `json:"loaded"`
	Skipped       []Skip `json:"skipped,omitempty"`
	Factual       int    `json:"factual"` // records that are VERIFIED and not overdue
}

type sourced struct {
	src string
	rec scheme.Record
}

// LoadPath loads a directory of *.json record files or a single bundle file.
// For a directory, bundleVersion may be empty: a content hash is used.
func (s *Store) LoadPath(ctx context.Context, path, bundleVersion string, verify func(scheme.Bundle) error) (Report, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Report{}, err
	}
	if fi.IsDir() {
		return s.LoadDir(ctx, path, bundleVersion)
	}
	return s.LoadBundleFile(ctx, path, verify)
}

// LoadDir loads every *.json file in dir (one record per file, non-recursive).
func (s *Store) LoadDir(ctx context.Context, dir, bundleVersion string) (Report, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Report{}, err
	}
	var in []sourced
	var skips []Skip
	h := sha256.New()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		if len(in)+len(skips) >= MaxRecords {
			skips = append(skips, Skip{Source: e.Name(), Reason: fmt.Sprintf("more than %d files", MaxRecords)})
			continue
		}
		b, err := readLimited(filepath.Join(dir, e.Name()), MaxRecordFileSize)
		if err != nil {
			skips = append(skips, Skip{Source: e.Name(), Reason: err.Error()})
			continue
		}
		h.Write([]byte(e.Name()))
		h.Write(b)
		var r scheme.Record
		if err := decodeStrict(b, &r); err != nil {
			skips = append(skips, Skip{Source: e.Name(), Reason: "decode: " + err.Error()})
			continue
		}
		in = append(in, sourced{src: e.Name(), rec: r})
	}
	if bundleVersion == "" {
		bundleVersion = "local-" + hex.EncodeToString(h.Sum(nil))[:12]
	}
	rep, err := s.replace(ctx, bundleVersion, in, len(in)+len(skips))
	rep.Skipped = append(skips, rep.Skipped...)
	return rep, err
}

// LoadBundleFile loads a scheme.Bundle JSON file. verify, if non-nil, is
// called before anything is written (signature verification belongs to the
// caller, which holds the trusted keys); a verify error aborts the load.
func (s *Store) LoadBundleFile(ctx context.Context, path string, verify func(scheme.Bundle) error) (Report, error) {
	b, err := readLimited(path, MaxBundleFileSize)
	if err != nil {
		return Report{}, err
	}
	var bun scheme.Bundle
	if err := json.Unmarshal(b, &bun); err != nil {
		return Report{}, fmt.Errorf("knowledge: decode bundle: %w", err)
	}
	return s.LoadBundle(ctx, bun, verify)
}

// LoadBundle loads an already-decoded bundle.
func (s *Store) LoadBundle(ctx context.Context, bun scheme.Bundle, verify func(scheme.Bundle) error) (Report, error) {
	if verify != nil {
		if err := verify(bun); err != nil {
			return Report{}, fmt.Errorf("knowledge: bundle verification failed: %w", err)
		}
	}
	if strings.TrimSpace(bun.BundleVersion) == "" {
		return Report{}, errors.New("knowledge: bundle_version is required")
	}
	if len(bun.Records) > MaxRecords {
		return Report{}, fmt.Errorf("knowledge: bundle has %d records (max %d)", len(bun.Records), MaxRecords)
	}
	in := make([]sourced, 0, len(bun.Records))
	for i, r := range bun.Records {
		in = append(in, sourced{src: fmt.Sprintf("records[%d]", i), rec: r})
	}
	return s.replace(ctx, bun.BundleVersion, in, len(in))
}

// Replace loads records directly (used by tests and the sync worker).
func (s *Store) Replace(ctx context.Context, bundleVersion string, recs []scheme.Record) (Report, error) {
	if len(recs) > MaxRecords {
		return Report{}, fmt.Errorf("knowledge: %d records (max %d)", len(recs), MaxRecords)
	}
	in := make([]sourced, 0, len(recs))
	for i, r := range recs {
		in = append(in, sourced{src: fmt.Sprintf("records[%d]", i), rec: r})
	}
	return s.replace(ctx, bundleVersion, in, len(in))
}

func (s *Store) replace(ctx context.Context, bundleVersion string, in []sourced, total int) (Report, error) {
	rep := Report{BundleVersion: bundleVersion}
	now := s.cfg.Now()
	seen := map[string]bool{}
	var valid []scheme.Record
	var raw [][]byte
	for _, x := range in {
		r := x.rec
		if err := r.Validate(); err != nil {
			rep.Skipped = append(rep.Skipped, Skip{Source: x.src, ID: r.ID, Reason: oneLine(err.Error())})
			continue
		}
		if seen[r.ID] {
			rep.Skipped = append(rep.Skipped, Skip{Source: x.src, ID: r.ID, Reason: "duplicate id"})
			continue
		}
		b, err := json.Marshal(r)
		if err != nil || len(b) > MaxRecordJSON {
			rep.Skipped = append(rep.Skipped, Skip{Source: x.src, ID: r.ID, Reason: "record too large"})
			continue
		}
		seen[r.ID] = true
		valid = append(valid, r)
		raw = append(raw, b)
		if r.IsFactual(now) {
			rep.Factual++
		}
	}
	if len(valid) == 0 && total > 0 {
		return rep, ErrNothingValid
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return rep, err
	}
	defer tx.Rollback()
	for _, q := range []string{`DELETE FROM schemes`, `DELETE FROM schemes_fts`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return rep, fmt.Errorf("knowledge: clear: %w", err)
		}
	}
	insS, err := tx.PrepareContext(ctx, `INSERT INTO schemes(id,version,status,category,state_code,name_en,name_hi,record,last_verified_at,bundle_version)
		VALUES(?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return rep, err
	}
	defer insS.Close()
	insF, err := tx.PrepareContext(ctx, `INSERT INTO schemes_fts(id,text) VALUES(?,?)`)
	if err != nil {
		return rep, err
	}
	defer insF.Close()
	for i, r := range valid {
		if _, err := insS.ExecContext(ctx, r.ID, r.Version, string(r.Verification.Status), r.Category,
			nullIfEmpty(r.State), r.Name.En, r.Name.Hi, string(raw[i]), nullIfEmpty(r.LastVerified), bundleVersion); err != nil {
			return rep, fmt.Errorf("knowledge: insert %s: %w", r.ID, err)
		}
		if _, err := insF.ExecContext(ctx, r.ID, indexText(r)); err != nil {
			return rep, fmt.Errorf("knowledge: index %s: %w", r.ID, err)
		}
	}
	meta := map[string]string{
		MetaBundleVersion: bundleVersion,
		MetaLoadedAt:      now.UTC().Format(time.RFC3339),
		MetaRecordCount:   fmt.Sprint(len(valid)),
	}
	for k, v := range meta {
		if _, err := tx.ExecContext(ctx, `INSERT INTO device_meta(key,value,updated_at) VALUES(?,?,strftime('%Y-%m-%dT%H:%M:%fZ','now'))
			ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, k, v); err != nil {
			return rep, fmt.Errorf("knowledge: meta: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return rep, err
	}
	rep.Loaded = len(valid)
	s.rebuild(valid)
	return rep, nil
}

// BundleVersion returns the loaded bundle version ("" if none).
func (s *Store) BundleVersion(ctx context.Context) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM device_meta WHERE key=?`, MetaBundleVersion).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// Get returns one record.
func (s *Store) Get(ctx context.Context, id string) (scheme.Record, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT record FROM schemes WHERE id=?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return scheme.Record{}, ErrNotFound
	}
	if err != nil {
		return scheme.Record{}, err
	}
	var r scheme.Record
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return scheme.Record{}, fmt.Errorf("knowledge: corrupt record %s: %w", id, err)
	}
	return r, nil
}

// ByCategory returns all records of a category, ordered by English name.
func (s *Store) ByCategory(ctx context.Context, cat string) ([]scheme.Record, error) {
	return s.query(ctx, `SELECT record FROM schemes WHERE category=? ORDER BY name_en LIMIT ?`, cat, MaxRecords)
}

func (s *Store) all(ctx context.Context) ([]scheme.Record, error) {
	return s.query(ctx, `SELECT record FROM schemes ORDER BY id LIMIT ?`, MaxRecords)
}

func (s *Store) query(ctx context.Context, q string, args ...any) ([]scheme.Record, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scheme.Record
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var r scheme.Record
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, fmt.Errorf("knowledge: corrupt record: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return b, nil
}

func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after record")
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", "; ")
	if len(s) > 400 {
		s = s[:400]
	}
	return s
}

// sortedKeys is a small helper for deterministic iteration.
func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
