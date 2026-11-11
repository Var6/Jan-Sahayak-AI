package api

import (
	"context"
	"strings"
	"testing"

	"github.com/janmanindia/jan-sahayak/internal/deviceapi"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/outbox"
)

// A session left open by a killed agent (no session.ended, ended_at NULL)
// is closed at the next start with end_reason "error", in one transaction
// with its session.ended event, and the sweep is idempotent.
func TestCloseOrphanedSessions(t *testing.T) {
	h := newHarness(t, hopts{})
	r := h.start()
	sid := r.Session.ID
	if w := h.post("/api/v1/session/"+sid+"/text", map[string]string{"text": "मुझे लोन चाहिए"}); w.Code != 200 {
		t.Fatalf("text: %d %s", w.Code, w.Body)
	}
	// The process "dies" here: nothing records the end. A second row
	// that was ended properly must not be touched.
	if _, err := h.st.DB.Exec(`INSERT INTO sessions (id, started_at, ended_at, language, end_reason, turns)
		VALUES ('done-1', '2026-01-01T00:00:00.000000000Z', '2026-01-01T00:05:00.000000000Z', 'hi', 'citizen_ended', 2)`); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	ob := outbox.New(h.st.DB, outbox.DefaultConfig, nil)
	n, err := CloseOrphanedSessions(ctx, h.st.DB, ob)
	if err != nil || n != 1 {
		t.Fatalf("closed %d, err %v; want 1", n, err)
	}
	var endedAt, reason string
	var turns int
	if err := h.st.DB.QueryRow(`SELECT ended_at, end_reason, turns FROM sessions WHERE id = ?`, sid).Scan(&endedAt, &reason, &turns); err != nil {
		t.Fatal(err)
	}
	if endedAt == "" || reason != "error" || turns != 1 {
		t.Fatalf("row: ended_at %q reason %q turns %d", endedAt, reason, turns)
	}
	var endedEvents int
	for _, row := range h.outbox() {
		if row.typ != deviceapi.TypeSessionEnded {
			continue
		}
		endedEvents++
		if err := deviceapi.ValidatePayload(row.typ, 1, []byte(row.payload)); err != nil {
			t.Fatalf("payload rejected by central schema: %v %s", err, row.payload)
		}
		if !strings.Contains(row.payload, sid) || !strings.Contains(row.payload, `"end_reason":"error"`) || !strings.Contains(row.payload, `"turns":1`) {
			t.Fatalf("session.ended: %s", row.payload)
		}
	}
	if endedEvents != 1 {
		t.Fatalf("%d session.ended events, want 1", endedEvents)
	}
	if n, err := CloseOrphanedSessions(ctx, h.st.DB, ob); err != nil || n != 0 {
		t.Fatalf("second sweep closed %d, err %v; want 0", n, err)
	}
}

// With the outbox full, session.ended is refused but the local row is still
// closed, so the next start does not report it as a crash orphan.
func TestFullOutboxStillClosesSessionRow(t *testing.T) {
	h := newHarness(t, hopts{})
	sid := h.start().Session.ID
	// Fill the outbox: the counter row says MaxRows rows exist.
	if _, err := h.st.DB.Exec(`UPDATE outbox_counter SET n = ?`, outbox.DefaultConfig.MaxRows); err != nil {
		t.Fatal(err)
	}
	if w := h.post("/api/v1/session/"+sid+"/end", nil); w.Code != 200 {
		t.Fatalf("end: %d %s", w.Code, w.Body)
	}
	var endedAt, reason string
	if err := h.st.DB.QueryRow(`SELECT coalesce(ended_at,''), coalesce(end_reason,'') FROM sessions WHERE id = ?`, sid).Scan(&endedAt, &reason); err != nil {
		t.Fatal(err)
	}
	if endedAt == "" || reason != "citizen_ended" {
		t.Fatalf("row not closed: ended_at %q reason %q", endedAt, reason)
	}
	for _, r := range h.outbox() {
		if r.typ == deviceapi.TypeSessionEnded {
			t.Fatalf("event stored despite full outbox: %s", r.payload)
		}
	}
	if n, err := CloseOrphanedSessions(context.Background(), h.st.DB, outbox.New(h.st.DB, outbox.DefaultConfig, nil)); err != nil || n != 0 {
		t.Fatalf("sweep closed %d, err %v; want 0", n, err)
	}
}
