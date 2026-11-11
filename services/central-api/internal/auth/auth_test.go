package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestHashAndVerify(t *testing.T) {
	ctx := context.Background()
	h, err := HashPassword(ctx, "a-long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("hash format %q", h)
	}
	h2, _ := HashPassword(ctx, "a-long-enough-password")
	if h == h2 {
		t.Fatal("salt not random")
	}
	if ok, err := VerifyPassword(ctx, h, "a-long-enough-password"); !ok || err != nil {
		t.Fatalf("verify: %v %v", ok, err)
	}
	if ok, _ := VerifyPassword(ctx, h, "a-long-enough-passwore"); ok {
		t.Fatal("wrong password verified")
	}
	if _, err := HashPassword(ctx, "short"); err == nil {
		t.Fatal("short password accepted")
	}
	if ok, err := VerifyPassword(ctx, dummyHash, "anything-at-all"); ok || err != nil {
		t.Fatalf("dummy hash must parse and never match: %v %v", ok, err)
	}
}

func TestVerifyRejectsHostileParams(t *testing.T) {
	ctx := context.Background()
	for _, h := range []string{
		"$argon2id$v=19$m=9999999,t=2,p=1$c29tZXNhbHRzb21lc2FsdA$Jm9Hhb3qkHxGnu7H8I1QXJcLwV1o5i5p7C2BqD3d4mE",
		"$argon2id$v=19$m=19456,t=99,p=1$c29tZXNhbHRzb21lc2FsdA$Jm9Hhb3qkHxGnu7H8I1QXJcLwV1o5i5p7C2BqD3d4mE",
		"$argon2i$v=19$m=19456,t=2,p=1$c29tZXNhbHRzb21lc2FsdA$Jm9Hhb3qkHxGnu7H8I1QXJcLwV1o5i5p7C2BqD3d4mE",
		"$argon2id$v=19$m=19456,t=2,p=1$$",
		"plaintext",
	} {
		if ok, err := VerifyPassword(ctx, h, "whatever-password"); ok || err == nil {
			t.Errorf("%q: want error, got %v %v", h, ok, err)
		}
	}
}

func TestSessionToken(t *testing.T) {
	tok, sum, err := NewSessionToken()
	if err != nil || len(tok) != 43 || len(sum) != 32 {
		t.Fatalf("%q %d %v", tok, len(sum), err)
	}
	got, ok := TokenSum(tok)
	if !ok || string(got) != string(sum) {
		t.Fatal("TokenSum mismatch")
	}
	if _, ok := TokenSum("short"); ok {
		t.Fatal("malformed token accepted")
	}
}

// A flood holding the shared slot cannot starve trusted callers: they get
// the reserved slot, while normal callers wait (and give up with ErrBusy).
func TestReservedSlotForTrustedCallers(t *testing.T) {
	h, err := HashPassword(context.Background(), "a-long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	sharedSlot <- struct{}{} // the flood holds the shared slot
	defer func() { <-sharedSlot }()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := VerifyPasswordPriority(ctx, h, "a-long-enough-password", PriorityNormal); !errors.Is(err, ErrBusy) {
		t.Fatalf("normal caller with shared slot taken: %v", err)
	}
	if err := VerifyDummy(ctx, "whatever-password", PriorityNormal); !errors.Is(err, ErrBusy) {
		t.Fatalf("dummy must report busy exactly like a real check: %v", err)
	}
	ok, err := VerifyPasswordPriority(context.Background(), h, "a-long-enough-password", PriorityTrusted)
	if err != nil || !ok {
		t.Fatalf("trusted caller: %v %v", ok, err)
	}
}
