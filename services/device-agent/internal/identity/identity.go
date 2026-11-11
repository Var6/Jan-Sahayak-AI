// Package identity manages the device's Ed25519 key pair and device id.
//
// The private key is generated on the device at first boot, stored with mode
// 0600 (a group/world-accessible key file is refused) and never leaves the
// device. Only the public key is registered with
// the central server (provisioning).
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/janmanindia/jan-sahayak/internal/ids"
)

// Identity is the device's credentials.
type Identity struct {
	DeviceID   string
	PrivateKey ed25519.PrivateKey
}

type fileFormat struct {
	DeviceID string `json:"device_id"`
	Seed     string `json:"ed25519_seed"` // base64, 32 bytes
}

// PublicKeyBase64 returns the public key for provisioning.
func (i Identity) PublicKeyBase64() string {
	return base64.StdEncoding.EncodeToString(i.PrivateKey.Public().(ed25519.PublicKey))
}

// LoadOrCreate reads the identity file, creating it atomically if missing.
//
// An existing file must not be group- or world-accessible (mode & 0o077):
// such a key may already have been copied, so it is refused rather than
// silently used or "repaired" (fix the permissions with chmod 600 if the
// exposure is known to be harmless, otherwise re-provision the device).
//
// Creation writes a temporary file, fsyncs it, renames it into place and
// fsyncs the directory. A temporary file left behind by a power cut during
// an earlier first boot is removed first, so it can never block startup.
func LoadOrCreate(path string) (Identity, bool, error) {
	b, err := readPrivate(path)
	if err == nil {
		id, err := parse(b)
		return id, false, err
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Identity{}, false, err
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return Identity{}, false, err
	}
	ff := fileFormat{DeviceID: ids.NewV4(), Seed: base64.StdEncoding.EncodeToString(seed)}
	out, _ := json.MarshalIndent(ff, "", "  ")
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Identity{}, false, err
	}
	tmp := path + ".tmp"
	// Stale leftover from an interrupted first boot: it never became the
	// identity (the rename did not happen), so it holds no registered key.
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Identity{}, false, fmt.Errorf("identity: remove stale %s: %w", tmp, err)
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Identity{}, false, err
	}
	if _, err := f.Write(out); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return Identity{}, false, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return Identity{}, false, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return Identity{}, false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return Identity{}, false, err
	}
	if err := syncDir(dir); err != nil {
		return Identity{}, false, fmt.Errorf("identity: fsync %s: %w", dir, err)
	}
	id, err := parse(out)
	return id, true, err
}

// readPrivate reads the key file after checking it is not group- or
// world-accessible.
func readPrivate(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("identity: %s has mode %04o; the private key must not be group/world accessible (expected 0600): refusing to use a possibly exposed key", path, fi.Mode().Perm())
	}
	const maxFile = 4 << 10
	b := make([]byte, maxFile+1)
	n, err := io.ReadFull(f, b)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if n > maxFile {
		return nil, fmt.Errorf("identity: %s is larger than %d bytes", path, maxFile)
	}
	return b[:n], nil
}

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

func parse(b []byte) (Identity, error) {
	var ff fileFormat
	if err := json.Unmarshal(b, &ff); err != nil {
		return Identity{}, fmt.Errorf("identity: parse: %w", err)
	}
	seed, err := base64.StdEncoding.DecodeString(ff.Seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		return Identity{}, errors.New("identity: bad seed")
	}
	if ids.Validate(ff.DeviceID) != nil {
		return Identity{}, errors.New("identity: bad device id")
	}
	return Identity{DeviceID: ff.DeviceID, PrivateKey: ed25519.NewKeyFromSeed(seed)}, nil
}
