// Package syncproto is the wire contract between kiosk devices and the
// central API for uploading outbox events. Both sides import this package so
// the contract cannot drift.
//
// Protocol v1
//
//	POST /v1/sync/events
//	Headers: X-JS-Device, X-JS-Ts (unix seconds), X-JS-Nonce, X-JS-Sig
//	Body:    BatchRequest (JSON)
//
// The device signs SigningString(method, path, ts, nonce, sha256(body)) with
// its Ed25519 key. The server answers with a BatchAck whose Signature is the
// server's Ed25519 signature over AckSigningBytes(ack). The device marks an
// event ACKNOWLEDGED only if:
//
//  1. the ack signature verifies against the pinned server public key;
//  2. ack.BatchID and ack.DeviceID match the request;
//  3. the result for that event id is "accepted" or "duplicate";
//  4. the echoed payload hash equals the local payload hash.
//
// Anything else leaves the event in the outbox for retry.
package syncproto

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/ids"
)

// Version is the protocol version string included in signatures.
const Version = "JS1"

// Path is the batch upload endpoint.
const Path = "/v1/sync/events"

// Header names.
const (
	HeaderDevice = "X-JS-Device"
	HeaderTS     = "X-JS-Ts"
	HeaderNonce  = "X-JS-Nonce"
	HeaderSig    = "X-JS-Sig"
)

// Limits shared by both sides.
const (
	MaxBatchEvents  = 200
	MaxPayloadBytes = 16 << 10 // per event
	MaxBodyBytes    = 4 << 20  // per request
	MaxClockSkew    = 5 * time.Minute
	MaxEventTypeLen = 64
	MaxReasonLength = 200
	MaxNonceLength  = 64
	MinNonceLength  = 16
)

// Event is one outbox event on the wire.
type Event struct {
	ID            string `json:"id"`
	Type          string `json:"type"`
	SchemaVersion int    `json:"schema_version"`
	CreatedAt     string `json:"created_at"` // RFC 3339 UTC
	Payload       []byte `json:"payload"`    // raw JSON object (base64 in JSON encoding)
	PayloadSHA256 string `json:"payload_sha256"`
}

// BatchRequest is the upload body.
type BatchRequest struct {
	BatchID  string  `json:"batch_id"`
	DeviceID string  `json:"device_id"`
	SentAt   string  `json:"sent_at"`
	Events   []Event `json:"events"`
}

// Result statuses.
const (
	StatusAccepted  = "accepted"  // stored now
	StatusDuplicate = "duplicate" // already stored earlier with the same hash
	StatusConflict  = "conflict"  // same id already stored with a different hash
	StatusRejected  = "rejected"  // failed validation; will never be accepted
)

// Result is the server's verdict on one event.
type Result struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	PayloadSHA256 string `json:"payload_sha256,omitempty"`
	Receipt       string `json:"receipt,omitempty"` // server-side id, for audit
	Reason        string `json:"reason,omitempty"`
}

// BatchAck is the server's signed acknowledgement.
type BatchAck struct {
	BatchID    string   `json:"batch_id"`
	DeviceID   string   `json:"device_id"`
	ServerTime string   `json:"server_time"`
	Results    []Result `json:"results"`
	KeyID      string   `json:"key_id"`    // which server key signed it (rotation)
	Signature  string   `json:"signature"` // base64 Ed25519 over AckSigningBytes
}

// PayloadHash returns the lowercase hex SHA-256 of a payload.
func PayloadHash(payload []byte) string {
	h := sha256.Sum256(payload)
	return hex.EncodeToString(h[:])
}

// BodyHash returns the lowercase hex SHA-256 of a request body.
func BodyHash(body []byte) string { return PayloadHash(body) }

// SigningString builds the string a device signs for a request.
func SigningString(method, path string, ts int64, nonce, bodySHA256 string) []byte {
	return []byte(strings.Join([]string{Version, method, path, strconv.FormatInt(ts, 10), nonce, bodySHA256}, "\n"))
}

// SignRequest returns the X-JS-Sig value.
func SignRequest(key ed25519.PrivateKey, method, path string, ts int64, nonce string, body []byte) string {
	sig := ed25519.Sign(key, SigningString(method, path, ts, nonce, BodyHash(body)))
	return base64.StdEncoding.EncodeToString(sig)
}

// Errors from request verification.
var (
	ErrBadSignature = errors.New("syncproto: bad signature")
	ErrClockSkew    = errors.New("syncproto: timestamp outside allowed skew")
	ErrBadNonce     = errors.New("syncproto: bad nonce")
)

