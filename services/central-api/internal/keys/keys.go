// Package keys manages the server's Ed25519 signing keys.
//
// The active key signs sync acknowledgements and knowledge bundles. Older
// (rotated) keys are kept as public keys only, so stored bundles signed by
// them can still be verified. Devices pin the set of server public keys
// (key id → public key) in their configuration; rotation is: generate a new
// key, ship its public key to all devices, then switch the server to it.
//
// Private key file (JSON, mode 0600, created by `central-admin gen-server-key`):
//
//	{"key_id": "srv-20261109-1a2b", "ed25519_seed": "<base64 32 bytes>", "created_at": "..."}
//
// Public key file (JSON, from `central-admin server-public-key`):
//
//	{"key_id": "srv-20261109-1a2b", "public_key": "<base64 32 bytes>"}
package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var keyIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Signer is a key that can sign.
type Signer struct {
	KeyID string
	Priv  ed25519.PrivateKey
}

// Public returns the public half.
func (s Signer) Public() PublicKey {
	return PublicKey{KeyID: s.KeyID, Key: s.Priv.Public().(ed25519.PublicKey)}
}

// PublicKey is a verification key.
type PublicKey struct {
	KeyID string
	Key   ed25519.PublicKey
}

type privFile struct {
	KeyID     string `json:"key_id"`
	Seed      string `json:"ed25519_seed"`
	CreatedAt string `json:"created_at,omitempty"`
}

type pubFile struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
}

// Generate creates a new key and writes it to path (which must not exist).
func Generate(path string, now time.Time) (Signer, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return Signer{}, err
	}
	suffix := make([]byte, 2)
	if _, err := rand.Read(suffix); err != nil {
		return Signer{}, err
	}
	s := Signer{
		KeyID: "srv-" + now.UTC().Format("20060102") + "-" + hex.EncodeToString(suffix),
		Priv:  ed25519.NewKeyFromSeed(seed),
	}
	b, _ := json.MarshalIndent(privFile{KeyID: s.KeyID, Seed: base64.StdEncoding.EncodeToString(seed),
		CreatedAt: now.UTC().Format(time.RFC3339)}, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Signer{}, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Signer{}, fmt.Errorf("keys: create %s: %w", path, err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		os.Remove(path)
		return Signer{}, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return Signer{}, err
	}
	return s, f.Close()
}

// LoadSigner reads a private key file. Files readable by group/other are
// refused.
func LoadSigner(path string) (Signer, error) {
	st, err := os.Stat(path)
	if err != nil {
		return Signer{}, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return Signer{}, fmt.Errorf("keys: %s must not be accessible by group/other (mode %v); chmod 600", path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Signer{}, err
	}
	var pf privFile
	if err := json.Unmarshal(b, &pf); err != nil {
		return Signer{}, fmt.Errorf("keys: parse %s: %w", path, err)
	}
	if !keyIDRe.MatchString(pf.KeyID) {
		return Signer{}, errors.New("keys: bad key_id")
	}
	seed, err := base64.StdEncoding.DecodeString(pf.Seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		return Signer{}, errors.New("keys: bad ed25519_seed")
	}
	return Signer{KeyID: pf.KeyID, Priv: ed25519.NewKeyFromSeed(seed)}, nil
}

// MarshalPublic encodes a public key file.
func MarshalPublic(p PublicKey) []byte {
	b, _ := json.MarshalIndent(pubFile{KeyID: p.KeyID, PublicKey: base64.StdEncoding.EncodeToString(p.Key)}, "", "  ")
	return append(b, '\n')
}

// LoadPublic reads a public key file.
func LoadPublic(path string) (PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return PublicKey{}, err
	}
	var pf pubFile
	if err := json.Unmarshal(b, &pf); err != nil {
		return PublicKey{}, fmt.Errorf("keys: parse %s: %w", path, err)
	}
	if !keyIDRe.MatchString(pf.KeyID) {
		return PublicKey{}, errors.New("keys: bad key_id")
	}
	k, err := base64.StdEncoding.DecodeString(pf.PublicKey)
	if err != nil || len(k) != ed25519.PublicKeySize {
		return PublicKey{}, errors.New("keys: bad public_key")
	}
	return PublicKey{KeyID: pf.KeyID, Key: ed25519.PublicKey(k)}, nil
}

// Keyring is the active signer plus every key accepted for verification.
type Keyring struct {
	active Signer
	verify map[string]ed25519.PublicKey
}

// NewKeyring builds a keyring. Duplicate key ids with different keys are
// refused.
func NewKeyring(active Signer, retired ...PublicKey) (*Keyring, error) {
	if !keyIDRe.MatchString(active.KeyID) || len(active.Priv) != ed25519.PrivateKeySize {
		return nil, errors.New("keys: invalid active key")
	}
	k := &Keyring{active: active, verify: map[string]ed25519.PublicKey{}}
	k.verify[active.KeyID] = active.Priv.Public().(ed25519.PublicKey)
	for _, p := range retired {
		if old, ok := k.verify[p.KeyID]; ok {
			if !old.Equal(p.Key) {
				return nil, fmt.Errorf("keys: key id %q used for two different keys", p.KeyID)
			}
			continue
		}
		k.verify[p.KeyID] = p.Key
	}
	return k, nil
}

// Active returns the signing key.
func (k *Keyring) Active() Signer { return k.active }

// VerifyKeys returns a copy of the verification key map (the same shape the
// device pins: key id → public key).
func (k *Keyring) VerifyKeys() map[string]ed25519.PublicKey {
	out := make(map[string]ed25519.PublicKey, len(k.verify))
	for id, p := range k.verify {
		out[id] = p
	}
	return out
}
