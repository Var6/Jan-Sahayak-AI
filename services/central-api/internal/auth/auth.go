// Package auth implements staff password hashing (argon2id) and session
// tokens for the admin API.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters: OWASP 2023+ minimum profile (19 MiB, t=2, p=1).
// Chosen for a small VPS; raise memory when the server has headroom. Old
// hashes keep verifying because parameters are stored in each hash.
const (
	argonMemoryKiB = 19 * 1024
	argonTime      = 2
	argonThreads   = 1
	argonKeyLen    = 32
	saltLen        = 16

	// Upper bounds accepted when parsing stored hashes, so a tampered hash
	// cannot make verification consume unbounded memory/CPU.
	maxMemoryKiB = 256 * 1024
	maxTime      = 10
	maxThreads   = 16
)

// Password policy.
const (
	MinPasswordLen = 12
	MaxPasswordLen = 128
)

// hashSlots bounds concurrent argon2 computations (each uses ~19 MiB).
var hashSlots = make(chan struct{}, 2)

// ErrBusy is returned when the hashing slots stay full beyond ctx.
var ErrBusy = errors.New("auth: password hashing busy")

func acquire(ctx context.Context) error {
	select {
	case hashSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ErrBusy
	}
}

func release() { <-hashSlots }

// CheckPasswordPolicy validates a new password.
func CheckPasswordPolicy(pw string) error {
	if !utf8.ValidString(pw) {
		return errors.New("password must be valid UTF-8")
	}
	if n := utf8.RuneCountInString(pw); n < MinPasswordLen || len(pw) > MaxPasswordLen {
		return fmt.Errorf("password must be %d..%d characters", MinPasswordLen, MaxPasswordLen)
	}
	return nil
}

// HashPassword returns a PHC-format argon2id hash.
func HashPassword(ctx context.Context, pw string) (string, error) {
	if err := CheckPasswordPolicy(pw); err != nil {
		return "", err
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	if err := acquire(ctx); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	release()
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// dummyHash is verified against when the user does not exist, so response
// time does not reveal which usernames exist.
var dummyHash = "$argon2id$v=19$m=19456,t=2,p=1$c29tZXNhbHRzb21lc2FsdA$Jm9Hhb3qkHxGnu7H8I1QXJcLwV1o5i5p7C2BqD3d4mE"

// VerifyDummy burns the same work as a real verification.
func VerifyDummy(ctx context.Context, pw string) {
	_, _ = VerifyPassword(ctx, dummyHash, pw)
}

// VerifyPassword checks pw against an encoded hash in constant time.
func VerifyPassword(ctx context.Context, encoded, pw string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("auth: unsupported hash format")
	}
	var v int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &v); err != nil || v != argon2.Version {
		return false, errors.New("auth: unsupported argon2 version")
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, errors.New("auth: bad argon2 params")
	}
	if m < 8 || m > maxMemoryKiB || t < 1 || t > maxTime || p < 1 || p > maxThreads {
		return false, errors.New("auth: argon2 params out of bounds")
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return false, errors.New("auth: bad salt")
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) < 16 || len(want) > 64 {
		return false, errors.New("auth: bad hash")
	}
	if len(pw) > MaxPasswordLen {
		return false, nil
	}
	if err := acquire(ctx); err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	release()
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NewSessionToken returns a 32-byte random token (base64url, for the
// cookie) and its SHA-256 (for the database).
func NewSessionToken() (token string, sum []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(token))
	return token, h[:], nil
}

// TokenSum hashes a cookie token for lookup. ok is false for malformed tokens.
func TokenSum(token string) (sum []byte, ok bool) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(b) != 32 {
		return nil, false
	}
	h := sha256.Sum256([]byte(token))
	return h[:], true
}
