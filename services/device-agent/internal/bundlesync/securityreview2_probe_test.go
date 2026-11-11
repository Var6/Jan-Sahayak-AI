//go:build securityreview2

// Independent security-review probes for bundle sync (build tag
// securityreview2). Records are the knowledge package's FIXTURE records.
//
//	go test -tags securityreview2 -race -count=1 -run Probe ./services/device-agent/internal/bundlesync/
package bundlesync

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Probe: a signed bundle padded with case-variant duplicate keys still
// verifies (signature is over the re-encoded decoded content), and what is
// loaded is exactly the signed content, not the padding.
func TestProbeDuplicateKeyPaddingLoadsSignedContentOnly(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	recs := fixtureRecords(t)
	body := e.signed("2026-11-12.1", recs[:2], e.srvKey)
	// Prepend a decoy "Records" key; encoding/json keeps the LAST match
	// (the genuine "records"), so the decoy must be ignored.
	decoy := []byte(`{"Records":[{"id":"decoy-scheme"}],`)
	padded := append(decoy, body[1:]...)
	e.central.publish(padded, "2026-11-12.1")
	if _, err := e.w.CheckOnce(ctx); err != nil {
		t.Fatalf("padded bundle refused: %v", err)
	}
	n := 0
	_ = e.st.DB.QueryRow(`SELECT count(*) FROM schemes WHERE id = 'decoy-scheme'`).Scan(&n)
	if n != 0 {
		t.Fatal("decoy record loaded")
	}
	// Now the decoy AFTER the genuine key: last one wins → decoy content,
	// which no longer matches the signature.
	e2 := newEnv(t)
	tail := append(append([]byte{}, body[:len(body)-1]...), []byte(`,"Records":[{"id":"decoy-scheme"}]}`)...)
	e2.central.publish(tail, "2026-11-12.1")
	if _, err := e2.w.CheckOnce(ctx); err == nil || !strings.HasPrefix(err.Error(), ReasonSignature) {
		t.Fatalf("decoy-after bundle: %v", err)
	}
	// The kept "exact signed file" is the padded bytes (informational).
	if b, _ := os.ReadFile(e.cache); bytes.Equal(b, padded) {
		t.Log("note: cached file keeps unsigned padding bytes; content is re-verified on restore")
	}
}

// Probe: a device with an empty database and no cache file accepts an
// older (genuinely signed) bundle — there is no rollback floor outside the
// database. Informational: documents the trust boundary.
func TestProbeFreshDeviceAcceptsOlderSignedBundle(t *testing.T) {
	e := newEnv(t)
	recs := fixtureRecords(t)
	e.central.publish(e.signed("2026-01-01.1", recs[:2], e.srvKey), "2026-01-01.1")
	if _, err := e.w.CheckOnce(context.Background()); err != nil {
		t.Fatalf("err %v", err)
	}
	t.Logf("fresh device loaded %s (no floor; freshness relies on knowledge.stale_after)", e.version())
}

// Probe: a symlinked cache path is replaced, never written through.
func TestProbeCacheSymlinkNotFollowed(t *testing.T) {
	e := newEnv(t)
	target := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(e.cache), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, e.cache); err != nil {
		t.Fatal(err)
	}
	recs := fixtureRecords(t)
	e.central.publish(e.signed("2026-11-12.1", recs[:2], e.srvKey), "2026-11-12.1")
	if _, err := e.w.CheckOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("symlink target overwritten: %q", b)
	}
	if fi, _ := os.Lstat(e.cache); fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("cache path still a symlink")
	}
}

// Probe: a tampered cache file is refused on restore and the DB is kept.
func TestProbeTamperedCacheRefused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	recs := fixtureRecords(t)
	e.central.publish(e.signed("2026-11-12.1", recs[:2], e.srvKey), "2026-11-12.1")
	if _, err := e.w.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(e.cache)
	b = bytes.Replace(b, []byte("2026-11-12.1"), []byte("2026-11-13.1"), 1)
	if err := os.WriteFile(e.cache, b, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, loaded, err := e.w.RestoreFromDisk(ctx); err == nil || loaded {
		t.Fatalf("tampered cache: loaded=%v err=%v", loaded, err)
	}
	if e.version() != "2026-11-12.1" {
		t.Fatalf("version %s", e.version())
	}
}
