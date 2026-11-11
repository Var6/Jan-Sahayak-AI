// Package health tracks the health of every device component (database, AI
// runtimes, hardware, network, sync, resources) and exposes a snapshot used by
// the local API, the sync heartbeat and the systemd watchdog.
//
// Checks run sequentially from one goroutine owned by Monitor.Run, each with
// its own timeout, so a hung check can never pile up goroutines.
package health

import (
	"context"
	"sort"
	"sync"
	"time"
)

// State is the health state of a component.
type State string

const (
	Healthy           State = "HEALTHY"
	Degraded          State = "DEGRADED"
	Unavailable       State = "UNAVAILABLE"
	Recovering        State = "RECOVERING"
	RequiresAttention State = "REQUIRES_ATTENTION"
)

// severity orders states from best to worst for computing the overall state.
func (s State) severity() int {
	switch s {
	case Healthy:
		return 0
	case Recovering:
		return 1
	case Degraded:
		return 2
	case Unavailable:
		return 3
	case RequiresAttention:
		return 4
	default:
		return 4
	}
}

// Result is what a check returns.
type Result struct {
	State  State          `json:"state"`
	Detail string         `json:"detail,omitempty"`
	Data   map[string]any `json:"data,omitempty"`
}

// CheckFunc probes a component. It must respect ctx cancellation.
type CheckFunc func(ctx context.Context) Result

// Component describes one monitored component.
type Component struct {
	Name string
	// Critical components make the whole device REQUIRES_ATTENTION when they
	// fail (e.g. the database). Optional components (LLM, camera) only
	// degrade the device.
	Critical bool
	Interval time.Duration
	Timeout  time.Duration
	Check    CheckFunc
}

// Status is the last known status of a component.
type Status struct {
	Name                string         `json:"name"`
	Critical            bool           `json:"critical"`
	State               State          `json:"state"`
	Detail              string         `json:"detail,omitempty"`
	Data                map[string]any `json:"data,omitempty"`
	CheckedAt           time.Time      `json:"checked_at"`
	ConsecutiveFailures int            `json:"consecutive_failures"`
	LastHealthyAt       time.Time      `json:"last_healthy_at,omitzero"`
}

// Snapshot is the device-wide view.
type Snapshot struct {
	Overall    State     `json:"overall"`
	Components []Status  `json:"components"`
	TakenAt    time.Time `json:"taken_at"`
}

// attentionAfter is the number of consecutive failures after which a
// component is escalated to REQUIRES_ATTENTION (a human should look).
const attentionAfter = 10

type entry struct {
	c       Component
	status  Status
	nextRun time.Time
}

// Monitor owns the component registry.
type Monitor struct {
	mu      sync.RWMutex
	entries map[string]*entry
	now     func() time.Time
	// listener is invoked (outside the lock) whenever a component's state
	// changes. Used for logging and for pushing status to the UI.
	listener func(prev, cur Status)
}

// NewMonitor creates an empty monitor.
func NewMonitor() *Monitor {
	return &Monitor{entries: map[string]*entry{}, now: time.Now}
}

// OnChange registers a state-change listener. Must be called before Run.
func (m *Monitor) OnChange(fn func(prev, cur Status)) { m.listener = fn }

// Register adds a component. Registering the same name twice is a programming
// error and panics.
func (m *Monitor) Register(c Component) {
	if c.Check == nil {
		panic("health: component " + c.Name + " has no check")
	}
	if c.Interval <= 0 {
		c.Interval = 30 * time.Second
	}
	if c.Timeout <= 0 {
		c.Timeout = 5 * time.Second
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.entries[c.Name]; dup {
		panic("health: duplicate component " + c.Name)
	}
	m.entries[c.Name] = &entry{
		c:      c,
		status: Status{Name: c.Name, Critical: c.Critical, State: Recovering, Detail: "not_yet_checked"},
	}
}

// Report lets event-driven code (e.g. an STT call that just failed) update a
// component immediately instead of waiting for the next periodic check.
func (m *Monitor) Report(name string, r Result) {
	m.apply(name, r)
}

// Run executes due checks until ctx is cancelled. It blocks.
func (m *Monitor) Run(ctx context.Context) {
	m.CheckAll(ctx)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.runDue(ctx)
		}
	}
}

