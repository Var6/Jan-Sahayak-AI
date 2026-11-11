package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCreateThenLoadSameIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "identity.json")
	a, created, err := LoadOrCreate(path)
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v err %v", fi.Mode().Perm(), err)
	}
	b, created, err := LoadOrCreate(path)
	if err != nil || created || b.DeviceID != a.DeviceID || b.PublicKeyBase64() != a.PublicKeyBase64() {
		t.Fatalf("reload: created=%v err=%v", created, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary file left behind: %v", err)
	}
}

func TestStaleTmpIsReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	if err := os.WriteFile(path+".tmp", []byte("{partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, created, err := LoadOrCreate(path); err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
}

func TestGroupReadableKeyRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	if _, _, err := LoadOrCreate(path); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o660} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadOrCreate(path); err == nil {
			t.Fatalf("mode %04o accepted", mode)
		}
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreate(path); err != nil {
		t.Fatalf("0400 should be accepted: %v", err)
	}
}
