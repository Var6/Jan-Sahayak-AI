//go:build securityreview

// Security-review probes (independent reviewer, 2026-11-09). They assert
// the SECURE/robust behaviour and currently FAIL, documenting findings.
//
//	go test -race -tags securityreview -run SecReview ./services/device-agent/internal/identity/
package identity

import (
	"os"
	"path/filepath"
	"testing"
)

// A crash between creating identity.json.tmp (O_EXCL) and the rename leaves
// the .tmp behind; every later start then fails with EEXIST and the kiosk
// never boots (Restart=always loops until StartLimitBurst).
func TestSecReviewStaleTmpBlocksStartupForever(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.json")
	if err := os.WriteFile(path+".tmp", []byte("{partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreate(path); err != nil {
		t.Fatalf("FINDING: stale %s.tmp makes identity creation fail permanently: %v", filepath.Base(path), err)
	}
}

// An existing key file that is group/world readable is loaded silently
// (the central server's keys.LoadSigner refuses this case).
func TestSecReviewWorldReadableKeyAccepted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.json")
	if _, _, err := LoadOrCreate(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreate(path); err == nil {
		t.Fatal("FINDING: identity key with mode 0644 was accepted without error")
	}
}
