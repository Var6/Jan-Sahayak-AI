package deviceapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/ids"
	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/internal/syncproto"
)

func TestValidatePayload(t *testing.T) {
	sid := ids.NewV4()
	now := time.Now().UTC().Format(time.RFC3339)
	ok := []struct {
		typ string
		v   any
	}{
		{TypeSessionEnded, SessionEnded{SessionID: sid, Language: "hi", StartedAt: now, EndedAt: now, EndReason: "inactivity_timeout", Turns: 3}},
		{TypeTurn, Turn{SessionID: sid, Seq: 1, Language: "en", Source: "option", Intent: "off_topic", Resolution: "redirected"}},
		{TypeTurn, Turn{SessionID: sid, Seq: 2, Language: "hi", Source: "voice", Intent: "pension", Category: "pension", Service: "some-scheme-id", Resolution: "answered", UsedLLM: true}},
	}
	for _, c := range ok {
		b, _ := json.Marshal(c.v)
		if err := ValidatePayload(c.typ, 1, b); err != nil {
			t.Errorf("%s %s: %v", c.typ, b, err)
		}
	}
	bad := []struct {
		typ string
		ver int
		raw string
	}{
		{TypeTurn, 2, `{}`},
		{"other", 1, `{}`},
		{TypeTurn, 1, `{"session_id":"` + sid + `","seq":1,"language":"hi","source":"voice","intent":"x","name":"Ram"}`},
		{TypeTurn, 1, `{"session_id":"` + sid + `","seq":1,"language":"hi","source":"voice","intent":"Free Text"}`},
		{TypeTurn, 1, `{"session_id":"` + sid + `","seq":0,"language":"hi","source":"voice","intent":"x"}`},
		{TypeTurn, 1, `{"session_id":"` + sid + `","seq":1,"language":"hi","source":"voice","intent":"x"} {}`},
		{TypeSessionEnded, 1, `null`},
		{TypeTurn, 1, `{"session_id":"` + sid + `","seq":1,"language":"hi","source":"voice","Intent":"Free text Ram","intent":"x"}`},
		{TypeSessionEnded, 1, `[]`},
		{TypeSessionEnded, 1, `{"session_id":"` + sid + `","language":"hi","started_at":"` + now + `","ended_at":"2000-01-01T00:00:00Z","end_reason":"error","turns":0}`},
		{TypeSessionEnded, 1, "{\"session_id\":\"\xff\"}"},
	}
	for _, c := range bad {
		if err := ValidatePayload(c.typ, c.ver, []byte(c.raw)); err == nil {
			t.Errorf("accepted %s v%d %s", c.typ, c.ver, c.raw)
		}
	}
}

func TestSignHTTPVerifies(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	id := ids.NewV4()
	req, _ := http.NewRequest(http.MethodGet, "http://x"+KnowledgeBundlePath+"?since=2026-11-09.1", nil)
	if err := SignHTTP(req, id, priv, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !ValidNonce(req.Header.Get(syncproto.HeaderNonce)) {
		t.Fatal("nonce invalid")
	}
	verify := func(target string) error {
		return syncproto.VerifyRequest(pub, req.Method, target, req.Header.Get(syncproto.HeaderTS),
			req.Header.Get(syncproto.HeaderNonce), req.Header.Get(syncproto.HeaderSig), nil, time.Now())
	}
	if err := verify(SignedTarget(req.URL)); err != nil {
		t.Fatal(err)
	}
	// The query string is signed (security review finding 9): dropping or
	// altering it breaks the signature.
	if verify(req.URL.Path) == nil {
		t.Fatal("signature verified without its query string")
	}
	if verify(KnowledgeBundlePath+"?since=2020-01-01.1") == nil {
		t.Fatal("signature verified with an altered query string")
	}
	if ValidNonce("has space in it 1234") || ValidNonce("short") {
		t.Fatal("bad nonce accepted")
	}
}

// A request without a query signs exactly its path, so the syncproto
// event-upload signature (which signs syncproto.Path) is unchanged.
func TestSignedTargetWithoutQueryIsPath(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "http://x"+syncproto.Path, nil)
	if got := SignedTarget(req.URL); got != syncproto.Path {
		t.Fatalf("target %q", got)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	if err := SignHTTP(req, ids.NewV4(), priv, []byte("{}"), time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	want := syncproto.SignRequest(priv, http.MethodPost, syncproto.Path, 1700000000, req.Header.Get(syncproto.HeaderNonce), []byte("{}"))
	if req.Header.Get(syncproto.HeaderSig) != want {
		t.Fatal("SignHTTP without query differs from syncproto.SignRequest over the path")
	}
}

func TestBundleSignRoundTrip(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	keys := map[string]ed25519.PublicKey{"k1": priv.Public().(ed25519.PublicKey)}
	note := scheme.Text{Hi: "नोट <&>", En: "note <&>"}
	b := scheme.Bundle{BundleVersion: "2026-11-09.1", CreatedAt: "2026-11-09T00:00:00Z", Records: []scheme.Record{{
		ID: "fixture-a", Version: 2, Kind: scheme.KindService, Name: scheme.Text{Hi: "परीक्षण", En: "Fixture"},
		Aliases: []string{}, MandatoryDocs: []scheme.Doc{{Name: note, Note: &note}}, FeesNote: &note,
		Verification: scheme.Verification{Status: scheme.NeedsReview},
	}}}
	if err := SignBundle(priv, "k1", &b); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(b)
	var back scheme.Bundle
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if err := VerifyBundle(keys, back); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	back.BundleVersion = "2026-11-09.2"
	if VerifyBundle(keys, back) == nil {
		t.Fatal("modified version verified")
	}
	back.BundleVersion = b.BundleVersion
	back.KeyID = "k9"
	if VerifyBundle(keys, back) == nil {
		t.Fatal("unknown key verified")
	}
}

func TestHeartbeatValidate(t *testing.T) {
	n := int64(-1)
	h := Heartbeat{DeviceID: ids.NewV4(), SentAt: time.Now().UTC().Format(time.RFC3339), AgentVersion: "1", Overall: "HEALTHY"}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	h.Resources.DiskFreeBytes = &n
	h.Components = []ComponentHealth{{Name: "x\n", State: "OK"}}
	if err := h.Validate(); err == nil {
		t.Fatal("invalid heartbeat accepted")
	}
}

func TestReasonCodeAllowlist(t *testing.T) {
	for in, want := range map[string]string{
		"":                             "",
		"disk_low":                     "disk_low",
		"system_memory_low; throttled": "system_memory_low; throttled",
		"not started":                  DetailOther,
		"stt failed on: मेरा नाम 98765": DetailOther,
		"Disk_Low":              DetailOther,
		"a; b; c; d; e":         DetailOther,
		strings.Repeat("x", 65): DetailOther,
		"disk_low;throttled":    DetailOther,
		"stt_unreachable\nraw":  DetailOther,
	} {
		if got := ReasonCode(in); got != want {
			t.Errorf("ReasonCode(%q) = %q, want %q", in, got, want)
		}
	}
}
