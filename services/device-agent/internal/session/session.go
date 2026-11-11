// Package session owns the lifecycle of the single citizen session a kiosk
// serves at a time.
//
// Rules (from the product specification):
//   - A session starts only from the physical button (or its PC simulation).
//   - The microphone may only be used while the session is LISTENING.
//   - The camera may only be used after explicit document-scan consent.
//   - Inactivity: reminder at 2 min, final warning at 9 min, close at 10 min.
//     Any meaningful interaction resets the inactivity clock.
//   - On end, registered cleanup hooks run (temp audio/images, transcripts).
//
// All state lives behind one mutex; timing is driven by Manager.Run calling
// Tick once per second, which keeps the logic deterministic and testable.
//
// Busy-state caps and recovery API (reliability review finding 5,
// correctness review finding 1; consumed by internal/api and cmd):
//
//   - PROCESSING and SPEAKING are exempt from the inactivity rules only for a
//     bounded time. Timeouts.MaxProcessing (default DefaultMaxProcessing =
//     70 s, i.e. the default 55 s TurnTimeout + 15 s; callers should pass
//     TurnTimeout+15 s) caps PROCESSING. SPEAKING is capped at
//     max(clip-length hint, Timeouts.MaxSpeaking) (default 120 s); the hint
//     is given with SetSpeaking(id, clip), plain SetActivity(id, Speaking)
//     uses MaxSpeaking alone. When a cap is exceeded Tick moves the session
//     to READY (publishing session.activity) WITHOUT resetting the idle
//     clock, so the normal reminder / final-warning / close rules apply from
//     the citizen's last real interaction.
//   - Manager.ForceReady(id) moves the active session to READY immediately
//     (no idle-clock reset). The API calls it from a deferred recover in a
//     turn so a panic never leaves the session in PROCESSING.
//   - Manager.StartFresh(language) ends any open session with EndAbandoned
//     (cleanup hooks run) and then starts a new one. The API uses it when the
//     physical button is pressed on an abandoned session, so the next citizen
//     never inherits the previous citizen's in-memory dialogue state.
//   - EndAbandoned is a device-side reason. It is NOT in the central
//     session.ended enumeration; recorders must send EndReason.Wire(), which
//     maps it to "inactivity_timeout" (every other reason maps to itself).
package session

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Activity is what the active session is doing right now.
type Activity string

const (
	Idle       Activity = "IDLE"       // no session
	Ready      Activity = "READY"      // session open, waiting for citizen
	Listening  Activity = "LISTENING"  // mic may capture
	Processing Activity = "PROCESSING" // STT / dialogue / TTS running
	Speaking   Activity = "SPEAKING"   // response audio is playing
)

// EndReason explains why a session ended.
type EndReason string

const (
	EndCitizen  EndReason = "citizen_ended"
	EndTimeout  EndReason = "inactivity_timeout"
	EndShutdown EndReason = "device_shutdown"
	EndError    EndReason = "error"
	// EndAbandoned: the session was ended because the next citizen pressed
	// the button while it was still open (see StartFresh). Device-side only;
	// use Wire() for the central schema.
	EndAbandoned EndReason = "abandoned"
)

// Wire returns the reason as accepted by the central session.ended schema
// (citizen_ended, inactivity_timeout, device_shutdown, error). EndAbandoned
// maps to inactivity_timeout: the previous citizen walked away.
func (r EndReason) Wire() string {
	if r == EndAbandoned {
		return string(EndTimeout)
	}
	return string(r)
}

// EventType is pushed to subscribers (the kiosk UI via SSE, logs, outbox).
type EventType string

const (
	EvStarted       EventType = "session.started"
	EvActivity      EventType = "session.activity"
	EvReminder      EventType = "session.reminder"      // 2 min idle
	EvFinalWarning  EventType = "session.final_warning" // 9 min idle
	EvEnded         EventType = "session.ended"
	EvCameraConsent EventType = "session.camera_consent" // consent granted/revoked
	EvLanguage      EventType = "session.language"
)

// Timeouts holds the inactivity thresholds and the busy-state caps.
type Timeouts struct {
	Reminder     time.Duration
	FinalWarning time.Duration
	Close        time.Duration
	// MaxProcessing caps how long a session may stay PROCESSING before Tick
	// treats it as stuck and moves it to READY. Zero means
	// DefaultMaxProcessing; set it to the API TurnTimeout + 15 s.
	MaxProcessing time.Duration
	// MaxSpeaking caps SPEAKING when no longer clip-length hint was given
	// (SetSpeaking). Zero means DefaultMaxSpeaking.
	MaxSpeaking time.Duration
}

