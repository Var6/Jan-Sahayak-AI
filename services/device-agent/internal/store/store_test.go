package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janmanindia/jan-sahayak/internal/syncproto"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, rec, err := Open(context.Background(), Options{Path: filepath.Join(dir, "device.db")})
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil {
		t.Fatalf("unexpected recovery on fresh db: %+v", rec)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func TestOpenMigratesAndSetsPragmas(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v, err := s.SchemaVersion(ctx)
	if err != nil || v != LatestSchemaVersion() || v < 1 {
		t.Fatalf("schema version %d err %v", v, err)
	}
	var mode string
	if err := s.DB.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode=%q err=%v", mode, err)
	}
	var syncMode int
	if err := s.DB.QueryRow("PRAGMA synchronous").Scan(&syncMode); err != nil || syncMode != 2 {
		t.Fatalf("synchronous=%d (want 2=FULL) err=%v", syncMode, err)
	}
	if ok, d := s.QuickCheck(ctx); !ok {
		t.Fatal(d)
	}
}

func TestReopenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "device.db")
	for i := 0; i < 3; i++ {
		s, _, err := Open(context.Background(), Options{Path: p})
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}

func insertEvent(t *testing.T, s *Store, id, state string) {
	t.Helper()
	_, err := s.DB.Exec(`INSERT INTO outbox_events(id,type,payload,payload_sha256,created_at,next_attempt_at,state)
		VALUES(?,?,?,?,?,?, 'PENDING')`, id, "session.ended", `{"a":1}`, "x", "2026-11-09T00:00:00Z", "2026-11-09T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	_ = state
}

func TestDatabaseRefusesToDeleteUnacknowledgedEvents(t *testing.T) {
	s, _ := openTemp(t)
	insertEvent(t, s, "e1", "PENDING")

	for _, st := range []string{"PENDING", "UPLOADING"} {
		if st == "UPLOADING" {
			if _, err := s.DB.Exec(`UPDATE outbox_events SET state='UPLOADING' WHERE id='e1'`); err != nil {
				t.Fatal(err)
			}
		}
		_, err := s.DB.Exec(`DELETE FROM outbox_events WHERE id='e1'`)
		if err == nil || !strings.Contains(err.Error(), "refusing to delete") {
			t.Fatalf("delete in %s must be refused, got %v", st, err)
		}
	}
	// ACKNOWLEDGED without receipt is refused.
	if _, err := s.DB.Exec(`UPDATE outbox_events SET state='ACKNOWLEDGED' WHERE id='e1'`); err == nil {
		t.Fatal("ack without receipt must be refused")
	}
	if _, err := s.DB.Exec(`UPDATE outbox_events SET state='ACKNOWLEDGED', ack_receipt='r1', acked_at='2026-11-09T00:00:01Z' WHERE id='e1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`DELETE FROM outbox_events WHERE id='e1'`); err == nil {
		t.Fatal("delete of ACKNOWLEDGED (not yet DELETE_ELIGIBLE) must be refused")
	}
	if _, err := s.DB.Exec(`UPDATE outbox_events SET state='DELETE_ELIGIBLE', delete_eligible_at='2026-11-10T00:00:00Z' WHERE id='e1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`DELETE FROM outbox_events WHERE id='e1'`); err != nil {
		t.Fatalf("delete of DELETE_ELIGIBLE should succeed: %v", err)
	}
}

func TestIllegalTransitionsAndImmutablePayload(t *testing.T) {
	s, _ := openTemp(t)
	insertEvent(t, s, "e1", "PENDING")
	if _, err := s.DB.Exec(`UPDATE outbox_events SET state='DELETE_ELIGIBLE' WHERE id='e1'`); err == nil {
		t.Fatal("PENDING -> DELETE_ELIGIBLE must be refused")
	}
	if _, err := s.DB.Exec(`UPDATE outbox_events SET payload='{"a":2}' WHERE id='e1'`); err == nil {
		t.Fatal("payload must be immutable")
	}
	if _, err := s.DB.Exec(`DELETE FROM outbox_events`); err == nil {
		t.Fatal("bulk delete must be refused")
	}
}

func TestHindiFullTextSearch(t *testing.T) {
	s, _ := openTemp(t)
	if _, err := s.DB.Exec(`INSERT INTO schemes_fts(id,text) VALUES ('x','वृद्धावस्था पेंशन old age pension'), ('y','जाति प्रमाण पत्र caste certificate')`); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := s.DB.QueryRow(`SELECT id FROM schemes_fts WHERE schemes_fts MATCH ? ORDER BY rank LIMIT 1`, "पेंशन").Scan(&id); err != nil || id != "x" {
		t.Fatalf("hindi match got %q err %v", id, err)
	}
	if err := s.DB.QueryRow(`SELECT id FROM schemes_fts WHERE schemes_fts MATCH ? LIMIT 1`, "प्रमाण").Scan(&id); err != nil || id != "y" {
		t.Fatalf("hindi match 2 got %q err %v", id, err)
	}
}

func TestBackupAndCorruptionRecovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "device.db")
	bdir := filepath.Join(dir, "backups")
	s, _, err := Open(ctx, Options{Path: p, BackupDir: bdir})
	if err != nil {
		t.Fatal(err)
	}
	insertEvent(t, s, "keep-me", "PENDING")
	if _, err := s.Backup(ctx, bdir, 3); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Corrupt the main file: overwrite bytes past the header.
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	garbage := make([]byte, 8192)
	for i := range garbage {
		garbage[i] = 0xAB
	}
	if _, err := f.WriteAt(garbage, 100); err != nil {
		t.Fatal(err)
	}
	f.Close()

	s2, rec, err := Open(ctx, Options{Path: p, BackupDir: bdir})
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}
	defer s2.Close()
	if rec == nil || rec.RestoredFrom == "" || rec.FreshDatabase {
		t.Fatalf("expected restore from backup, got %+v", rec)
	}
	if _, err := os.Stat(rec.QuarantinedPath); err != nil {
		t.Fatalf("corrupt file must be quarantined, not deleted: %v", err)
	}
	var n int
	if err := s2.DB.QueryRow(`SELECT count(*) FROM outbox_events WHERE id='keep-me'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("restored db lost event: n=%d err=%v", n, err)
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	s, dir := openTemp(t)
	if _, err := s.DB.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, _, err := Open(context.Background(), Options{Path: filepath.Join(dir, "device.db")}); err == nil {
		t.Fatal("must refuse a schema newer than the binary")
	}
}

func insertHashed(t *testing.T, s *Store, id, state, payload string) {
	t.Helper()
	const at = "2026-11-09T00:00:00.000000000Z"
	var receipt, ackedAt any // NULL unless acknowledged
	if state == "ACKNOWLEDGED" {
		receipt, ackedAt = "r", at
	}
	_, err := s.DB.Exec(`INSERT INTO outbox_events(id,type,payload,payload_sha256,created_at,next_attempt_at,state,ack_receipt,acked_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, id, "session.ended", payload, syncproto.PayloadHash([]byte(payload)), at, at, state, receipt, ackedAt)
	if err != nil {
		t.Fatal(err)
	}
}

