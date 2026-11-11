package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/janmanindia/jan-sahayak/internal/ids"
	"github.com/janmanindia/jan-sahayak/internal/syncproto"
	"github.com/janmanindia/jan-sahayak/services/central-api/internal/core"
)

func TestSyncHappyPath(t *testing.T) {
	e := newEnv(t)
	d := e.newDevice("kiosk-1")
	sid := ids.NewV4()
	br := d.batch(
		mkEvent("dialogue.turn", turn(sid, 1, "pension", "answered")),
		mkEvent("dialogue.turn", turn(sid, 2, "pension", "unverified_info")),
		mkEvent("session.ended", sessionEnded(sid, "hi", 2)),
	)
	res := e.uploadOK(d, br)
	for _, ev := range br.Events {
		r := res[ev.ID]
		if r.Status != syncproto.StatusAccepted || !syncproto.IsDurablyStored(r, ev.PayloadSHA256) {
			t.Fatalf("event %s: %+v", ev.ID, r)
		}
		var receipt, hash, dev string
		if err := e.pool.QueryRow(context.Background(), `SELECT receipt::text, payload_sha256, device_id::text FROM sync_events WHERE id = $1`, ev.ID).
			Scan(&receipt, &hash, &dev); err != nil {
			t.Fatal(err)
		}
		if receipt != r.Receipt || hash != ev.PayloadSHA256 || dev != d.id {
			t.Fatalf("stored row mismatch: %s %s %s", receipt, hash, dev)
		}
	}
	if n := e.count(`SELECT count(*) FROM sync_events`); n != 3 {
		t.Fatalf("stored %d events", n)
	}
	if n := e.count(`SELECT count(*) FROM devices WHERE last_seen_at IS NOT NULL`); n != 1 {
		t.Fatal("last_seen_at not updated")
	}
}

func TestSyncDuplicateReturnsOriginalReceipt(t *testing.T) {
	e := newEnv(t)
	d := e.newDevice("kiosk-1")
	sid := ids.NewV4()
	ev1 := mkEvent("dialogue.turn", turn(sid, 1, "loan", "escalated"))
	ev2 := mkEvent("session.ended", sessionEnded(sid, "hi", 1))
	first := e.uploadOK(d, d.batch(ev1, ev2))

	// Resend (e.g. the ack was lost): new batch id and nonce, same events.
	again := e.uploadOK(d, d.batch(ev1, ev2))
	for _, ev := range []syncproto.Event{ev1, ev2} {
		r := again[ev.ID]
		if r.Status != syncproto.StatusDuplicate {
			t.Fatalf("want duplicate, got %+v", r)
		}
		if r.Receipt != first[ev.ID].Receipt {
			t.Fatalf("duplicate receipt %s != original %s", r.Receipt, first[ev.ID].Receipt)
		}
		if !syncproto.IsDurablyStored(r, ev.PayloadSHA256) {
			t.Fatal("duplicate must count as durably stored")
		}
	}
	if n := e.count(`SELECT count(*) FROM sync_events`); n != 2 {
		t.Fatalf("stored %d events", n)
	}
}

func TestSyncConflictOnChangedHash(t *testing.T) {
	e := newEnv(t)
	d := e.newDevice("kiosk-1")
	sid := ids.NewV4()
	ev := mkEvent("dialogue.turn", turn(sid, 1, "loan", "answered"))
	e.uploadOK(d, d.batch(ev))

	changed := mkEvent("dialogue.turn", turn(sid, 1, "loan", "unresolved"))
	changed.ID = ev.ID // same id, different payload
	res := e.uploadOK(d, d.batch(changed))
	r := res[ev.ID]
	if r.Status != syncproto.StatusConflict || r.Receipt != "" {
		t.Fatalf("want conflict without receipt, got %+v", r)
	}
	if syncproto.IsDurablyStored(r, changed.PayloadSHA256) {
		t.Fatal("conflict must not be durably stored")
	}
	var hash string
	_ = e.pool.QueryRow(context.Background(), `SELECT payload_sha256 FROM sync_events WHERE id = $1`, ev.ID).Scan(&hash)
	if hash != ev.PayloadSHA256 {
		t.Fatal("original event was modified")
	}
	// Another device reusing the id also gets a conflict (and no hash leak).
	d2 := e.newDevice("kiosk-2")
	other := mkEvent("dialogue.turn", turn(sid, 1, "loan", "answered"))
	other.ID = ev.ID
	r2 := e.uploadOK(d2, d2.batch(other))[ev.ID]
	if r2.Status != syncproto.StatusConflict || r2.PayloadSHA256 != "" || r2.Receipt != "" {
		t.Fatalf("cross-device id reuse: %+v", r2)
	}
}

