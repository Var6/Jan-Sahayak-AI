package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGenerateLoadAndRotate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "k1.json")
	s, err := Generate(p, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(p, time.Now()); err == nil {
		t.Fatal("overwrote existing key")
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	l, err := LoadSigner(p)
	if err != nil || l.KeyID != s.KeyID || !l.Priv.Equal(s.Priv) {
		t.Fatalf("load: %v", err)
	}
	os.Chmod(p, 0o644)
	if _, err := LoadSigner(p); err == nil {
		t.Fatal("world-readable key accepted")
	}

	pubPath := filepath.Join(dir, "k1.pub.json")
	if err := os.WriteFile(pubPath, MarshalPublic(s.Public()), 0o644); err != nil {
		t.Fatal(err)
	}
	pub, err := LoadPublic(pubPath)
	if err != nil || pub.KeyID != s.KeyID || !pub.Key.Equal(s.Public().Key) {
		t.Fatalf("public: %v", err)
	}

	_, priv2, _ := ed25519.GenerateKey(rand.Reader)
	kr, err := NewKeyring(Signer{KeyID: "k2", Priv: priv2}, pub)
	if err != nil {
		t.Fatal(err)
	}
	vk := kr.VerifyKeys()
	if len(vk) != 2 || kr.Active().KeyID != "k2" {
		t.Fatalf("keyring %v", vk)
	}
	// Same id, different key → refused.
	if _, err := NewKeyring(Signer{KeyID: s.KeyID, Priv: priv2}, pub); err == nil {
		t.Fatal("conflicting key id accepted")
	}
}
