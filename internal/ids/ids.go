// Package ids generates RFC 9562 UUIDs without external dependencies.
package ids

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

var (
	mu     sync.Mutex
	lastMS int64
	seq    uint16
)

// NewV7 returns a time-ordered UUIDv7 string. IDs created on the same
// device sort by creation time, which keeps outbox scans and server merges
// cheap. A 12-bit counter guarantees monotonicity within one millisecond.
func NewV7() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("ids: crypto/rand failed: " + err.Error())
	}
	mu.Lock()
	ms := time.Now().UnixMilli()
	if ms <= lastMS {
		seq++
		if seq > 0x0fff { // counter exhausted: borrow the next millisecond
			lastMS++
			seq = 0
		}
		ms = lastMS
	} else {
		lastMS = ms
		seq = uint16(b[6])<<8 | uint16(b[7])
		seq &= 0x07ff // start in lower half so increments rarely overflow
	}
	s := seq
	mu.Unlock()

	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = 0x70 | byte(s>>8)&0x0f
	b[7] = byte(s)
	b[8] = (b[8] & 0x3f) | 0x80
	return format(b)
}

// NewV4 returns a random UUIDv4 (for identifiers that must not reveal time).
func NewV4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("ids: crypto/rand failed: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return format(b)
}

func format(b [16]byte) string {
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}

// ErrInvalid is returned by Validate for malformed UUIDs.
var ErrInvalid = errors.New("ids: invalid uuid")

// Validate checks canonical lowercase 8-4-4-4-12 UUID syntax.
func Validate(s string) error {
	if len(s) != 36 {
		return ErrInvalid
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return ErrInvalid
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return ErrInvalid
			}
		}
	}
	return nil
}