func TestSyncRejectsInvalidEvents(t *testing.T) {
	e := newEnv(t)
	d := e.newDevice("kiosk-1")
	sid := ids.NewV4()
	good := mkEvent("session.ended", sessionEnded(sid, "en", 0))

	badHash := mkEvent("dialogue.turn", turn(sid, 1, "loan", "answered"))
	badHash.PayloadSHA256 = strings.Repeat("0", 64)

	unknownType := mkEvent("citizen.profile", map[string]string{"x": "y"})

	// Extra field (free text) in an otherwise valid turn: refused so that
	// personal data can never ride along.
	extra := mkEvent("dialogue.turn", map[string]any{"session_id": sid, "seq": 1, "language": "hi", "source": "text",
		"intent": "loan", "transcript": "free text must never be stored"})

	badEnum := mkEvent("dialogue.turn", turn(sid, 2, "not_a_category", "answered"))
	notObject := mkEvent("session.ended", []int{1, 2})
	badCreated := mkEvent("session.ended", sessionEnded(sid, "hi", 0))
	badCreated.CreatedAt = "yesterday"

	br := d.batch(good, badHash, unknownType, extra, badEnum, notObject, badCreated)
	res := e.uploadOK(d, br)
	if res[good.ID].Status != syncproto.StatusAccepted {
		t.Fatalf("good: %+v", res[good.ID])
	}
	for _, ev := range br.Events[1:] {
		r := res[ev.ID]
		if r.Status != syncproto.StatusRejected || r.Reason == "" || r.Receipt != "" {
			t.Fatalf("event %s (%s): want rejected with reason, got %+v", ev.ID, ev.Type, r)
		}
	}
	if n := e.count(`SELECT count(*) FROM sync_events`); n != 1 {
		t.Fatalf("stored %d events, want 1", n)
	}
}

func TestSyncBadSignature(t *testing.T) {
	e := newEnv(t)
	d := e.newDevice("kiosk-1")
	body, _ := json.Marshal(d.batch(mkEvent("session.ended", sessionEnded(ids.NewV4(), "hi", 0))))

	// Body modified after signing.
	sr := d.sign("POST", syncproto.Path, body, time.Now())
	sr.body = append(append([]byte(nil), body...), ' ') // still valid JSON, different bytes
	if code, b := e.do(sr); code != http.StatusUnauthorized {
		t.Fatalf("tampered body: %d %s", code, b)
	}
	// Signed with a key that is not the registered one.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	imp := device{id: d.id, priv: other}
	if code, _ := e.do(imp.sign("POST", syncproto.Path, body, time.Now())); code != http.StatusUnauthorized {
		t.Fatalf("wrong key: %d", code)
	}
	// Signature over a different path.
	sr = d.sign("POST", "/v1/devices/heartbeat", body, time.Now())
	sr.path = syncproto.Path
	if code, _ := e.do(sr); code != http.StatusUnauthorized {
		t.Fatalf("wrong path: %d", code)
	}
	// Unknown device.
	unk := device{id: ids.NewV4(), priv: other}
	if code, _ := e.do(unk.sign("POST", syncproto.Path, body, time.Now())); code != http.StatusUnauthorized {
		t.Fatalf("unknown device: %d", code)
	}
	if n := e.count(`SELECT count(*) FROM sync_events`); n != 0 {
		t.Fatal("events stored despite bad signature")
	}
	if n := e.count(`SELECT count(*) FROM sync_nonces`); n != 0 {
		t.Fatal("unauthenticated requests must not create nonce rows")
	}
}

func TestSyncReplayedNonce(t *testing.T) {
	e := newEnv(t)
	d := e.newDevice("kiosk-1")
	br := d.batch(mkEvent("session.ended", sessionEnded(ids.NewV4(), "hi", 0)))
	body, _ := json.Marshal(br)
	sr := d.sign("POST", syncproto.Path, body, time.Now())
	if code, b := e.do(sr); code != http.StatusOK {
		t.Fatalf("first: %d %s", code, b)
	}
	code, b := e.do(sr) // byte-identical replay
	if code != http.StatusUnauthorized {
		t.Fatalf("replay accepted: %d %s", code, b)
	}
	if strings.Contains(string(b), "signature") {
		t.Fatal("replay response must not contain an ack")
	}
}

func TestSyncClockSkew(t *testing.T) {
	e := newEnv(t)
	d := e.newDevice("kiosk-1")
	body, _ := json.Marshal(d.batch(mkEvent("session.ended", sessionEnded(ids.NewV4(), "hi", 0))))
	for _, off := range []time.Duration{-10 * time.Minute, 10 * time.Minute} {
		code, b := e.do(d.sign("POST", syncproto.Path, body, time.Now().Add(off)))
		var er map[string]apiError
		_ = json.Unmarshal(b, &er)
		if code != http.StatusUnauthorized || er["error"].Code != "clock_skew" {
			t.Fatalf("offset %v: %d %s", off, code, b)
		}
	}
	// Within skew is fine.
	if code, b := e.do(d.sign("POST", syncproto.Path, body, time.Now().Add(-4*time.Minute))); code != http.StatusOK {
		t.Fatalf("4 min skew: %d %s", code, b)
	}
}

