// Package identity manages the device's Ed25519 key pair and device id.
//
// The private key is generated on the device at first boot, stored with mode
// 0600 and never leaves the device. Only the public key is registered with
// the central server (provisioning).
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
func LoadOrCreate(path string) (Identity, bool, error) {
	b, err := os.ReadFile(path)
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Identity{}, false, err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Identity{}, false, err
	}
	if _, err := f.Write(out); err != nil {
		f.Close()
		return Identity{}, false, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return Identity{}, false, err
	}
	f.Close()
	if err := os.Rename(tmp, path); err != nil {
		return Identity{}, false, err
	}
	id, err := parse(out)
	return id, true, err
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
