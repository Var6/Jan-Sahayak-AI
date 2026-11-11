// Package dialogue is the conversation engine. It decides what the kiosk
// says next. It is deterministic first: rules, a state machine per intent and
// verified records produce every fact. The LLM (optional) may only classify
// ambiguous input and rephrase already-verified text; its output is checked
// by a validator and discarded if it adds anything.
//
// This file is the stable contract used by the local API. Implementation
// lives in the other files of this package.
package dialogue

import (
	"context"
	"time"
)

// Lang is a conversation language code.
type Lang string

const (
	Hindi   Lang = "hi"
	English Lang = "en"
)

// Source says how the citizen's input arrived.
type Source string

const (
	FromVoice  Source = "voice"
	FromText   Source = "text"
	FromOption Source = "option" // tapped one of Reply.Options
)

// Input is one citizen turn.
type Input struct {
	Text string
	// OptionID is set when Source == FromOption.
	OptionID string
	Source   Source
	// STTConfidence is 0..1 for voice input (ignored otherwise).
	STTConfidence float64
	Lang          Lang
	// NoSpeech is set by the agent when VAD/STT found no speech in the audio.
	NoSpeech bool
}

// ReplyKind classifies a reply for the UI and analytics.
type ReplyKind string

const (
	KindGreeting ReplyKind = "greeting"
	KindQuestion ReplyKind = "question" // clarifying / eligibility question
	KindAnswer   ReplyKind = "answer"   // scheme information
	KindRepeat   ReplyKind = "repeat"   // could not understand; please repeat
	KindRedirect ReplyKind = "redirect" // off-topic; polite redirect
	KindEscalate ReplyKind = "escalate" // needs a human / official office
	KindCrisis   ReplyKind = "crisis"   // safety first: helplines
	KindGoodbye  ReplyKind = "goodbye"
)

// Option is a tappable choice shown under the reply (also accepted by voice).
type Option struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Card is structured scheme information for the screen.
type Card struct {
	SchemeID     string        `json:"scheme_id"`
	Title        string        `json:"title"`
	Verified     bool          `json:"verified"`
	LastVerified string        `json:"last_verified,omitempty"`
	Sections     []CardSection `json:"sections"`
	OfficialURL  string        `json:"official_url,omitempty"`
	Helpline     string        `json:"helpline,omitempty"`
	Disclaimer   string        `json:"disclaimer,omitempty"`
}

// CardSection is one titled list on a card.
type CardSection struct {
	Title string   `json:"title"`
	Items []string `json:"items"`
}

// Reply is what the kiosk shows and speaks.
type Reply struct {
	Kind ReplyKind `json:"kind"`
	// Text is displayed on screen.
	Text string `json:"text"`
	// Speech is what TTS reads (no URLs, numbers written for speech). If
	// empty, Text is spoken.
	Speech  string   `json:"speech,omitempty"`
	Options []Option `json:"options,omitempty"`
	Cards   []Card   `json:"cards,omitempty"`
	// ListenNext tells the UI to open the mic after speaking.
	ListenNext bool `json:"listen_next"`
	// EndSession asks the agent to close the session after speaking.
	EndSession bool `json:"end_session"`
	// UsedLLM is true if any part of Text came from the LLM (after validation).
	UsedLLM bool `json:"used_llm"`
	// Analytics is the anonymous summary recorded in the outbox.
	Analytics Analytics `json:"-"`
}

// Analytics is the only information about a turn that leaves the device.
// It must never contain free text, names, numbers typed by the citizen, or
// anything that identifies a person.
type Analytics struct {
	Intent     string `json:"intent"`               // e.g. "loan", "certificate", "off_topic"
	Category   string `json:"category,omitempty"`   // scheme.Categories value
	Service    string `json:"service,omitempty"`    // scheme record id
	Resolution string `json:"resolution,omitempty"` // "answered" | "unverified_info" | "escalated" | "unresolved" | "redirected" | "crisis"
}

// State is the per-session conversation memory. It lives only in RAM, is
// bounded, and is discarded when the session ends.
type State struct {
	Lang    Lang
	Flow    string            // current flow id ("" = top level)
	Step    string            // step within the flow
	Slots   map[string]string // answers collected in this session (bounded)
	History []Turn            // last MaxHistory turns
	Misses  int               // consecutive not-understood turns
	// Distress is set when a low-confidence voice turn contained distress
	// words; until a turn is understood, "please repeat" prompts also show
	// the emergency number. It holds no citizen text.
	Distress bool
	Started  time.Time
	Scheme   string // scheme currently being discussed
	// Last is the previous reply (for "repeat", for validating tapped
	// options and for matching spoken answers to offered options).
	Last *Reply
}

// Turn is a history entry.
type Turn struct {
	Citizen string
	Kiosk   string
}

// Limits on State to keep memory bounded.
const (
	MaxHistory    = 8
	MaxSlots      = 16
	MaxInputRunes = 500
)

// Engine produces replies. Implementations must be safe for concurrent use
// across sessions (the kiosk has one session, but tests run many).
type Engine interface {
	// Greet returns the opening message for a new session.
	Greet(lang Lang) Reply
	// Handle processes one input and mutates st.
	Handle(ctx context.Context, st *State, in Input) (Reply, error)
}

// NewState returns an empty state for a new session.
func NewState(lang Lang, now time.Time) *State {
	return &State{Lang: lang, Slots: map[string]string{}, Started: now}
}
