package deviceapi

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/janmanindia/jan-sahayak/internal/scheme"
)

// ErrBadBundle reports a knowledge bundle the device must not load.
var ErrBadBundle = errors.New("deviceapi: bundle failed verification")

// BundleSigningBytes is the canonical byte string the server signs for a
// knowledge bundle: a header line per metadata field followed by the
// encoding/json encoding of the records. Go's encoder is deterministic for
// scheme.Record (struct fields in declaration order, no maps), so a device
// that decodes the bundle and re-encodes the records gets the same bytes.
// A record field unknown to an older device would be dropped on decode and
// make verification fail — which is the safe outcome.
func BundleSigningBytes(b scheme.Bundle) ([]byte, error) {
	for _, f := range []string{b.BundleVersion, b.CreatedAt, b.KeyID} {
		if strings.ContainsAny(f, "\n") {
			return nil, errors.New("deviceapi: bundle metadata contains newline")
		}
	}
	recs := b.Records
	if recs == nil {
		recs = []scheme.Record{}
	}
	rj, err := json.Marshal(recs)
	if err != nil {
		return nil, err
	}
	var sb strings.Builder
	sb.WriteString("JS1-BUNDLE\n")
	sb.WriteString(b.BundleVersion + "\n" + b.CreatedAt + "\n" + b.KeyID + "\n")
	sb.Write(rj)
	return []byte(sb.String()), nil
}

// SignBundle sets b.KeyID and b.Signature.
func SignBundle(key ed25519.PrivateKey, keyID string, b *scheme.Bundle) error {
	b.KeyID = keyID
	msg, err := BundleSigningBytes(*b)
	if err != nil {
		return err
	}
	b.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, msg))
	return nil
}

// VerifyBundle checks a bundle signature against pinned server keys.
func VerifyBundle(keys map[string]ed25519.PublicKey, b scheme.Bundle) error {
	pub, ok := keys[b.KeyID]
	if !ok {
		return fmt.Errorf("%w: unknown key id %q", ErrBadBundle, b.KeyID)
	}
	sig, err := base64.StdEncoding.DecodeString(b.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w: malformed signature", ErrBadBundle)
	}
	msg, err := BundleSigningBytes(b)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadBundle, err)
	}
	if !ed25519.Verify(pub, msg, sig) {
		return fmt.Errorf("%w: signature", ErrBadBundle)
	}
	return nil
}
