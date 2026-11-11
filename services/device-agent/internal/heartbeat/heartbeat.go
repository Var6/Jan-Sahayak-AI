// Package heartbeat sends the device's signed health snapshot to the central
// API (POST /v1/devices/heartbeat, body deviceapi.Heartbeat).
//
// One goroutine (Run) sends a heartbeat shortly after start and then every
// Interval (±10 % jitter). Failures back off (Interval doubling, capped at
// 30 min) and are only logged and counted: the kiosk works the same with or
// without the central server, and nothing waits on a heartbeat.
//
// Privacy: the snapshot carries health states with fixed reason codes,
// versions, outbox counts and resource numbers. No citizen data, no error
// text and no free text leave the device.
package heartbeat

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/deviceapi"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/health"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/identity"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/outbox"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/resources"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/syncer"
)

// Defaults and bounds.
const (
	DefaultInterval     = 5 * time.Minute
	DefaultInitialDelay = 30 * time.Second
	DefaultHTTPTimeout  = 20 * time.Second
	MaxBackoff          = 30 * time.Minute
	maxResponseBytes    = 64 << 10
	// sourceTimeout bounds collecting the snapshot (outbox stats, disk,
	// `ps`/vcgencmd) so a slow source cannot delay the loop for long.
	sourceTimeout = 10 * time.Second
)

// Fixed failure reason codes (Status.LastError). Error text goes only to
// the local log.
const (
	ReasonNetwork     = "network_error"
	ReasonClockSkew   = "clock_skew"
	ReasonUnauthorize = "unauthorized"
	ReasonTooFrequent = "too_frequent"
	ReasonBadResponse = "bad_response"
	ReasonInvalidBody = "heartbeat_invalid"
	ReasonInternal    = "internal_error"
)

// Sources are read when a heartbeat is built. Every field is optional; a
// missing source leaves its part of the heartbeat empty (never invented).
type Sources struct {
	Health        func() health.Snapshot
	Outbox        func(context.Context) (outbox.Stats, error)
	Sync          func() syncer.Status
	BundleVersion func(context.Context) (string, error)
	// Models maps a runtime component to a model/version label (static,
	// from configuration).
	Models map[string]string
	// DataDir is the filesystem whose free space is reported.
	DataDir string
	// StartedAt is the agent start time: uptime_seconds is the agent's
	// process uptime, so a restart shows centrally as an uptime reset.
	StartedAt time.Time
	// OnLatestBundle is called (on the heartbeat goroutine, must not
	// block) with the server's latest bundle version from each accepted
	// heartbeat.
	OnLatestBundle func(version string)
}

// Config for the worker.
type Config struct {
	BaseURL      string
	AgentVersion string
	Interval     time.Duration
	InitialDelay time.Duration
	HTTPTimeout  time.Duration
}

// Status is exposed to local health output.
type Status struct {
	LastAttemptAt       time.Time `json:"last_attempt_at,omitzero"`
	LastSuccessAt       time.Time `json:"last_success_at,omitzero"`
	LastError           string    `json:"last_error,omitempty"` // fixed reason code
	ConsecutiveFailures int       `json:"consecutive_failures"`
	Sent                int64     `json:"sent"`
	// ClockSkewSeconds is server minus device time, from the last
	// clock_skew refusal (0 = none seen).
	ClockSkewSeconds int64 `json:"clock_skew_seconds,omitempty"`
}

// Worker sends heartbeats.
type Worker struct {
	cfg    Config
	id     identity.Identity
	src    Sources
	client *http.Client
	log    *slog.Logger
	now    func() time.Time

	mu     sync.Mutex
	status Status
}