// Default busy-state caps.
const (
	DefaultMaxProcessing = 55*time.Second + 15*time.Second // default TurnTimeout + 15 s
	DefaultMaxSpeaking   = 120 * time.Second
)

// DefaultTimeouts are the specified 2/9/10-minute rules.
var DefaultTimeouts = Timeouts{
	Reminder: 2 * time.Minute, FinalWarning: 9 * time.Minute, Close: 10 * time.Minute,
	MaxProcessing: DefaultMaxProcessing, MaxSpeaking: DefaultMaxSpeaking,
}

// Snapshot is the externally visible session state.
type Snapshot struct {
	ID            string    `json:"id,omitempty"`
	Activity      Activity  `json:"activity"`
	Language      string    `json:"language,omitempty"`
	StartedAt     time.Time `json:"started_at,omitzero"`
	LastActiveAt  time.Time `json:"last_active_at,omitzero"`
	CameraConsent bool      `json:"camera_consent"`
	Turns         int       `json:"turns"`
	// IdleDeadline is when the session will close if nothing happens.
	IdleDeadline time.Time `json:"idle_deadline,omitzero"`
}

// Event is a state notification.
type Event struct {
	Type      EventType `json:"type"`
	Session   Snapshot  `json:"session"`
	EndReason EndReason `json:"end_reason,omitempty"`
	At        time.Time `json:"at"`
}

// Errors returned for disallowed transitions.
var (
	ErrNoSession       = errors.New("session: no active session")
	ErrAlreadyActive   = errors.New("session: a session is already active")
	ErrWrongSession    = errors.New("session: session id does not match the active session")
	ErrBadTransition   = errors.New("session: transition not allowed from current activity")
	ErrNoCameraConsent = errors.New("session: camera consent not given")
)

// CleanupFunc runs when a session ends. It receives the ended session id.
// It must be quick and must not call back into the Manager.
type CleanupFunc func(sessionID string, reason EndReason)

// Manager is safe for concurrent use.
type Manager struct {
	mu       sync.Mutex
	cur      *Snapshot
	reminded bool
	warned   bool
	// activitySince is when the current activity began; speakingCap is the
	// SPEAKING limit for the current clip.
	activitySince time.Time
	speakingCap   time.Duration
	t             Timeouts
	now           func() time.Time
	cleanups      []CleanupFunc
	subs          map[int]chan Event
	nextSub       int
}

// NewManager creates a manager. now may be nil (uses time.Now).
func NewManager(t Timeouts, now func() time.Time) *Manager {
	if now == nil {
		now = time.Now
	}
	if t.Close <= 0 {
		t.Reminder, t.FinalWarning, t.Close = DefaultTimeouts.Reminder, DefaultTimeouts.FinalWarning, DefaultTimeouts.Close
	}
	if t.MaxProcessing <= 0 {
		t.MaxProcessing = DefaultMaxProcessing
	}
	if t.MaxSpeaking <= 0 {
		t.MaxSpeaking = DefaultMaxSpeaking
	}
	return &Manager{t: t, now: now, subs: map[int]chan Event{}}
}

// OnEnd registers a cleanup hook. Call during wiring, before Run.
func (m *Manager) OnEnd(fn CleanupFunc) {
	m.mu.Lock()
	m.cleanups = append(m.cleanups, fn)
	m.mu.Unlock()
}

// subscriberBuffer bounds each subscriber's queue. A slow subscriber loses
// its oldest events rather than blocking the session (UI re-syncs via
// Snapshot on reconnect).
const subscriberBuffer = 32

// maxSubscribers bounds how many concurrent listeners (SSE clients) exist.
const maxSubscribers = 8

// ErrTooManySubscribers is returned by Subscribe when the bound is reached.
var ErrTooManySubscribers = errors.New("session: too many subscribers")

// Subscribe returns a channel of events and a cancel func that must be called.
func (m *Manager) Subscribe() (<-chan Event, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.subs) >= maxSubscribers {
		return nil, nil, ErrTooManySubscribers
	}
	id := m.nextSub
	m.nextSub++
	ch := make(chan Event, subscriberBuffer)
	m.subs[id] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			m.mu.Lock()
			if c, ok := m.subs[id]; ok {
				delete(m.subs, id)
				close(c)
			}
			m.mu.Unlock()
		})
	}, nil
}

// publishLocked must be called with m.mu held.
func (m *Manager) publishLocked(ev Event) {
	for _, ch := range m.subs {
		select {
		case ch <- ev:
		default:
			// drop oldest, then enqueue
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- ev:
			default:
			}
		}
	}
}

