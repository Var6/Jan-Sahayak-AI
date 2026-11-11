// Package bundlesync keeps the device's scheme knowledge in step with the
// central server: it fetches the latest signed knowledge bundle (signed
// GET /v1/devices/knowledge-bundle?since=<loaded version>), verifies it
// against the pinned server keys, refuses rollbacks, loads it atomically
// through the knowledge package and keeps the exact signed bytes on disk.
//
// Rules:
//   - a bundle is loaded only if its Ed25519 signature verifies against a
//     pinned server key (deviceapi.VerifyBundle) — on every profile, dev
//     included; the network is never trusted;
//   - a bundle older than the loaded one is refused (knowledge.ErrBundleRollback);
//   - the load replaces the scheme table in one transaction (knowledge.Store);
//     a bundle with no valid record is refused and the old knowledge stays;
//   - downloads are bounded (deviceapi.MaxBundleBytes) and time-limited;
//   - no download starts while a citizen session is open (Busy), and Busy
//     is asked again just before loading (a session may open during the
//     download), so the facts never change in the middle of a conversation;
//   - the kept file is re-verified (signature and rollback) before reuse
//     (RestoreFromDisk, e.g. after the database was rebuilt from a backup).
//
// Failures are logged with fixed reason codes and retried with backoff;
// the kiosk keeps answering from the knowledge it has.
package bundlesync

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/deviceapi"
	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/identity"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/knowledge"
)

// Defaults and bounds.
const (
	DefaultInterval     = 6 * time.Hour
	DefaultInitialDelay = 15 * time.Second
	DefaultHTTPTimeout  = 60 * time.Second
	// busyRetry is the wait while a citizen session is open.
	busyRetry = time.Minute
	// minFailBackoff..maxFailBackoff pace retries after a failure.
	minFailBackoff = 5 * time.Minute
	maxFailBackoff = time.Hour
	// minNotifyGap rate-limits checks triggered by heartbeat responses.
	minNotifyGap = 10 * time.Minute
)

// Outcome / reason codes (Status.LastResult, Status.LastError).
const (
	ResultLoaded      = "loaded"
	ResultUpToDate    = "up_to_date"
	ResultNoBundle    = "no_bundle_published"
	ReasonNetwork     = "network_error"
	ReasonUnauthorize = "unauthorized"
	ReasonClockSkew   = "clock_skew"
	ReasonTooLarge    = "bundle_too_large"
	ReasonChecksum    = "bundle_checksum_mismatch"
	ReasonDecode      = "bundle_undecodable"
	ReasonSignature   = "bundle_signature_invalid"
	ReasonVersion     = "bundle_version_malformed"
	ReasonRollback    = "bundle_rollback_refused"
	ReasonNoValid     = "bundle_no_valid_records"
	ReasonLoad        = "bundle_load_failed"
	ReasonInternal    = "internal_error"
)