// VerifyRequest checks a device request signature and timestamp. Nonce
// replay must additionally be checked by the caller against a store.
func VerifyRequest(pub ed25519.PublicKey, method, path, tsHeader, nonce, sigHeader string, body []byte, now time.Time) error {
	ts, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		return ErrClockSkew
	}
	skew := now.Sub(time.Unix(ts, 0))
	if skew > MaxClockSkew || skew < -MaxClockSkew {
		return ErrClockSkew
	}
	if len(nonce) < MinNonceLength || len(nonce) > MaxNonceLength {
		return ErrBadNonce
	}
	sig, err := base64.StdEncoding.DecodeString(sigHeader)
	if err != nil || len(sig) != ed25519.SignatureSize || len(pub) != ed25519.PublicKeySize {
		return ErrBadSignature
	}
	if !ed25519.Verify(pub, SigningString(method, path, ts, nonce, BodyHash(body)), sig) {
		return ErrBadSignature
	}
	return nil
}

// AckSigningBytes is the canonical byte string the server signs. Results are
// sorted by id so the encoding is independent of JSON field order.
func AckSigningBytes(a BatchAck) []byte {
	rs := append([]Result(nil), a.Results...)
	sort.Slice(rs, func(i, j int) bool { return rs[i].ID < rs[j].ID })
	var b strings.Builder
	b.WriteString(Version + "-ACK\n")
	b.WriteString(a.BatchID + "\n" + a.DeviceID + "\n" + a.ServerTime + "\n" + a.KeyID + "\n")
	for _, r := range rs {
		// Fields cannot contain '\n' or '|' (validated), so this is unambiguous.
		b.WriteString(r.ID + "|" + r.Status + "|" + r.PayloadSHA256 + "|" + r.Receipt + "\n")
	}
	return []byte(b.String())
}

// SignAck fills in a.Signature.
func SignAck(key ed25519.PrivateKey, a *BatchAck) {
	a.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, AckSigningBytes(*a)))
}

// ErrBadAck reports an acknowledgement the device must not trust.
var ErrBadAck = errors.New("syncproto: acknowledgement failed verification")

// VerifyAck checks the server signature and that the ack belongs to req.
// It returns the per-event results keyed by id, restricted to ids that were
// in the request (unknown ids in the ack make the whole ack invalid).
func VerifyAck(serverKeys map[string]ed25519.PublicKey, req BatchRequest, a BatchAck) (map[string]Result, error) {
	pub, ok := serverKeys[a.KeyID]
	if !ok {
		return nil, fmt.Errorf("%w: unknown key id %q", ErrBadAck, a.KeyID)
	}
	sig, err := base64.StdEncoding.DecodeString(a.Signature)
	if err != nil || !ed25519.Verify(pub, AckSigningBytes(a), sig) {
		return nil, fmt.Errorf("%w: signature", ErrBadAck)
	}
	if a.BatchID != req.BatchID || a.DeviceID != req.DeviceID {
		return nil, fmt.Errorf("%w: batch/device mismatch", ErrBadAck)
	}
	inReq := make(map[string]string, len(req.Events))
	for _, e := range req.Events {
		inReq[e.ID] = e.PayloadSHA256
	}
	out := make(map[string]Result, len(a.Results))
	for _, r := range a.Results {
		if _, ok := inReq[r.ID]; !ok {
			return nil, fmt.Errorf("%w: result for unknown event %s", ErrBadAck, r.ID)
		}
		if _, dup := out[r.ID]; dup {
			return nil, fmt.Errorf("%w: duplicate result for %s", ErrBadAck, r.ID)
		}
		out[r.ID] = r
	}
	return out, nil
}

// IsDurablyStored reports whether a verified result proves the server holds
// exactly our payload.
func IsDurablyStored(r Result, localHash string) bool {
	return (r.Status == StatusAccepted || r.Status == StatusDuplicate) &&
		r.PayloadSHA256 == localHash && r.Receipt != ""
}

// Validate checks a batch request's structure (used by the server; the
// device runs it before sending as a self-check).
func (b BatchRequest) Validate() error {
	if ids.Validate(b.BatchID) != nil {
		return errors.New("batch_id must be a uuid")
	}
	if ids.Validate(b.DeviceID) != nil {
		return errors.New("device_id must be a uuid")
	}
	if len(b.Events) == 0 || len(b.Events) > MaxBatchEvents {
		return fmt.Errorf("batch must contain 1..%d events", MaxBatchEvents)
	}
	return nil
}

// Validate checks one event. Returns a short reason suitable for Result.Reason.
func (e Event) Validate() error {
	if ids.Validate(e.ID) != nil {
		return errors.New("id must be a uuid")
	}
	if e.Type == "" || len(e.Type) > MaxEventTypeLen || strings.ContainsAny(e.Type, "\n|") {
		return errors.New("bad type")
	}
	if e.SchemaVersion < 1 {
		return errors.New("bad schema_version")
	}
	if _, err := time.Parse(time.RFC3339Nano, e.CreatedAt); err != nil {
		return errors.New("created_at must be RFC 3339")
	}
	if len(e.Payload) == 0 || len(e.Payload) > MaxPayloadBytes {
		return errors.New("payload size out of range")
	}
	if PayloadHash(e.Payload) != e.PayloadSHA256 {
		return errors.New("payload hash mismatch")
	}
	return nil
}
