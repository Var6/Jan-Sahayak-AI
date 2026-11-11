//go:build reliabilityrepro

package store

// Reliability review repro (run with -tags reliabilityrepro). Expected to
// FAIL until fixed: Open treats any error from PRAGMA quick_check (e.g. a
// permission problem left by a technician's `sudo sqlite3`, EIO, a locked
// file) as corruption, quarantines the live database and starts from an
// older backup or an empty file, so pending outbox events silently leave the
// live database.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReproTransientOpenErrorIsNotCorruption(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "device.db")
	s, _, err := Open(ctx, Options{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	insertEvent(t, s, "pending-1", "PENDING")
	s.Close()

	if err := os.Chmod(p, 0o000); err != nil { // e.g. wrong owner after manual repair
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o640) })

	s2, rec, err := Open(ctx, Options{Path: p})
	if s2 != nil {
		defer s2.Close()
	}
	if rec != nil {
		t.Fatalf("an unreadable (not corrupt) database was quarantined: %+v (err=%v)", *rec, err)
	}
	if err == nil {
		t.Fatal("expected Open to fail loudly on an unreadable database")
	}
}
