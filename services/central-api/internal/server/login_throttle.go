package server

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Login throttling (decision 7, security review findings 3 and 15).
//
// There is no hard account lock: a lock lets anyone deny service to a known
// admin and, answered differently from an unknown username, reveals which
// usernames exist. Instead every login attempt is delayed according to the
// recent failures of its client IP and of the submitted username, whichever
// is larger. The username key is the submitted string whether or not such a
// user exists, so the delay reveals nothing either.
//
//   - failures within loginWindow are counted per IP and per username;
//   - the first loginFreeFailures failures cost nothing, then the delay
//     doubles from cfg.LoginBaseDelay up to cfg.LoginMaxDelay, which is
//     reached at maxFailedLogins failures;
//   - a successful login clears the username counter (not the IP counter,
//     so one valid account cannot be used to reset an IP's guessing budget);
//   - at most loginPerIPInflight attempts per IP run at once, and a bounded
//     number of attempts may be waiting overall: clients with no recent
//     failures ("clean") have their own pool and their own argon2 slot, so
//     a flood of failing guesses cannot starve legitimate logins.
//
// Both maps are bounded (loginMaxKeys) and entries expire loginWindow after
// the last failure. When a map is full, expired entries are pruned and then
// arbitrary entries are evicted (Go map iteration order), which costs the
// attacker at least loginMaxKeys distinct keys per eviction of a victim.
const (
	// maxFailedLogins is the failure count (per IP or username, within
	// loginWindow) at which the delay reaches cfg.LoginMaxDelay.
	maxFailedLogins    = 10
	loginFreeFailures  = 3
	loginWindow        = 15 * time.Minute
	loginMaxKeys       = 20000
	loginPerIPInflight = 2
	loginMaxWaiting    = 48 // attempts in flight from IPs with recent failures
	loginMaxClean      = 16 // attempts in flight from clean IPs
)

type throttleEntry struct {
	failures int
	last     time.Time // last failure
	inflight int       // IP entries only
}

type loginThrottle struct {
	mu     sync.Mutex
	now    func() time.Time
	base   time.Duration
	max    time.Duration
	byIP   map[string]*throttleEntry
	byUser map[string]*throttleEntry
	// lastPrune is when expired entries were last swept (guarded by mu).
	lastPrune time.Time
	dirty     chan struct{} // semaphore: attempts from IPs with failures
	clean     chan struct{} // semaphore: attempts from clean IPs
}

func newLoginThrottle(now func() time.Time, base, max time.Duration) *loginThrottle {
	return &loginThrottle{now: now, base: base, max: max,
		byIP: map[string]*throttleEntry{}, byUser: map[string]*throttleEntry{},
		dirty: make(chan struct{}, loginMaxWaiting), clean: make(chan struct{}, loginMaxClean)}
}

// delayFor maps a failure count to a delay.
func (t *loginThrottle) delayFor(failures int) time.Duration {
	if failures < loginFreeFailures {
		return 0
	}
	if failures >= maxFailedLogins {
		return t.max
	}
	d := t.base << (failures - loginFreeFailures)
	if d > t.max || d <= 0 {
		return t.max
	}
	return d
}

// live returns the failure count of e, treating expired entries as zero.
func (t *loginThrottle) live(e *throttleEntry, now time.Time) int {
	if e == nil || now.Sub(e.last) > loginWindow {
		return 0
	}
	return e.failures
}

// loginTicket is one admitted attempt.
type loginTicket struct {
	t       *loginThrottle
	ip      string
	user    string
	delay   time.Duration
	trusted bool // client IP had no recent failures
	sem     chan struct{}
}