func (m *Manager) snapshotLocked() Snapshot {
	if m.cur == nil {
		return Snapshot{Activity: Idle}
	}
	s := *m.cur
	s.IdleDeadline = s.LastActiveAt.Add(m.t.Close)
	return s
}

func (m *Manager) eventLocked(t EventType) Event {
	return Event{Type: t, Session: m.snapshotLocked(), At: m.now()}
}

// Current returns the current state.
func (m *Manager) Current() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

// Start opens a new session (button press). language is the default
// language for the kiosk (citizen can change it).
func (m *Manager) Start(language string) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur != nil {
		return m.snapshotLocked(), ErrAlreadyActive
	}
	now := m.now()
	m.cur = &Snapshot{ID: newID(), Activity: Ready, Language: language, StartedAt: now, LastActiveAt: now}
	m.reminded, m.warned = false, false
	m.activitySince, m.speakingCap = now, 0
	m.publishLocked(m.eventLocked(EvStarted))
	return m.snapshotLocked(), nil
}

// StartFresh ends any open session with EndAbandoned (its cleanup hooks
// run, outside the lock) and then starts a new one. If another caller starts
// a session in between, that session is returned with ErrAlreadyActive.
func (m *Manager) StartFresh(language string) (Snapshot, error) {
	m.mu.Lock()
	if m.cur != nil {
		m.endLocked(EndAbandoned) // releases m.mu
	} else {
		m.mu.Unlock()
	}
	return m.Start(language)
}

// ForceReady moves the active session to READY at once, whatever it is
// doing, without counting as citizen interaction (the idle clock is not
// reset). Used by the API to recover from a panicking turn.
func (m *Manager) ForceReady(id string) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkLocked(id); err != nil {
		return m.snapshotLocked(), err
	}
	if m.cur.Activity != Ready {
		m.setActivityLocked(Ready, 0)
		m.publishLocked(m.eventLocked(EvActivity))
	}
	return m.snapshotLocked(), nil
}

// setActivityLocked changes the activity and restarts its busy-state timer.
func (m *Manager) setActivityLocked(a Activity, speakingCap time.Duration) {
	m.cur.Activity = a
	m.activitySince = m.now()
	m.speakingCap = speakingCap
}

func (m *Manager) checkLocked(id string) error {
	if m.cur == nil {
		return ErrNoSession
	}
	if id != m.cur.ID {
		return ErrWrongSession
	}
	return nil
}

// allowed lists legal activity transitions while a session is open.
var allowed = map[Activity]map[Activity]bool{
	Ready:      {Listening: true, Processing: true, Speaking: true},
	Listening:  {Processing: true, Ready: true},
	Processing: {Speaking: true, Ready: true, Listening: true},
	Speaking:   {Ready: true, Listening: true},
}

// SetActivity moves the session to a new activity. Every successful
// transition counts as meaningful interaction and resets the idle clock.
// SPEAKING set this way is capped at Timeouts.MaxSpeaking; use SetSpeaking
// when the clip length is known.
func (m *Manager) SetActivity(id string, a Activity) (Snapshot, error) {
	return m.setActivity(id, a, 0)
}

// SetSpeaking moves the session to SPEAKING for a clip of the given length.
// SPEAKING is then capped at max(clip, Timeouts.MaxSpeaking).
func (m *Manager) SetSpeaking(id string, clip time.Duration) (Snapshot, error) {
	return m.setActivity(id, Speaking, clip)
}

func (m *Manager) setActivity(id string, a Activity, clip time.Duration) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkLocked(id); err != nil {
		return m.snapshotLocked(), err
	}
	if m.cur.Activity != a && !allowed[m.cur.Activity][a] {
		return m.snapshotLocked(), fmt.Errorf("%w: %s -> %s", ErrBadTransition, m.cur.Activity, a)
	}
	var speakCap time.Duration
	if a == Speaking {
		speakCap = max(clip, m.t.MaxSpeaking)
	}
	// Re-entering the same activity restarts its timer too: it is a new
	// turn or a new clip.
	m.setActivityLocked(a, speakCap)
	if a == Processing {
		m.cur.Turns++
	}
	m.touchLocked()
	m.publishLocked(m.eventLocked(EvActivity))
	return m.snapshotLocked(), nil
}

// Touch records meaningful interaction (e.g. citizen tapped a button).
func (m *Manager) Touch(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkLocked(id); err != nil {
		return err
	}
	m.touchLocked()
	return nil
}