// Real corruption elsewhere in the file: the database is quarantined and the
// backup restored, and unacknowledged outbox rows written after the backup
// are salvaged from the quarantined file (verified by payload hash).
func TestCorruptionRecoverySalvagesUnackedEvents(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "device.db")
	bdir := filepath.Join(dir, "backups")
	s, _, err := Open(ctx, Options{Path: p, BackupDir: bdir})
	if err != nil {
		t.Fatal(err)
	}
	insertHashed(t, s, "in-backup", "PENDING", `{"a":1}`)
	if _, err := s.Backup(ctx, bdir, 3); err != nil {
		t.Fatal(err)
	}
	insertHashed(t, s, "after-backup-pending", "PENDING", `{"a":2}`)
	insertHashed(t, s, "after-backup-uploading", "UPLOADING", `{"a":3}`)
	insertHashed(t, s, "after-backup-acked", "ACKNOWLEDGED", `{"a":4}`)
	if _, err := s.DB.Exec(`INSERT INTO outbox_events(id,type,payload,payload_sha256,created_at,next_attempt_at)
		VALUES('tampered','x','{"a":5}','not-the-hash','2026-11-09T00:00:00Z','2026-11-09T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	// A large unrelated table occupies the tail of the file; damage it there.
	if _, err := s.DB.Exec(`CREATE TABLE filler(x BLOB)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, err := s.DB.Exec(`INSERT INTO filler(x) VALUES (randomblob(3000))`); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	garbage := make([]byte, 64<<10)
	for i := range garbage {
		garbage[i] = 0xAB
	}
	if _, err := f.WriteAt(garbage, fi.Size()-int64(len(garbage))); err != nil {
		t.Fatal(err)
	}
	f.Close()

	s2, rec, err := Open(ctx, Options{Path: p, BackupDir: bdir})
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}
	defer s2.Close()
	if rec == nil || rec.RestoredFrom == "" {
		t.Fatalf("expected restore from backup, got %+v", rec)
	}
	if rec.SalvagedEvents != 2 || rec.SalvageSkipped != 1 {
		t.Fatalf("salvage: %+v", *rec)
	}
	for id, want := range map[string]string{"in-backup": "PENDING", "after-backup-pending": "PENDING", "after-backup-uploading": "PENDING"} {
		var st string
		if err := s2.DB.QueryRow(`SELECT state FROM outbox_events WHERE id=?`, id).Scan(&st); err != nil || st != want {
			t.Fatalf("%s: state %q err %v", id, st, err)
		}
	}
	var n int
	_ = s2.DB.QueryRow(`SELECT count(*) FROM outbox_events WHERE id IN ('after-backup-acked','tampered')`).Scan(&n)
	if n != 0 {
		t.Fatalf("acked/tampered rows must not be salvaged, found %d", n)
	}
	var counter int
	if err := s2.DB.QueryRow(`SELECT n FROM outbox_counter`).Scan(&counter); err != nil || counter != 3 {
		t.Fatalf("outbox_counter=%d err=%v, want 3", counter, err)
	}
	if _, err := os.Stat(rec.QuarantinedPath); err != nil {
		t.Fatalf("quarantined file must be kept: %v", err)
	}
}

// Decision 2: a database that cannot be opened for a reason other than
// corruption makes Open fail loudly and leaves the file in place.
func TestUnreadableDatabaseFailsLoudlyAndIsNotMoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "device.db")
	s, _, err := Open(ctx, Options{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o640) })
	_, rec, err := Open(ctx, Options{Path: p})
	if !errors.Is(err, ErrUnreadable) || rec != nil {
		t.Fatalf("want ErrUnreadable and no recovery, got rec=%v err=%v", rec, err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("live database must stay in place: %v", err)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "backups", "corrupt-*")); len(m) != 0 {
		t.Fatalf("nothing may be quarantined: %v", m)
	}
}

func TestOutboxCounterTracksRows(t *testing.T) {
	s, _ := openTemp(t)
	insertEvent(t, s, "e1", "PENDING")
	insertEvent(t, s, "e2", "PENDING")
	var n int
	if err := s.DB.QueryRow(`SELECT n FROM outbox_counter`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("counter %d err %v", n, err)
	}
}