func TestSyncRevokedDevice(t *testing.T) {
	e := newEnv(t)
	d := e.newDevice("kiosk-1")
	e.uploadOK(d, d.batch(mkEvent("session.ended", sessionEnded(ids.NewV4(), "hi", 0))))
	if err := core.RevokeDevice(context.Background(), e.pool, core.Actor{Label: "test"}, d.id, "stolen"); err != nil {
		t.Fatal(err)
	}
	code, b := e.upload(d, d.batch(mkEvent("session.ended", sessionEnded(ids.NewV4(), "hi", 0))))
	if code != http.StatusUnauthorized {
		t.Fatalf("revoked device: %d %s", code, b)
	}
	if n := e.count(`SELECT count(*) FROM sync_events`); n != 1 {
		t.Fatalf("revocation must keep stored events; have %d", n)
	}
}

func TestSyncOversizedBody(t *testing.T) {
	e := newEnv(t)
	d := e.newDevice("kiosk-1")
	body := bytes.Repeat([]byte(" "), syncproto.MaxBodyBytes+1)
	code, _ := e.do(d.sign("POST", syncproto.Path, body, time.Now()))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized: %d", code)
	}
	// Same, with chunked transfer (no Content-Length) via a pipe-like reader.
	req, _ := http.NewRequest("POST", e.hs.URL+syncproto.Path, struct{ *bytes.Reader }{bytes.NewReader(body)})
	sr := d.sign("POST", syncproto.Path, body, time.Now())
	req.Header.Set(syncproto.HeaderDevice, d.id)
	req.Header.Set(syncproto.HeaderTS, strconv.FormatInt(sr.ts, 10))
	req.Header.Set(syncproto.HeaderNonce, sr.nonce)
	req.Header.Set(syncproto.HeaderSig, sr.sig)
	resp, err := e.client.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("chunked oversized: %d", resp.StatusCode)
		}
	}
}

func TestSyncBatchDeviceMismatchAndMalformed(t *testing.T) {
	e := newEnv(t)
	d := e.newDevice("kiosk-1")
	other := e.newDevice("kiosk-2")
	br := other.batch(mkEvent("session.ended", sessionEnded(ids.NewV4(), "hi", 0)))
	if code, _ := e.upload(d, br); code != http.StatusBadRequest {
		t.Fatalf("device mismatch: %d", code)
	}
	ev := mkEvent("session.ended", sessionEnded(ids.NewV4(), "hi", 0))
	if code, _ := e.upload(d, d.batch(ev, ev)); code != http.StatusBadRequest {
		t.Fatalf("duplicate ids in batch: %d", code)
	}
	if code, _ := e.upload(d, d.batch()); code != http.StatusBadRequest {
		t.Fatalf("empty batch: %d", code)
	}
	if n := e.count(`SELECT count(*) FROM sync_events`); n != 0 {
		t.Fatal("events stored from rejected batch")
	}
}

func TestSyncDBFailureSendsNoAck(t *testing.T) {
	e := newEnv(t)
	d := e.newDevice("kiosk-1")
	ev := mkEvent("session.ended", sessionEnded(ids.NewV4(), "hi", 0))

	// 1. Failure inside the ingest transaction (after the inserts).
	fail := hookFunc(func(ctx context.Context, tx pgx.Tx) error { return errors.New("injected failure") })
	e.srv.beforeCommit.Store(&fail)
	code, body := e.upload(d, d.batch(ev))
	if code < 500 {
		t.Fatalf("want 5xx, got %d %s", code, body)
	}
	var probe map[string]any
	_ = json.Unmarshal(body, &probe)
	if _, ok := probe["signature"]; ok || strings.Contains(string(body), "results") {
		t.Fatalf("failure response contains an ack: %s", body)
	}
	if n := e.count(`SELECT count(*) FROM sync_events`); n != 0 {
		t.Fatal("transaction was not rolled back")
	}
	// Device retries later: accepted (not duplicate) — nothing half-stored.
	e.srv.beforeCommit.Store(nil)
	if r := e.uploadOK(d, d.batch(ev))[ev.ID]; r.Status != syncproto.StatusAccepted {
		t.Fatalf("retry: %+v", r)
	}

	// 2. Context cancelled mid-transaction (client went away / timeout).
	ev2 := mkEvent("session.ended", sessionEnded(ids.NewV4(), "hi", 0))
	slow := hookFunc(func(ctx context.Context, tx pgx.Tx) error {
		ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		_, err := tx.Exec(ctx, `SELECT pg_sleep(5)`)
		return err
	})
	e.srv.beforeCommit.Store(&slow)
	code, body = e.upload(d, d.batch(ev2))
	if code < 500 || strings.Contains(string(body), "signature") {
		t.Fatalf("timeout: %d %s", code, body)
	}
	e.srv.beforeCommit.Store(nil)
	if n := e.count(`SELECT count(*) FROM sync_events WHERE id = $1`, ev2.ID); n != 0 {
		t.Fatal("event stored despite failed request")
	}

	// 3. Database gone entirely (the server's pool; it runs as the runtime role).
	e.srv.pool.Close()
	code, body = e.upload(d, d.batch(ev2))
	if code < 500 || strings.Contains(string(body), "signature") {
		t.Fatalf("closed pool: %d %s", code, body)
	}
}
