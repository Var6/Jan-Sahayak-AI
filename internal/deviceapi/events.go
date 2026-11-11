package deviceapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/janmanindia/jan-sahayak/internal/ids"
	"github.com/janmanindia/jan-sahayak/internal/scheme"
)

// Event types the central API accepts. Anything else is rejected (and stays
// on the device as FAILED, never deleted) so that no unreviewed data shape —
// which could carry personal data — is ever stored centrally.
const (
	TypeSessionEnded = "session.ended"
	TypeTurn         = "dialogue.turn"
)

// Enumerations used in event payloads.
var (
	Languages   = []string{"hi", "en"}
	EndReasons  = []string{"citizen_ended", "inactivity_timeout", "device_shutdown", "error"}
	Resolutions = []string{"answered", "unverified_info", "escalated", "unresolved", "redirected", "crisis"}
	Sources     = []string{"voice", "text", "option"}
)

// MaxTurns bounds per-session turn counters.
const MaxTurns = 1000

var (
	intentRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	serviceRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

// SessionEnded (type "session.ended", schema_version 1) is written once per
// citizen session. SessionID is a random UUIDv4 that is not linked to any
// person.
type SessionEnded struct {
	SessionID string `json:"session_id"`
	Language  string `json:"language"`
	StartedAt string `json:"started_at"` // RFC 3339
	EndedAt   string `json:"ended_at"`   // RFC 3339
	EndReason string `json:"end_reason"`
	Turns     int    `json:"turns"`
}

// Turn (type "dialogue.turn", schema_version 1) summarises one citizen turn.
// It mirrors dialogue.Analytics and carries no free text.
type Turn struct {
	SessionID  string `json:"session_id"`
	Seq        int    `json:"seq"` // 1-based turn number in the session
	Language   string `json:"language"`
	Source     string `json:"source"`
	Intent     string `json:"intent"`
	Category   string `json:"category,omitempty"`   // scheme.Categories value
	Service    string `json:"service,omitempty"`    // scheme record id
	Resolution string `json:"resolution,omitempty"` // Resolutions value
	UsedLLM    bool   `json:"used_llm"`
}

// Validate checks a SessionEnded payload.
func (s SessionEnded) Validate() error {
	if ids.Validate(s.SessionID) != nil {
		return errors.New("session_id must be a uuid")
	}
	if !slices.Contains(Languages, s.Language) {
		return errors.New("bad language")
	}
	st, err1 := time.Parse(time.RFC3339Nano, s.StartedAt)
	en, err2 := time.Parse(time.RFC3339Nano, s.EndedAt)
	if err1 != nil || err2 != nil {
		return errors.New("started_at/ended_at must be RFC 3339")
	}
	if en.Before(st) {
		return errors.New("ended_at before started_at")
	}
	if !slices.Contains(EndReasons, s.EndReason) {
		return errors.New("bad end_reason")
	}
	if s.Turns < 0 || s.Turns > MaxTurns {
		return errors.New("turns out of range")
	}
	return nil
}

// Validate checks a Turn payload.
func (t Turn) Validate() error {
	if ids.Validate(t.SessionID) != nil {
		return errors.New("session_id must be a uuid")
	}
	if t.Seq < 1 || t.Seq > MaxTurns {
		return errors.New("seq out of range")
	}
	if !slices.Contains(Languages, t.Language) {
		return errors.New("bad language")
	}
	if !slices.Contains(Sources, t.Source) {
		return errors.New("bad source")
	}
	if !intentRe.MatchString(t.Intent) {
		return errors.New("bad intent")
	}
	if t.Category != "" && !slices.Contains(scheme.Categories, t.Category) {
		return errors.New("bad category")
	}
	if t.Service != "" && (len(t.Service) > 80 || !serviceRe.MatchString(t.Service)) {
		return errors.New("bad service")
	}
	if t.Resolution != "" && !slices.Contains(Resolutions, t.Resolution) {
		return errors.New("bad resolution")
	}
	return nil
}

// ValidatePayload checks that an event payload matches the schema for its
// type and version. Unknown fields are rejected, so free text cannot be
// smuggled into an otherwise valid event.
func ValidatePayload(eventType string, schemaVersion int, payload []byte) error {
	if !utf8.Valid(payload) {
		return errors.New("payload is not valid UTF-8")
	}
	switch {
	case eventType == TypeSessionEnded && schemaVersion == 1:
		var v SessionEnded
		if err := strictDecode(payload, &v, sessionEndedKeys); err != nil {
			return err
		}
		return v.Validate()
	case eventType == TypeTurn && schemaVersion == 1:
		var v Turn
		if err := strictDecode(payload, &v, turnKeys); err != nil {
			return err
		}
		return v.Validate()
	default:
		return fmt.Errorf("unsupported event type/version")
	}
}

// Exact key sets. encoding/json matches keys case-insensitively, so
// {"Intent":"free text","intent":"x"} would decode fine while the database
// stores both keys; checking exact names closes that gap.
var (
	sessionEndedKeys = []string{"session_id", "language", "started_at", "ended_at", "end_reason", "turns"}
	turnKeys         = []string{"session_id", "seq", "language", "source", "intent", "category", "service", "resolution", "used_llm"}
)

func strictDecode(b []byte, v any, keys []string) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return errors.New("payload must be a JSON object")
	}
	for k := range m {
		if !slices.Contains(keys, k) {
			return errors.New("payload has an unknown field")
		}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("payload does not match schema")
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after payload")
	}
	return nil
}
