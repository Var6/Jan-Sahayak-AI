// Package syncer uploads outbox events to the central API using the
// acknowledgement protocol in internal/syncproto.
//
// One goroutine (Run) loops: maintain → claim a batch → sign → POST →
// verify ack → apply. Every failure path returns the batch to PENDING;
// transport/HTTP-level failures never consume an event's attempt budget
// (see package outbox). The worker never
// deletes anything; deletion is outbox.Housekeep's job and only touches
// acknowledged rows.
package syncer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/syncproto"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/identity"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/outbox"
)

// Config for the worker.
type Config struct {
	BaseURL      string // e.g. https://api.jansahayak.example
	ServerKeys   map[string]ed25519.PublicKey
	BatchSize    int
	Interval     time.Duration // idle poll interval
	HTTPTimeout  time.Duration
	HousekeepDur time.Duration
}

// Status is exposed to health checks and the admin heartbeat.
type Status struct {
	LastAttemptAt       time.Time `json:"last_attempt_at,omitzero"`
	LastSuccessAt       time.Time `json:"last_success_at,omitzero"`
	LastError           string    `json:"last_error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	Online              bool      `json:"online"`
}

// Worker uploads events.
type Worker struct {
	cfg    Config
	ob     *outbox.Outbox
	id     identity.Identity
	client *http.Client
	log    *slog.Logger
	now    func() time.Time
	kick   chan struct{}

	mu     sync.Mutex
	status Status
}

// New creates a worker.
func New(cfg Config, ob *outbox.Outbox, id identity.Identity, log *slog.Logger) *Worker {
	if cfg.BatchSize <= 0 || cfg.BatchSize > syncproto.MaxBatchEvents {
		cfg.BatchSize = 100
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = 30 * time.Second
	}
	if cfg.HousekeepDur <= 0 {
		cfg.HousekeepDur = time.Hour
	}
	return &Worker{
		cfg: cfg, ob: ob, id: id, log: log, now: time.Now,
		client: &http.Client{
			Timeout: cfg.HTTPTimeout,
			// Never follow redirects: a 3xx (captive portal, misconfigured
			// proxy, hijacked DNS) is returned as-is and handled as a
			// non-200 retry, so signed batches are only ever sent to
			// BaseURL.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		kick: make(chan struct{}, 1),
	}
}

// Kick asks the worker to try now (e.g. after a session ends). Non-blocking.
func (w *Worker) Kick() {
	select {
	case w.kick <- struct{}{}:
	default:
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
	if n, err := w.ob.RecoverInterrupted(ctx); err != nil {
		w.log.Error("sync: recover interrupted uploads", "err", err)
	} else if n > 0 {
		w.log.Warn("sync: re-queued uploads interrupted by restart", "count", n)
	}
	housekeep := time.NewTicker(w.cfg.HousekeepDur)
	defer housekeep.Stop()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-housekeep.C:
			w.housekeep(ctx)
		case <-w.kick:
		case <-timer.C:
		}
		next := w.drain(ctx)
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(next)
	}
}

func (w *Worker) housekeep(ctx context.Context) {
	if p, d, err := w.ob.Housekeep(ctx); err != nil {
		w.log.Error("sync: housekeep", "err", err)
	} else if p+d > 0 {
		w.log.Info("sync: housekeep", "promoted", p, "deleted", d)
	}
	if n, err := w.ob.PruneSessions(ctx); err != nil {
		w.log.Error("sync: prune sessions", "err", err)
	} else if n > 0 {
		w.log.Info("sync: pruned old session rows", "count", n)
	}
}

// maintain runs outbox maintenance at the start of a sync pass, when no
// batch of this worker is in flight: stale UPLOADING rows (older than 2×
// the HTTP timeout) and FAILED rows due for their automatic re-queue go
// back to PENDING, and retry times pushed out by a backwards clock jump are
// capped.
func (w *Worker) maintain(ctx context.Context) {
	r, err := w.ob.Maintain(ctx, 2*w.cfg.HTTPTimeout)
	if err != nil {
		w.log.Error("sync: outbox maintenance", "err", err)
		return
	}
	if r.StaleUploading > 0 {
		w.log.Warn("sync: re-queued stale uploads", "count", r.StaleUploading)
	}
	if r.FailedRequeued > 0 {
		w.log.Warn("sync: automatically re-queued failed events", "count", r.FailedRequeued)
	}
	if r.ClockCapped > 0 {
		w.log.Warn("sync: capped retry times after a backwards clock jump", "count", r.ClockCapped)
	}
}

// drain uploads batches until the outbox has nothing due or an error occurs.
// It returns how long to wait before the next attempt.
func (w *Worker) drain(ctx context.Context) time.Duration {
	if w.cfg.BaseURL == "" {
		return w.cfg.Interval // sync not configured (offline-only device)
	}
	w.maintain(ctx)
	for i := 0; i < 50 && ctx.Err() == nil; i++ { // bounded per wake-up
		n, err := w.SyncOnce(ctx)
		if err != nil {
			fails := w.recordFailure(err)
			return backoffWithJitter(fails)
		}
		if n == 0 {
			break
		}
	}
	return w.cfg.Interval
}

func backoffWithJitter(fails int) time.Duration {
	d := 15 * time.Second
	for i := 1; i < fails && d < 30*time.Minute; i++ {
		d *= 2
	}
	if d > 30*time.Minute {
		d = 30 * time.Minute
	}
	j, _ := rand.Int(rand.Reader, big.NewInt(int64(d/4)+1))
	return d + time.Duration(j.Int64())
}

func (w *Worker) recordFailure(err error) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status.LastError = err.Error()
	w.status.ConsecutiveFailures++
	w.status.Online = false
	return w.status.ConsecutiveFailures
}

// SyncOnce uploads one batch. Returns the number of events sent (0 = idle).
func (w *Worker) SyncOnce(ctx context.Context) (int, error) {
	w.mu.Lock()
	w.status.LastAttemptAt = w.now()
	w.mu.Unlock()

	batch, evs, err := w.ob.Claim(ctx, w.cfg.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("claim: %w", err)
	}
	if len(evs) == 0 {
		w.markOnline()
		return 0, nil
	}
	req := syncproto.BatchRequest{BatchID: batch, DeviceID: w.id.DeviceID, SentAt: w.now().UTC().Format(time.RFC3339), Events: evs}
	ack, err := w.post(ctx, req)
	if err != nil {
		// Use a fresh context: the batch must go back to PENDING even if ctx
		// was cancelled mid-request (shutdown).
		if rerr := w.ob.RetryBatch(context.WithoutCancel(ctx), batch, err.Error()); rerr != nil {
			w.log.Error("sync: retry batch", "err", rerr)
		}
		return 0, err
	}
	results, err := syncproto.VerifyAck(w.cfg.ServerKeys, req, ack)
	if err != nil {
		if rerr := w.ob.RetryBatch(context.WithoutCancel(ctx), batch, err.Error()); rerr != nil {
			// Rows stay UPLOADING; maintain re-queues them on a later pass.
			w.log.Error("sync: retry batch", "err", rerr)
		}
		return 0, err
	}
	out, err := w.ob.ApplyAck(context.WithoutCancel(ctx), batch, results)
	if err != nil {
		// Rows stay UPLOADING; maintain re-queues them on a later pass.
		return 0, fmt.Errorf("apply ack: %w", err)
	}
	w.log.Info("sync: batch", "batch", batch, "sent", len(evs), "acked", out.Acknowledged,
		"retried", out.Retried, "conflicts", out.Conflicts, "failed", out.Failed)
	w.markOnline()
	return len(evs), nil
}

func (w *Worker) markOnline() {
	w.mu.Lock()
	w.status.Online = true
	w.status.LastSuccessAt = w.now()
	w.status.LastError = ""
	w.status.ConsecutiveFailures = 0
	w.mu.Unlock()
}

// errServer marks HTTP-level failures.
var errServer = errors.New("sync: server error")

func (w *Worker) post(ctx context.Context, req syncproto.BatchRequest) (syncproto.BatchAck, error) {
	var ack syncproto.BatchAck
	body, err := json.Marshal(req)
	if err != nil {
		return ack, err
	}
	nonceB := make([]byte, 16)
	if _, err := rand.Read(nonceB); err != nil {
		return ack, err
	}
	nonce := hex.EncodeToString(nonceB)
	ts := w.now().Unix()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.BaseURL+syncproto.Path, bytes.NewReader(body))
	if err != nil {
		return ack, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set(syncproto.HeaderDevice, w.id.DeviceID)
	hreq.Header.Set(syncproto.HeaderTS, strconv.FormatInt(ts, 10))
	hreq.Header.Set(syncproto.HeaderNonce, nonce)
	hreq.Header.Set(syncproto.HeaderSig, syncproto.SignRequest(w.id.PrivateKey, http.MethodPost, syncproto.Path, ts, nonce, body))
	resp, err := w.client.Do(hreq)
	if err != nil {
		return ack, fmt.Errorf("sync: request: %w", err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ack, fmt.Errorf("sync: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// HTTP success alone is never treated as an acknowledgement, and
		// non-200 is always a retry.
		return ack, fmt.Errorf("%w: HTTP %d: %.200s", errServer, resp.StatusCode, rb)
	}
	if err := json.Unmarshal(rb, &ack); err != nil {
		return ack, fmt.Errorf("sync: decode ack: %w", err)
	}
	return ack, nil
}