func (m *Manager) touchLocked() {
	m.cur.LastActiveAt = m.now()
	m.reminded, m.warned = false, false
}

// RequireListening returns nil only if the session id is active and
// LISTENING. The audio endpoint calls this before accepting any audio.
func (m *Manager) RequireListening(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkLocked(id); err != nil {
		return err
	}
	if m.cur.Activity != Listening {
		return fmt.Errorf("%w: microphone requires LISTENING, current %s", ErrBadTransition, m.cur.Activity)
	}
	return nil
}

// SetLanguage changes the conversation language.
func (m *Manager) SetLanguage(id, lang string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkLocked(id); err != nil {
		return err
	}
	m.cur.Language = lang
	m.touchLocked()
	m.publishLocked(m.eventLocked(EvLanguage))
	return nil
}

// SetCameraConsent records explicit consent (or its withdrawal) for document
// scanning in this session only.
func (m *Manager) SetCameraConsent(id string, consent bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkLocked(id); err != nil {
		return err
	}
	m.cur.CameraConsent = consent
	m.touchLocked()
	m.publishLocked(m.eventLocked(EvCameraConsent))
	return nil
}

// RequireCameraConsent returns nil only if the active session consented.
func (m *Manager) RequireCameraConsent(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkLocked(id); err != nil {
		return err
	}
	if !m.cur.CameraConsent {
		return ErrNoCameraConsent
	}
	return nil
}

// End closes the active session. id may be empty to end whatever session is
// active (used by shutdown).
func (m *Manager) End(id string, reason EndReason) (Snapshot, error) {
	m.mu.Lock()
	if m.cur == nil {
		m.mu.Unlock()
		return Snapshot{Activity: Idle}, ErrNoSession
	}
	if id != "" && id != m.cur.ID {
		m.mu.Unlock()
		return m.Current(), ErrWrongSession
	}
	return m.endLocked(reason), nil
}

// endLocked is called with m.mu held and releases it.
func (m *Manager) endLocked(reason EndReason) Snapshot {
	ended := m.snapshotLocked()
	ev := Event{Type: EvEnded, Session: ended, EndReason: reason, At: m.now()}
	m.cur = nil
	cleanups := append([]CleanupFunc(nil), m.cleanups...)
	m.publishLocked(ev)
	m.mu.Unlock()
	for _, fn := range cleanups {
		runCleanup(fn, ended.ID, reason)
	}
	return ended
}

func runCleanup(fn CleanupFunc, id string, reason EndReason) {
	defer func() { _ = recover() }() // a failing hook must not block others
	fn(id, reason)
}

// Tick evaluates inactivity rules at the current time.
func (m *Manager) Tick() {
	m.mu.Lock()
	if m.cur == nil {
		m.mu.Unlock()
		return
	}
	// Never time out while the device itself is busy on the citizen's
	// behalf — but only for a bounded time. A turn that panicked or hung,
	// or a UI that never reports speaking-done, must not keep the session
	// (and the kiosk) open forever.
	if limit, busy := m.busyLimitLocked(); busy {
		if m.now().Sub(m.activitySince) < limit {
			m.mu.Unlock()
			return
		}
		m.setActivityLocked(Ready, 0) // idle clock deliberately not reset
		m.publishLocked(m.eventLocked(EvActivity))
	}
	idle := m.now().Sub(m.cur.LastActiveAt)
	switch {
	case idle >= m.t.Close:
		m.endLocked(EndTimeout)
		return
	case idle >= m.t.FinalWarning && !m.warned:
		m.warned = true
		m.publishLocked(m.eventLocked(EvFinalWarning))
	case idle >= m.t.Reminder && !m.reminded:
		m.reminded = true
		m.publishLocked(m.eventLocked(EvReminder))
	}
	m.mu.Unlock()
}

// busyLimitLocked returns the cap for the current activity and whether the
// activity is a busy one (PROCESSING or SPEAKING).
func (m *Manager) busyLimitLocked() (time.Duration, bool) {
	switch m.cur.Activity {
	case Processing:
		return m.t.MaxProcessing, true
	case Speaking:
		if m.speakingCap > 0 {
			return m.speakingCap, true
		}
		return m.t.MaxSpeaking, true
	}
	return 0, false
}

// Run drives Tick once per second until ctx is done, then ends any active
// session with EndShutdown so cleanup hooks run.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_, _ = m.End("", EndShutdown)
			return
		case <-t.C:
			m.Tick()
		}
	}
}

// newID returns a random RFC 4122 version-4 UUID. Session ids are random so
// they cannot be linked to a person or to each other.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("session: crypto/rand failed: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
