package server

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/deviceapi"
	"github.com/janmanindia/jan-sahayak/internal/ids"
	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/internal/syncproto"
	"github.com/janmanindia/jan-sahayak/services/central-api/internal/keys"
)

// TestEndToEndDeviceProtocol plays the device side using only the shared
// contract packages (syncproto, deviceapi) plus ed25519 — the same calls the
// device agent's syncer makes — and verifies every server answer exactly
// as the device does, with a pinned server key map.
func TestEndToEndDeviceProtocol(t *testing.T) {
	e := newEnv(t)
	d := e.newDevice("kiosk-e2e")
	pinned := map[string]ed25519.PublicKey{e.signer.KeyID: e.signer.Priv.Public().(ed25519.PublicKey)}

	post := func(path string, body []byte) (*http.Response, []byte) {
		req, _ := http.NewRequest(http.MethodPost, e.hs.URL+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if err := deviceapi.SignHTTP(req, d.id, d.priv, body, time.Now()); err != nil {
			t.Fatal(err)
		}
		resp, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp, b
	}

	// --- 1. Outbox upload: 120 events, as the device's Claim would produce.
	var evs []syncproto.Event
	localHash := map[string]string{}
	for s := 0; s < 40; s++ {
		sid := ids.NewV4()
		evs = append(evs,
			mkEvent(deviceapi.TypeTurn, turn(sid, 1, "certificate", "answered")),
			mkEvent(deviceapi.TypeTurn, turn(sid, 2, "certificate", "escalated")),
			mkEvent(deviceapi.TypeSessionEnded, sessionEnded(sid, "hi", 2)))
	}
	for _, ev := range evs {
		localHash[ev.ID] = syncproto.PayloadHash(ev.Payload) // device recomputes from its own row
	}
	req := syncproto.BatchRequest{BatchID: ids.NewV7(), DeviceID: d.id, SentAt: time.Now().UTC().Format(time.RFC3339), Events: evs}
	if err := req.Validate(); err != nil { // device self-check before sending
		t.Fatal(err)
	}
	body, _ := json.Marshal(req)
	resp, rb := post(syncproto.Path, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload: %d %s", resp.StatusCode, rb)
	}
	var ack syncproto.BatchAck
	if err := json.Unmarshal(rb, &ack); err != nil {
		t.Fatal(err)
	}
	results, err := syncproto.VerifyAck(pinned, req, ack)
	if err != nil {
		t.Fatalf("device rejects ack: %v", err)
	}
	for _, ev := range evs {
		if !syncproto.IsDurablyStored(results[ev.ID], localHash[ev.ID]) {
			t.Fatalf("event %s not durably stored: %+v", ev.ID, results[ev.ID])
		}
	}

	// A device that pinned only a different key must refuse the ack.
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := syncproto.VerifyAck(map[string]ed25519.PublicKey{e.signer.KeyID: otherPriv.Public().(ed25519.PublicKey)}, req, ack); !errors.Is(err, syncproto.ErrBadAck) {
		t.Fatalf("ack verified with wrong pinned key: %v", err)
	}
	// Tampering with any result invalidates the signature.
	forged := ack
	forged.Results = append([]syncproto.Result(nil), ack.Results...)
	forged.Results[0].Status = syncproto.StatusConflict
	if _, err := syncproto.VerifyAck(pinned, req, forged); !errors.Is(err, syncproto.ErrBadAck) {
		t.Fatal("forged ack verified")
	}

	// --- 2. Key rotation: server switches to a new key; device pins both.
	_, newPriv, _ := ed25519.GenerateKey(rand.Reader)
	newSigner := keys.Signer{KeyID: "test-k2", Priv: newPriv}
	kr2, err := keys.NewKeyring(newSigner, e.signer.Public())
	if err != nil {
		t.Fatal(err)
	}
	e.srv.SetKeyring(kr2)
	pinned[newSigner.KeyID] = newPriv.Public().(ed25519.PublicKey)
	ev := mkEvent(deviceapi.TypeSessionEnded, sessionEnded(ids.NewV4(), "en", 0))
	req2 := syncproto.BatchRequest{BatchID: ids.NewV7(), DeviceID: d.id, SentAt: time.Now().UTC().Format(time.RFC3339), Events: []syncproto.Event{ev}}
	body, _ = json.Marshal(req2)
	_, rb = post(syncproto.Path, body)
	var ack2 syncproto.BatchAck
	mustJSON(t, rb, &ack2)
	if ack2.KeyID != "test-k2" {
		t.Fatalf("ack key id %q", ack2.KeyID)
	}
	if res, err := syncproto.VerifyAck(pinned, req2, ack2); err != nil || !syncproto.IsDurablyStored(res[ev.ID], ev.PayloadSHA256) {
		t.Fatalf("rotated ack: %v %+v", err, res)
	}

	// --- 3. Heartbeat.
	free, total := int64(10<<30), int64(32<<30)
	temp := 51.5
	hb := deviceapi.Heartbeat{DeviceID: d.id, SentAt: time.Now().UTC().Format(time.RFC3339), AgentVersion: "0.1.0-test",
		OS: "linux/arm64", Overall: "DEGRADED",
		Models:     map[string]string{"llm": "test-model"},
		Components: []deviceapi.ComponentHealth{{Name: "database", State: "HEALTHY", Critical: true}, {Name: "llm", State: "UNAVAILABLE", Detail: "not started"}},
		Outbox:     deviceapi.OutboxCounts{Pending: 3, Failed: 1, Total: 4, Capacity: 100000},
		Resources:  deviceapi.Resources{DiskFreeBytes: &free, DiskTotalBytes: &total, CPUTempC: &temp}}
	body, _ = json.Marshal(hb)
	resp, rb = post(deviceapi.HeartbeatPath, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", resp.StatusCode, rb)
	}
	var hbr deviceapi.HeartbeatResponse
	mustJSON(t, rb, &hbr)
	if hbr.ServerTime == "" || hbr.LatestBundleVersion != "" {
		t.Fatalf("heartbeat response %+v", hbr)
	}
	if n := e.count(`SELECT count(*) FROM device_heartbeats WHERE device_id = $1 AND outbox_pending = 3 AND overall_health = 'DEGRADED'
		AND disk_free_bytes = $2 AND cpu_temp_c > 51`, d.id, free); n != 1 {
		t.Fatal("heartbeat not stored")
	}
	// Too frequent.
	resp, _ = post(deviceapi.HeartbeatPath, body)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second heartbeat: %d", resp.StatusCode)
	}
	// Invalid heartbeat.
	bad := hb
	bad.Overall = "FINE"
	body, _ = json.Marshal(bad)
	if resp, _ = post(deviceapi.HeartbeatPath, body); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid heartbeat: %d", resp.StatusCode)
	}

	// --- 4. Knowledge bundle: none yet → 404; publish one; fetch + verify.
	get := func(query string) (*http.Response, []byte) {
		req, _ := http.NewRequest(http.MethodGet, e.hs.URL+deviceapi.KnowledgeBundlePath+query, nil)
		if err := deviceapi.SignHTTP(req, d.id, d.priv, nil, time.Now()); err != nil {
			t.Fatal(err)
		}
		resp, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, deviceapi.MaxBundleBytes))
		return resp, b
	}
	if resp, _ := get(""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no bundle: %d", resp.StatusCode)
	}
	editor, reviewer := e.adminClient(), e.adminClient()
	e.createUser("editor1", "scheme_editor")
	e.createUser("reviewer1", "scheme_reviewer")
	editor.login("editor1")
	reviewer.login("reviewer1")
	if code, b, _ := editor.req("PUT", "/v1/admin/schemes/test-scheme-alpha", putBody(0, fixtureRecord("test-scheme-alpha"))); code != 200 {
		t.Fatalf("put: %d %s", code, b)
	}
	if code, b, _ := reviewer.req("POST", "/v1/admin/schemes/test-scheme-alpha/verify", map[string]any{"version": 1}); code != 200 {
		t.Fatalf("verify: %d %s", code, b)
	}
	code, b, _ := reviewer.req("POST", "/v1/admin/bundles", nil)
	if code != http.StatusCreated {
		t.Fatalf("publish: %d %s", code, b)
	}
	resp, rb = get("")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bundle: %d %s", resp.StatusCode, rb)
	}
	var bundle scheme.Bundle
	mustJSON(t, rb, &bundle)
	if err := deviceapi.VerifyBundle(pinned, bundle); err != nil {
		t.Fatalf("device rejects bundle: %v", err)
	}
	if len(bundle.Records) != 1 || bundle.Records[0].Verification.Status != scheme.Verified || bundle.Records[0].Verification.VerifiedBy == "" {
		t.Fatalf("bundle records: %+v", bundle.Records)
	}
	bundle.Records[0].Summary.Hi += "!" // tampering is detected
	if err := deviceapi.VerifyBundle(pinned, bundle); err == nil {
		t.Fatal("tampered bundle verified")
	}
	if resp, _ := get("?since=" + resp.Header.Get("X-JS-Bundle-Version")); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("since=latest: %d", resp.StatusCode)
	}
	// Heartbeat now reports the bundle version (after the rate window).
	if n := e.count(`SELECT count(*) FROM knowledge_bundles`); n != 1 {
		t.Fatal("bundle not stored")
	}
}

// fixtureRecord is a structurally valid record with placeholder content.
// It intentionally contains no real scheme facts.
func fixtureRecord(id string) scheme.Record {
	return scheme.Record{
		ID: id, Version: 1, Kind: scheme.KindScheme,
		Name:     scheme.Text{Hi: "परीक्षण योजना", En: "Test Fixture Scheme"},
		Category: "pension",
		Summary:  scheme.Text{Hi: "यह केवल सॉफ़्टवेयर परीक्षण के लिए है।", En: "Software test fixture only."},
		Sources:  []scheme.Source{{URL: "https://example.org/test-fixture", Title: "Test fixture", AccessedOn: "2026-11-01"}},
		// A client may try to self-verify; the server must override this.
		Verification: scheme.Verification{Status: scheme.Verified, VerifiedBy: "someone", VerifiedAt: "2026-11-01T00:00:00Z"},
	}
}

func putBody(base int, rec scheme.Record) map[string]any {
	return map[string]any{"base_version": base, "change_note": "test edit", "record": rec}
}