// admit registers an attempt. ok=false means the caller must answer 429
// (too many concurrent attempts); the answer does not depend on the
// username. The caller must call ticket.done() when ok.
func (t *loginThrottle) admit(ip, user string) (*loginTicket, bool) {
	t.mu.Lock()
	now := t.now()
	e := t.byIP[ip]
	if e != nil && e.inflight >= loginPerIPInflight {
		t.mu.Unlock()
		return nil, false
	}
	ipFails := t.live(e, now)
	userFails := t.live(t.byUser[user], now)
	if e == nil {
		e = t.insert(t.byIP, ip, now)
		e.last = time.Time{} // no failure yet
	}
	e.inflight++
	t.mu.Unlock()

	tk := &loginTicket{t: t, ip: ip, user: user, trusted: ipFails == 0,
		delay: max(t.delayFor(ipFails), t.delayFor(userFails))}
	tk.sem = t.dirty
	if tk.trusted {
		tk.sem = t.clean
	}
	select {
	case tk.sem <- struct{}{}:
	default:
		t.releaseInflight(ip)
		return nil, false
	}
	return tk, true
}

// insert adds a fresh entry, enforcing the size cap. Caller holds mu.
func (t *loginThrottle) insert(m map[string]*throttleEntry, key string, now time.Time) *throttleEntry {
	if len(m) >= loginMaxKeys {
		// Full scans for expired entries at most once a minute, so a flood
		// of new keys costs O(1) per insert, not O(loginMaxKeys).
		if now.Sub(t.lastPrune) > time.Minute {
			t.lastPrune = now
			for _, mm := range []map[string]*throttleEntry{t.byIP, t.byUser} {
				for k, v := range mm {
					if v.inflight == 0 && now.Sub(v.last) > loginWindow {
						delete(mm, k)
					}
				}
			}
		}
		for k, v := range m {
			if len(m) < loginMaxKeys {
				break
			}
			if v.inflight == 0 {
				delete(m, k)
			}
		}
	}
	e := &throttleEntry{last: now}
	m[key] = e
	return e
}

func (t *loginThrottle) releaseInflight(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.byIP[ip]; e != nil && e.inflight > 0 {
		e.inflight--
		if e.inflight == 0 && e.failures == 0 {
			delete(t.byIP, ip)
		}
	}
}

// done releases the attempt's concurrency slots.
func (tk *loginTicket) done() {
	<-tk.sem
	tk.t.releaseInflight(tk.ip)
}

// failed records a failed attempt for both keys.
func (tk *loginTicket) failed() {
	t := tk.t
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	bump := func(m map[string]*throttleEntry, key string) {
		e := m[key]
		if e == nil {
			e = t.insert(m, key, now)
		}
		if now.Sub(e.last) > loginWindow {
			e.failures = 0
		}
		e.failures++
		e.last = now
	}
	bump(t.byIP, tk.ip)
	bump(t.byUser, tk.user)
}

// succeeded clears the username counter.
func (tk *loginTicket) succeeded() {
	t := tk.t
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.byUser, tk.user)
}

// sizes reports the map sizes (tests).
func (t *loginThrottle) sizes() (ips, users int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.byIP), len(t.byUser)
}

// clientIP returns the throttling key for the request's client. With
// trustProxy (a reverse proxy on the same host, see deployment/server/
// Caddyfile) and a loopback peer, the rightmost X-Forwarded-For entry is
// used: the proxy sets it to the address it accepted the connection from.
// IPv6 addresses are reduced to their /64 so one host cannot rotate
// addresses within its own prefix.
func clientIP(r *http.Request, trustProxy bool) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err == nil && trustProxy && addr.IsLoopback() {
		if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
			last := xff[len(xff)-1]
			if i := strings.LastIndexByte(last, ','); i >= 0 {
				last = last[i+1:]
			}
			if a, err := netip.ParseAddr(strings.TrimSpace(last)); err == nil {
				addr = a
			}
		}
	}
	if err != nil && !addr.IsValid() {
		return "unknown"
	}
	addr = addr.Unmap()
	if addr.Is6() {
		p, _ := addr.Prefix(64)
		return p.String()
	}
	return addr.String()
}