// CheckAll runs every check once, immediately.
func (m *Monitor) CheckAll(ctx context.Context) {
	for _, name := range m.names() {
		if ctx.Err() != nil {
			return
		}
		m.runOne(ctx, name)
	}
}

func (m *Monitor) names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.entries))
	for n := range m.entries {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (m *Monitor) runDue(ctx context.Context) {
	now := m.now()
	for _, name := range m.names() {
		if ctx.Err() != nil {
			return
		}
		m.mu.RLock()
		due := !now.Before(m.entries[name].nextRun)
		m.mu.RUnlock()
		if due {
			m.runOne(ctx, name)
		}
	}
}

func (m *Monitor) runOne(ctx context.Context, name string) {
	m.mu.RLock()
	e := m.entries[name]
	c := e.c
	m.mu.RUnlock()

	cctx, cancel := context.WithTimeout(ctx, c.Timeout)
	r := safeCheck(cctx, c.Check)
	cancel()
	if ctx.Err() != nil {
		return // shutting down; don't record a spurious failure
	}
	if cctx.Err() == context.DeadlineExceeded && r.State == "" {
		r = Result{State: Unavailable, Detail: "check_timed_out"}
	}
	m.mu.Lock()
	e.nextRun = m.now().Add(c.Interval)
	m.mu.Unlock()
	m.apply(name, r)
}

// safeCheck converts a panicking check into an UNAVAILABLE result so one buggy
// probe cannot take down the agent.
func safeCheck(ctx context.Context, fn CheckFunc) (r Result) {
	defer func() {
		if p := recover(); p != nil {
			// Fixed reason code: details can reach the central server, so
			// no free text (the panic value) goes into them.
			_ = p
			r = Result{State: Unavailable, Detail: "check_panicked"}
		}
	}()
	return fn(ctx)
}

func (m *Monitor) apply(name string, r Result) {
	if r.State == "" {
		r.State = Unavailable
	}
	m.mu.Lock()
	e, ok := m.entries[name]
	if !ok {
		m.mu.Unlock()
		return
	}
	prev := e.status
	now := m.now()
	s := Status{
		Name: name, Critical: e.c.Critical, State: r.State, Detail: r.Detail, Data: r.Data,
		CheckedAt: now, LastHealthyAt: prev.LastHealthyAt,
	}
	switch r.State {
	case Healthy:
		s.ConsecutiveFailures = 0
		s.LastHealthyAt = now
	case Degraded, Recovering:
		s.ConsecutiveFailures = prev.ConsecutiveFailures
	default:
		s.ConsecutiveFailures = prev.ConsecutiveFailures + 1
		if s.ConsecutiveFailures >= attentionAfter {
			s.State = RequiresAttention
		}
	}
	e.status = s
	l := m.listener
	m.mu.Unlock()
	if l != nil && prev.State != s.State {
		l(prev, s)
	}
}

// Get returns the status of one component.
func (m *Monitor) Get(name string) (Status, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.entries[name]
	if !ok {
		return Status{}, false
	}
	return e.status, true
}

// IsUsable reports whether a component can currently be used (HEALTHY or
// DEGRADED). Unknown components are not usable.
func (m *Monitor) IsUsable(name string) bool {
	s, ok := m.Get(name)
	return ok && (s.State == Healthy || s.State == Degraded)
}

// Snapshot returns all statuses and the overall device state.
//
// Overall rules: any critical component that is not usable → REQUIRES_ATTENTION
// if it has escalated, otherwise UNAVAILABLE; any optional component not
// healthy → DEGRADED; otherwise the worst state seen, capped at DEGRADED for
// optional components.
func (m *Monitor) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	snap := Snapshot{Overall: Healthy, TakenAt: m.now()}
	worst := 0
	for _, e := range m.entries {
		s := e.status
		snap.Components = append(snap.Components, s)
		sev := s.State.severity()
		if !e.c.Critical && sev > Degraded.severity() {
			sev = Degraded.severity()
		}
		if sev > worst {
			worst = sev
			snap.Overall = stateForSeverity(sev)
		}
	}
	sort.Slice(snap.Components, func(i, j int) bool { return snap.Components[i].Name < snap.Components[j].Name })
	return snap
}

func stateForSeverity(sev int) State {
	for _, s := range []State{Healthy, Recovering, Degraded, Unavailable, RequiresAttention} {
		if s.severity() == sev {
			return s
		}
	}
	return RequiresAttention
}