// versionRe is the central bundle_version format (central-api, cmd/kb).
var versionRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}\.[0-9]{1,6}$`)

// ErrBusy is returned by CheckOnce while a citizen session is open.
var ErrBusy = errors.New("bundlesync: a session is open; check postponed")

// Config for the worker.
type Config struct {
	BaseURL    string
	ServerKeys map[string]ed25519.PublicKey
	// Interval between checks (default 6 h); one check runs InitialDelay
	// after Run starts.
	Interval     time.Duration
	InitialDelay time.Duration
	HTTPTimeout  time.Duration
	// CachePath keeps the last loaded bundle's signed bytes ("" = not kept).
	CachePath string
	// MaxBytes bounds a download and the cached file (default
	// deviceapi.MaxBundleBytes).
	MaxBytes int64
	// Busy reports whether a citizen session is open (nil = never busy).
	Busy func() bool
}

// Status is exposed to the knowledge health check.
type Status struct {
	LastCheckAt         time.Time `json:"last_check_at,omitzero"`
	LastSuccessAt       time.Time `json:"last_success_at,omitzero"` // last check that reached a verdict
	LastResult          string    `json:"last_result,omitempty"`
	LastError           string    `json:"last_error,omitempty"` // fixed reason code
	ConsecutiveFailures int       `json:"consecutive_failures"`
	LastLoadedVersion   string    `json:"last_loaded_version,omitempty"`
	LastLoadedAt        time.Time `json:"last_loaded_at,omitzero"`
	LastRefusedVersion  string    `json:"last_refused_version,omitempty"`
	CacheError          string    `json:"cache_error,omitempty"`
}

// Worker syncs bundles.
type Worker struct {
	cfg    Config
	ks     *knowledge.Store
	id     identity.Identity
	client *http.Client
	log    *slog.Logger
	now    func() time.Time
	kick   chan struct{}

	mu         sync.Mutex
	status     Status
	lastNotify time.Time
	// cachedVersion is the verified version known to be in CachePath
	// ("" = unknown, missing or not the loaded one): while it differs from
	// the loaded version, checks ask for the full bundle (no ?since) so the
	// file is rewritten.
	cachedVersion string
	// dbVersion is the bundle version last seen in the local database
	// (read at every check and restore, updated on load). NotifyLatest
	// compares against it, so a heartbeat announcing the version already
	// held never triggers a check — also after a restart, when nothing was
	// loaded by this process.
	dbVersion string
}

// New creates a worker.
func New(cfg Config, ks *knowledge.Store, id identity.Identity, log *slog.Logger) *Worker {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.InitialDelay <= 0 {
		cfg.InitialDelay = DefaultInitialDelay
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = DefaultHTTPTimeout
	}
	if cfg.MaxBytes <= 0 || cfg.MaxBytes > deviceapi.MaxBundleBytes {
		cfg.MaxBytes = deviceapi.MaxBundleBytes
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Worker{
		cfg: cfg, ks: ks, id: id, log: log, now: time.Now, kick: make(chan struct{}, 1),
		client: &http.Client{
			Timeout:       cfg.HTTPTimeout,
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

// Kick asks for a check now. Non-blocking.
func (w *Worker) Kick() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// NotifyLatest is called with the server's latest bundle version (from a
// heartbeat response). It kicks a check when that version is not the one
// loaded or last refused, at most once per 10 minutes. Non-blocking.
func (w *Worker) NotifyLatest(version string) {
	if !versionRe.MatchString(version) {
		return
	}
	w.mu.Lock()
	skip := version == w.dbVersion || version == w.status.LastLoadedVersion || version == w.status.LastRefusedVersion ||
		(!w.lastNotify.IsZero() && w.now().Sub(w.lastNotify) < minNotifyGap)
	if !skip {
		w.lastNotify = w.now()
	}
	w.mu.Unlock()
	if !skip {
		w.Kick()
	}
}

// Run restores the cached bundle if the database lacks it, then checks
// InitialDelay after start and every Interval, until ctx is done.
func (w *Worker) Run(ctx context.Context) {
	if rep, loaded, err := w.RestoreFromDisk(ctx); err != nil {
		w.log.Warn("bundlesync: cached bundle not restored", "err", err)
	} else if loaded {
		w.log.Info("bundlesync: restored cached bundle", "bundle_version", rep.BundleVersion, "loaded", rep.Loaded)
	}
	wait := jitter(w.cfg.InitialDelay)
	for {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-w.kick:
			t.Stop()
		case <-t.C:
		}
		wait = w.checkAndSchedule(ctx)
	}
}

func (w *Worker) checkAndSchedule(ctx context.Context) time.Duration {
	_, err := w.CheckOnce(ctx)
	switch {
	case err == nil:
		return jitter(w.cfg.Interval)
	case errors.Is(err, ErrBusy):
		return busyRetry
	case ctx.Err() != nil:
		return w.cfg.Interval
	}
	fails := w.Status().ConsecutiveFailures
	d := minFailBackoff
	for i := 1; i < fails && d < maxFailBackoff; i++ {
		d *= 2
	}
	return jitter(min(d, maxFailBackoff, w.cfg.Interval))
}

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

// Result describes one check.
type Result struct {
	Outcome string           // ResultLoaded | ResultUpToDate | ResultNoBundle
	Report  knowledge.Report // for ResultLoaded
}

// CheckOnce asks the server for a newer bundle and loads it if it verifies.
func (w *Worker) CheckOnce(ctx context.Context) (Result, error) {
	if w.cfg.Busy != nil && w.cfg.Busy() {
		return Result{}, ErrBusy
	}
	w.mu.Lock()
	w.status.LastCheckAt = w.now()
	w.mu.Unlock()

	cur, err := w.ks.BundleVersion(ctx)
	if err != nil {
		return Result{}, w.fail(ReasonInternal, "", err)
	}
	w.setDBVersion(cur)
	u, err := url.Parse(w.cfg.BaseURL + deviceapi.KnowledgeBundlePath)
	if err != nil {
		return Result{}, w.fail(ReasonInternal, "", err)
	}
	w.mu.Lock()
	cacheStale := w.cfg.CachePath != "" && w.cachedVersion != cur
	w.mu.Unlock()
	if versionRe.MatchString(cur) && !cacheStale {
		// A local/dev version ("local-…") is not a central version; the
		// server would refuse it, so ask for the latest instead. A stale
		// cache file also asks for the full bundle, to rewrite the file.
		u.RawQuery = url.Values{"since": {cur}}.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Result{}, w.fail(ReasonInternal, "", err)
	}
	if err := deviceapi.SignHTTP(req, w.id.DeviceID, w.id.PrivateKey, nil, w.now()); err != nil {
		return Result{}, w.fail(ReasonInternal, "", err)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return Result{}, w.fail(ReasonNetwork, "", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, w.cfg.MaxBytes+1))
	if err != nil {
		return Result{}, w.fail(ReasonNetwork, "", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNoContent:
		w.succeed(ResultUpToDate)
		return Result{Outcome: ResultUpToDate}, nil
	case http.StatusNotFound:
		if errorCode(body) == "no_bundle" {
			w.succeed(ResultNoBundle)
			return Result{Outcome: ResultNoBundle}, nil
		}
		return Result{}, w.fail("http_404", "", errors.New("bundlesync: HTTP 404"))
	case http.StatusUnauthorized:
		if errorCode(body) == "clock_skew" {
			return Result{}, w.fail(ReasonClockSkew, "", errors.New("bundlesync: device clock refused"))
		}
		return Result{}, w.fail(ReasonUnauthorize, "", errors.New("bundlesync: device not accepted (unregistered or revoked?)"))
	default:
		return Result{}, w.fail(fmt.Sprintf("http_%d", resp.StatusCode), "", fmt.Errorf("bundlesync: HTTP %d: %.200s", resp.StatusCode, body))
	}
	if int64(len(body)) > w.cfg.MaxBytes {
		return Result{}, w.fail(ReasonTooLarge, "", fmt.Errorf("bundlesync: bundle exceeds %d bytes", w.cfg.MaxBytes))
	}
	if want := resp.Header.Get("X-JS-Bundle-SHA256"); want != "" {
		// Transport corruption check only; the signature is what is trusted.
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != want {
			return Result{}, w.fail(ReasonChecksum, "", errors.New("bundlesync: body does not match X-JS-Bundle-SHA256"))
		}
	}
	// A citizen session may have opened during the download: postpone (the
	// next attempt downloads again; bundles are small and this is rare).
	if w.cfg.Busy != nil && w.cfg.Busy() {
		return Result{}, ErrBusy
	}
	rep, err := w.load(ctx, body)
	if err != nil {
		return Result{}, err
	}
	w.setDBVersion(rep.BundleVersion)
	if rep.BundleVersion == cur {
		if cacheStale {
			w.persist(body, cur)
		}
		w.succeed(ResultUpToDate)
		return Result{Outcome: ResultUpToDate}, nil
	}
	w.persist(body, rep.BundleVersion)
	w.mu.Lock()
	w.status.LastLoadedVersion = rep.BundleVersion
	w.status.LastLoadedAt = w.now()
	w.mu.Unlock()
	w.succeed(ResultLoaded)
	w.log.Info("bundlesync: knowledge bundle loaded", "bundle_version", rep.BundleVersion, "previous", cur,
		"loaded", rep.Loaded, "factual", rep.Factual, "skipped", len(rep.Skipped))
	for _, s := range rep.Skipped {
		w.log.Warn("bundlesync: record skipped", "source", s.Source, "id", s.ID, "reason", s.Reason)
	}
	return Result{Outcome: ResultLoaded, Report: rep}, nil
}

// load decodes, verifies and loads signed bundle bytes. A bundle whose
// version equals the loaded one is verified but not reloaded (Report with
// only BundleVersion set).
func (w *Worker) load(ctx context.Context, body []byte) (knowledge.Report, error) {
	var b scheme.Bundle
	if err := json.Unmarshal(body, &b); err != nil {
		return knowledge.Report{}, w.fail(ReasonDecode, "", err)
	}
	verify := func(x scheme.Bundle) error { return deviceapi.VerifyBundle(w.cfg.ServerKeys, x) }
	if err := verify(b); err != nil {
		return knowledge.Report{}, w.fail(ReasonSignature, b.BundleVersion, err)
	}
	if !versionRe.MatchString(b.BundleVersion) {
		return knowledge.Report{}, w.fail(ReasonVersion, "", fmt.Errorf("bundlesync: bundle_version %.70q", b.BundleVersion))
	}
	cur, err := w.ks.BundleVersion(ctx)
	if err != nil {
		return knowledge.Report{}, w.fail(ReasonInternal, "", err)
	}
	if b.BundleVersion == cur {
		return knowledge.Report{BundleVersion: cur}, nil
	}
	rep, err := w.ks.LoadBundle(ctx, b, verify)
	switch {
	case errors.Is(err, knowledge.ErrBundleRollback):
		return rep, w.fail(ReasonRollback, b.BundleVersion, err)
	case errors.Is(err, knowledge.ErrNothingValid):
		return rep, w.fail(ReasonNoValid, b.BundleVersion, err)
	case err != nil:
		return rep, w.fail(ReasonLoad, "", err)
	}
	return rep, nil
}

// RestoreFromDisk loads the cached bundle when it is not the loaded one
// (e.g. the database was rebuilt from an older backup or from scratch). The
// file is verified exactly like a download; an older bundle is refused.
func (w *Worker) RestoreFromDisk(ctx context.Context) (knowledge.Report, bool, error) {
	if w.cfg.CachePath == "" {
		return knowledge.Report{}, false, nil
	}
	body, err := readLimited(w.cfg.CachePath, w.cfg.MaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return knowledge.Report{}, false, nil
	}
	if err != nil {
		return knowledge.Report{}, false, err
	}
	cur, err := w.ks.BundleVersion(ctx)
	if err != nil {
		return knowledge.Report{}, false, err
	}
	rep, err := w.load(ctx, body)
	if err != nil {
		return rep, false, err
	}
	w.mu.Lock()
	w.cachedVersion = rep.BundleVersion
	w.dbVersion = rep.BundleVersion
	w.mu.Unlock()
	if rep.BundleVersion == cur {
		return rep, false, nil
	}
	w.mu.Lock()
	w.status.LastLoadedVersion = rep.BundleVersion
	w.status.LastLoadedAt = w.now()
	w.mu.Unlock()
	return rep, true, nil
}

func (w *Worker) setDBVersion(v string) {
	w.mu.Lock()
	w.dbVersion = v
	w.mu.Unlock()
}

func (w *Worker) succeed(result string) {
	w.mu.Lock()
	w.status.LastSuccessAt = w.now()
	w.status.LastResult = result
	w.status.LastError = ""
	w.status.ConsecutiveFailures = 0
	w.mu.Unlock()
}

// fail records a failure; refused is the bundle version that was refused
// on its merits ("" for transport/internal failures).
func (w *Worker) fail(reason, refused string, err error) error {
	w.mu.Lock()
	w.status.LastError = reason
	w.status.ConsecutiveFailures++
	if refused != "" {
		w.status.LastRefusedVersion = refused
	}
	n := w.status.ConsecutiveFailures
	w.mu.Unlock()
	lvl := slog.LevelWarn
	switch reason {
	case ReasonSignature, ReasonRollback, ReasonNoValid, ReasonChecksum, ReasonVersion:
		lvl = slog.LevelError // a bundle was offered and refused: worth a look
	}
	if lvl == slog.LevelError || n <= 3 || n%12 == 0 {
		w.log.Log(context.Background(), lvl, "bundlesync: no bundle loaded", "reason", reason, "err", err,
			"refused_version", refused, "consecutive_failures", n)
	}
	return fmt.Errorf("%s: %w", reason, err)
}

// persist writes the signed bytes atomically (temp file, fsync, rename,
// fsync dir). A failure is logged and reported; the database is the
// authority, the file only helps recovery.
func (w *Worker) persist(body []byte, version string) {
	if w.cfg.CachePath == "" {
		return
	}
	err := writeAtomic(w.cfg.CachePath, body)
	w.mu.Lock()
	if err != nil {
		w.status.CacheError = "bundle_cache_write_failed"
		w.cachedVersion = ""
	} else {
		w.status.CacheError = ""
		w.cachedVersion = version
	}
	w.mu.Unlock()
	if err != nil {
		w.log.Error("bundlesync: could not keep the bundle file", "path", w.cfg.CachePath, "err", err)
	}
}

func writeAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".bundle-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(0o640); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("bundlesync: %s exceeds %d bytes", path, limit)
	}
	return b, nil
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

// Age returns how old a central bundle is, from the date in its version
// ("YYYY-MM-DD.N", an IST calendar date), measured from the end of that
// day so a bundle is never "older" than it can be. ok=false for local/dev
// versions, which carry no date.
func Age(version string, now time.Time) (time.Duration, bool) {
	if !versionRe.MatchString(version) {
		return 0, false
	}
	ist := time.FixedZone("IST", 5*3600+1800)
	day, err := time.ParseInLocation("2006-01-02", version[:10], ist)
	if err != nil {
		return 0, false
	}
	age := now.Sub(day.Add(24 * time.Hour))
	if age < 0 {
		age = 0
	}
	return age, true
}