// New creates a worker. cfg.BaseURL must be set (callers create no worker
// for an offline-only device).
func New(cfg Config, id identity.Identity, src Sources, log *slog.Logger) *Worker {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.InitialDelay <= 0 {
		cfg.InitialDelay = DefaultInitialDelay
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = DefaultHTTPTimeout
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Worker{
		cfg: cfg, id: id, src: src, log: log, now: time.Now,
		client: &http.Client{
			Timeout: cfg.HTTPTimeout,
			// Never follow redirects: signed requests go only to BaseURL.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Status returns a copy of the current status.
func (w *Worker) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

// Run blocks until ctx is done.
func (w *Worker) Run(ctx context.Context) {
	wait := jitter(w.cfg.InitialDelay)
	for {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		wait = w.sendAndSchedule(ctx)
	}
}

// sendAndSchedule sends one heartbeat and returns the wait before the next.
func (w *Worker) sendAndSchedule(ctx context.Context) time.Duration {
	err := w.SendOnce(ctx)
	var tf *tooFrequentError
	switch {
	case err == nil:
		return jitter(w.cfg.Interval)
	case errors.As(err, &tf):
		return jitter(max(w.cfg.Interval, tf.retryAfter))
	case ctx.Err() != nil:
		return w.cfg.Interval
	}
	fails := w.Status().ConsecutiveFailures
	d := w.cfg.Interval
	for i := 1; i < fails && d < MaxBackoff; i++ {
		d *= 2
	}
	return jitter(min(d, max(MaxBackoff, w.cfg.Interval)))
}

// jitter returns d ± 10 %.
func jitter(d time.Duration) time.Duration {
	span := int64(d / 5)
	if span <= 0 {
		return d
	}
	n, err := rand.Int(rand.Reader, big.NewInt(span+1))
	if err != nil {
		return d
	}
	return d - time.Duration(span/2) + time.Duration(n.Int64())
}

type tooFrequentError struct{ retryAfter time.Duration }

func (e *tooFrequentError) Error() string { return "heartbeat: server says too frequent" }

// Build assembles the heartbeat from the sources.
func (w *Worker) Build(ctx context.Context) deviceapi.Heartbeat {
	ctx, cancel := context.WithTimeout(ctx, sourceTimeout)
	defer cancel()
	now := w.now()
	hb := deviceapi.Heartbeat{
		DeviceID:     w.id.DeviceID,
		SentAt:       now.UTC().Format(time.RFC3339),
		AgentVersion: clip(w.cfg.AgentVersion, deviceapi.MaxShortText),
		OS:           runtime.GOOS + "/" + runtime.GOARCH,
		Overall:      string(health.Unavailable),
	}
	if hb.AgentVersion == "" {
		hb.AgentVersion = "unknown"
	}
	if len(w.src.Models) > 0 {
		hb.Models = make(map[string]string, len(w.src.Models))
		for k, v := range w.src.Models {
			if len(hb.Models) >= deviceapi.MaxModels {
				break
			}
			hb.Models[clip(k, deviceapi.MaxShortText)] = clip(v, 2*deviceapi.MaxShortText)
		}
	}
	if w.src.BundleVersion != nil {
		if v, err := w.src.BundleVersion(ctx); err == nil {
			hb.BundleVersion = clip(v, deviceapi.MaxShortText)
		}
	}
	if w.src.Health != nil {
		snap := w.src.Health()
		hb.Overall = string(snap.Overall)
		for _, c := range snap.Components {
			if len(hb.Components) >= deviceapi.MaxComponents {
				break
			}
			hb.Components = append(hb.Components, deviceapi.ComponentHealth{
				Name: clip(c.Name, deviceapi.MaxShortText), State: string(c.State), Critical: c.Critical,
				Detail: deviceapi.ReasonCode(c.Detail), // reason codes only, never free text
			})
		}
	}
	if w.src.Outbox != nil {
		if st, err := w.src.Outbox(ctx); err == nil {
			hb.Outbox = deviceapi.OutboxCounts{
				Pending: st.ByState["PENDING"], Uploading: st.ByState["UPLOADING"], Acknowledged: st.ByState["ACKNOWLEDGED"],
				DeleteEligible: st.ByState["DELETE_ELIGIBLE"], Failed: st.ByState["FAILED"], Conflict: st.ByState["CONFLICT"],
				Total: st.Total, Capacity: st.Capacity, OldestPendingAt: st.OldestPendingAt,
			}
		} else {
			w.log.Debug("heartbeat: outbox stats unavailable", "err", err)
		}
	}
	if w.src.Sync != nil {
		s := w.src.Sync()
		hb.Sync.ConsecutiveFailures = s.ConsecutiveFailures
		if !s.LastSuccessAt.IsZero() {
			hb.Sync.LastSuccessAt = s.LastSuccessAt.UTC().Format(time.RFC3339)
		}
	}
	hb.Resources = w.readResources(ctx, now)
	return hb
}

func (w *Worker) readResources(ctx context.Context, now time.Time) deviceapi.Resources {
	var r deviceapi.Resources
	i64 := func(v uint64) *int64 {
		if v == 0 || v > 1<<62 {
			return nil
		}
		x := int64(v)
		return &x
	}
	if w.src.DataDir != "" {
		if d, err := resources.ReadDisk(w.src.DataDir); err == nil {
			r.DiskFreeBytes, r.DiskTotalBytes = i64(d.Free), i64(d.Total)
			if d.Total > 0 && d.Free == 0 {
				zero := int64(0)
				r.DiskFreeBytes = &zero // a full disk is a real 0, not "unknown"
			}
		}
	}
	m := resources.ReadMemory(ctx)
	r.MemAvailableBytes, r.MemTotalBytes = i64(m.SysAvail), i64(m.SysTotal)
	if c, ok, err := resources.ReadTemperature(); err == nil && ok && c >= -50 && c <= 150 {
		r.CPUTempC = &c
	}
	if t, ok, err := resources.ReadThrottle(ctx); err == nil && ok {
		th := t.ThrottledNow || t.FreqCappedNow || t.UnderVoltageNow || t.SoftTempLimitNow
		r.Throttled = &th
	}
	if !w.src.StartedAt.IsZero() {
		up := int64(now.Sub(w.src.StartedAt) / time.Second)
		if up >= 0 {
			r.UptimeSeconds = &up
		}
	}
	return r
}

// SendOnce builds, signs and sends one heartbeat.
func (w *Worker) SendOnce(ctx context.Context) error {
	w.mu.Lock()
	w.status.LastAttemptAt = w.now()
	w.mu.Unlock()

	hb := w.Build(ctx)
	if err := hb.Validate(); err != nil {
		// A bug on our side; never send a body the server will refuse.
		w.log.Error("heartbeat: built an invalid heartbeat", "err", err)
		return w.fail(ReasonInvalidBody, err)
	}
	body, err := json.Marshal(hb)
	if err != nil || len(body) > deviceapi.MaxHeartbeatBytes {
		return w.fail(ReasonInvalidBody, fmt.Errorf("heartbeat: body: %v (%d bytes)", err, len(body)))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.BaseURL+deviceapi.HeartbeatPath, bytes.NewReader(body))
	if err != nil {
		return w.fail(ReasonInternal, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := deviceapi.SignHTTP(req, w.id.DeviceID, w.id.PrivateKey, body, w.now()); err != nil {
		return w.fail(ReasonInternal, err)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return w.fail(ReasonNetwork, err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return w.fail(ReasonNetwork, err)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusTooManyRequests:
		ra, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		ra = min(max(ra, deviceapi.MinHeartbeatSecs), 3600)
		w.mu.Lock()
		w.status.LastError = ReasonTooFrequent
		w.mu.Unlock()
		return &tooFrequentError{retryAfter: time.Duration(ra) * time.Second}
	case resp.StatusCode == http.StatusUnauthorized && errorCode(rb) == "clock_skew":
		if st, err := strconv.ParseInt(resp.Header.Get("X-JS-Server-Time"), 10, 64); err == nil {
			skew := st - w.now().Unix()
			w.mu.Lock()
			w.status.ClockSkewSeconds = skew
			w.mu.Unlock()
			w.log.Warn("heartbeat: device clock refused by the server", "skew_seconds", skew)
		}
		return w.fail(ReasonClockSkew, errors.New("heartbeat: clock skew"))
	case resp.StatusCode == http.StatusUnauthorized:
		return w.fail(ReasonUnauthorize, errors.New("heartbeat: device not accepted (unregistered or revoked?)"))
	default:
		return w.fail("http_"+strconv.Itoa(resp.StatusCode), fmt.Errorf("heartbeat: HTTP %d: %.200s", resp.StatusCode, rb))
	}
	var hr deviceapi.HeartbeatResponse
	if len(rb) > maxResponseBytes || json.Unmarshal(rb, &hr) != nil {
		return w.fail(ReasonBadResponse, errors.New("heartbeat: undecodable response"))
	}
	w.mu.Lock()
	w.status.LastSuccessAt = w.now()
	w.status.LastError = ""
	w.status.ConsecutiveFailures = 0
	w.status.ClockSkewSeconds = 0
	w.status.Sent++
	w.mu.Unlock()
	w.log.Debug("heartbeat: sent", "overall", hb.Overall, "latest_bundle", hr.LatestBundleVersion)
	if hr.LatestBundleVersion != "" && len(hr.LatestBundleVersion) <= deviceapi.MaxShortText && w.src.OnLatestBundle != nil {
		w.src.OnLatestBundle(hr.LatestBundleVersion)
	}
	return nil
}

func (w *Worker) fail(reason string, err error) error {
	w.mu.Lock()
	w.status.LastError = reason
	w.status.ConsecutiveFailures++
	n := w.status.ConsecutiveFailures
	w.mu.Unlock()
	// Offline is normal for a kiosk: log the first failures and then
	// only occasionally.
	if n <= 3 || n%12 == 0 {
		w.log.Warn("heartbeat: not delivered", "reason", reason, "err", err, "consecutive_failures", n)
	}
	return fmt.Errorf("%s: %w", reason, err)
}

func errorCode(b []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &e)
	return e.Error.Code
}

// clip truncates s to at most n bytes on a rune boundary and drops control
// characters, so a heartbeat never fails validation on a long label.
func clip(s string, n int) string {
	var b []byte
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		var tmp [4]byte
		enc := tmp[:0]
		enc = append(enc, string(r)...)
		if len(b)+len(enc) > n {
			break
		}
		b = append(b, enc...)
	}
	return string(b)
}
