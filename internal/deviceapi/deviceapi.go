// Package deviceapi holds the device ↔ central wire types that are not part
// of the event-upload protocol itself (internal/syncproto): request signing
// helpers for the other signed device endpoints, the heartbeat body, the
// schemas of the anonymous analytics events, and knowledge-bundle signing.
//
// Both the device agent and the central API import this package so the
// contract cannot drift.
//
// Every signed device request uses the syncproto scheme: headers X-JS-Device,
// X-JS-Ts, X-JS-Nonce, X-JS-Sig, where the signature covers
// syncproto.SigningString(method, URL path (no query), ts, nonce, sha256(body)).
// A GET has an empty body.
package deviceapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/ids"
	"github.com/janmanindia/jan-sahayak/internal/syncproto"
)

// Endpoint paths (in addition to syncproto.Path).
const (
	HeartbeatPath       = "/v1/devices/heartbeat"
	KnowledgeBundlePath = "/v1/devices/knowledge-bundle"
)

// Limits.
const (
	MaxHeartbeatBytes = 64 << 10
	MaxBundleBytes    = 8 << 20
)

// NewNonce returns a random 32-hex-character nonce.
func NewNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ValidNonce reports whether a nonce has an allowed length and charset
// ([A-Za-z0-9_-]). The charset rule keeps nonces safe to store and log.
func ValidNonce(n string) bool {
	if len(n) < syncproto.MinNonceLength || len(n) > syncproto.MaxNonceLength {
		return false
	}
	for i := 0; i < len(n); i++ {
		c := n[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// SignHTTP sets the four authentication headers on req. body must be the
// exact bytes sent (nil for GET).
func SignHTTP(req *http.Request, deviceID string, key ed25519.PrivateKey, body []byte, now time.Time) error {
	if ids.Validate(deviceID) != nil {
		return errors.New("deviceapi: bad device id")
	}
	nonce, err := NewNonce()
	if err != nil {
		return err
	}
	ts := now.Unix()
	req.Header.Set(syncproto.HeaderDevice, deviceID)
	req.Header.Set(syncproto.HeaderTS, strconv.FormatInt(ts, 10))
	req.Header.Set(syncproto.HeaderNonce, nonce)
	req.Header.Set(syncproto.HeaderSig, syncproto.SignRequest(key, req.Method, req.URL.Path, ts, nonce, body))
	return nil
}
