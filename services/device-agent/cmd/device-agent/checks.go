package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/api"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/health"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/resources"
)

// Health component names (api.Comp* are the ones the API consults).
const (
	compSpeechWorker = "speech_worker"
	compSync         = "sync"
	compKnowledge    = "knowledge"
	compBackup       = "backup"
	compDisk         = "disk"
	compMemory       = "memory"
	compTemperature  = "temperature"
	compGoroutines   = "goroutines"
	compButton       = "button"
)

// speechLoadingLimit: a speech worker still loading models after this long
// is stuck (a Pi loads them in well under a minute) and needs a human.
const speechLoadingLimit = 5 * time.Minute

// Health details are fixed reason codes: they may be sent to the central
// server, so error text goes to the local log only.

// lastResult holds an event-driven result (backup) for its health check.
type lastResult struct {
	mu sync.Mutex
	r  health.Result
}

func (l *lastResult) set(r health.Result) { l.mu.Lock(); l.r = r; l.mu.Unlock() }
func (l *lastResult) get() health.Result  { l.mu.Lock(); defer l.mu.Unlock(); return l.r }

var backupStatus = &lastResult{r: health.Result{State: health.Healthy, Detail: "no_backup_since_start"}}

func (a *agent) registerChecks() {
	h := a.cfg.Health
	aiIv := h.AIInterval.D()

	// Database: critical. Ping every check; PRAGMA quick_check periodically.
	var lastQuick time.Time
	a.mon.Register(health.Component{Name: api.CompDatabase, Critical: true, Interval: h.DBInterval.D(), Timeout: 30 * time.Second,
		Check: func(ctx context.Context) health.Result {
			// Heartbeat of the health monitor loop: stamped only when a
			// periodic check completes (API failure reports do not count).
			defer stamp(&a.hb.health)
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := a.st.Ping(pctx)
			cancel()
			if err != nil {
				a.log.Error("health: database ping failed", "err", err)
				return health.Result{State: health.RequiresAttention, Detail: "db_ping_failed"}
			}
			data := map[string]any{}
			if time.Since(lastQuick) >= h.QuickCheckInterval.D() {
				start := time.Now()
				ok, detail := a.st.QuickCheck(ctx)
				if !ok {
					a.log.Error("health: database quick_check failed", "detail", detail)
					return health.Result{State: health.RequiresAttention, Detail: "db_quick_check_failed"}
				}
				lastQuick = time.Now()
				data["quick_check_ms"] = time.Since(start).Milliseconds()
			}
			data["last_quick_check"] = lastQuick.UTC().Format(time.RFC3339)
			return health.Result{State: health.Healthy, Data: data}
		}})

	if a.cfg.Speech.STTEngine == "speech-worker" || a.cfg.Speech.TTSEnabled {
		client := &http.Client{Timeout: 3 * time.Second}
		url := a.cfg.Speech.URL + "/health"
		var loadingSince time.Time // guarded by the monitor (checks run one at a time)
		a.mon.Register(health.Component{Name: compSpeechWorker, Interval: aiIv, Timeout: 4 * time.Second,
			Check: func(ctx context.Context) health.Result {
				st, code := speechWorkerHealth(ctx, client, url)
				if code != "speech_loading" {
					loadingSince = time.Time{}
					return health.Result{State: st, Detail: code}
				}
				if loadingSince.IsZero() {
					loadingSince = time.Now()
				}
				if time.Since(loadingSince) > speechLoadingLimit {
					return health.Result{State: health.RequiresAttention, Detail: "speech_loading_stuck"}
				}
				return health.Result{State: health.Recovering, Detail: code}
			}})
	}
	if a.stt != nil {
		a.mon.Register(health.Component{Name: api.CompSTT, Interval: aiIv, Timeout: 4 * time.Second, Check: a.clientCheck(api.CompSTT, a.stt.Health)})
	}
	if a.tts != nil {
		a.mon.Register(health.Component{Name: api.CompTTS, Interval: aiIv, Timeout: 4 * time.Second, Check: a.clientCheck(api.CompTTS, a.tts.Health)})
	}
	if a.llm != nil {
		a.mon.Register(health.Component{Name: api.CompLLM, Interval: aiIv, Timeout: 4 * time.Second, Check: a.clientCheck(api.CompLLM, a.llm.Health)})
	}

	a.mon.Register(health.Component{Name: compSync, Interval: time.Minute, Timeout: 5 * time.Second,
		Check: func(ctx context.Context) health.Result {
			st := a.sync.Status()
			obs, err := a.ob.Stats(ctx)
			if err != nil {
				a.log.Error("health: outbox stats failed", "err", err)
				return health.Result{State: health.Unavailable, Detail: "outbox_stats_failed"}
			}
			data := map[string]any{"outbox": obs, "status": st}
			r := health.Result{State: health.Healthy, Data: data}
			stuck := obs.ByState["FAILED"] + obs.ByState["CONFLICT"]
			data["failed_or_conflict"] = stuck
			switch {
			case obs.Capacity > 0 && obs.Total*10 >= obs.Capacity*9:
				r.State, r.Detail = health.Degraded, "outbox_above_90pct"
			case stuck > 0:
				r.State, r.Detail = health.Degraded, "events_failed_or_conflict"
			case a.cfg.Sync.BaseURL == "":
				r.Detail = "sync_not_configured"
			case st.ConsecutiveFailures >= h.SyncFailWarn:
				r.State, r.Detail = health.Degraded, "upload_failures"
			}
			return r
		}})

	// Outbox capacity: critical. A full outbox refuses new analytics events
	// (existing ones are never dropped), which needs a technician, so the
	// device goes REQUIRES_ATTENTION (the API also reports it the moment an
	// insert is refused).
	a.mon.Register(health.Component{Name: api.CompOutbox, Critical: true, Interval: time.Minute, Timeout: 5 * time.Second,
		Check: func(ctx context.Context) health.Result {
			// Full reads a single counter row (no count(*)); a read error
			// counts as not full — the database check owns DB faults.
			if a.ob.Full(ctx) {
				return health.Result{State: health.RequiresAttention, Detail: api.ReasonOutboxFull}
			}
			return health.Result{State: health.Healthy}
		}})

	a.mon.Register(health.Component{Name: compKnowledge, Interval: 5 * time.Minute, Timeout: 5 * time.Second,
		Check: func(ctx context.Context) health.Result {
			var total, verified int
			err := a.st.DB.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(status = 'VERIFIED'), 0) FROM schemes`).Scan(&total, &verified)
			if err != nil {
				a.log.Error("health: knowledge query failed", "err", err)
				return health.Result{State: health.Unavailable, Detail: "knowledge_query_failed"}
			}
			bv, _ := a.ks.BundleVersion(ctx)
			r := health.Result{State: health.Healthy, Data: map[string]any{
				"records": total, "verified": verified, "bundle_version": bv,
				"unverified_shown_with_disclaimer": a.ks.Config().UnverifiedAllowed(),
			}}
			switch {
			case total == 0:
				r.State, r.Detail = health.Degraded, "no_records"
			case verified == 0 && !a.ks.Config().UnverifiedAllowed():
				r.State, r.Detail = health.Degraded, "no_verified_records"
			}
			return r
		}})

	a.mon.Register(health.Component{Name: compBackup, Interval: time.Hour, Timeout: time.Second,
		Check: func(context.Context) health.Result { return backupStatus.get() }})

	th := resources.Thresholds{
		MemWarnMB: h.MemWarnMB, MemCritMB: h.MemCritMB, SysMemWarnPct: h.SysMemWarnPct, SysMemCritPct: h.SysMemCritPct,
		DiskWarnMB: h.DiskWarnMB, DiskCritMB: h.DiskCritMB, TempWarnC: h.TempWarnC, TempCritC: h.TempCritC,
		GoroutineWarn: h.GoroutineWarn, GoroutineCrit: h.GoroutineCrit,
	}
	if a.button.Kind() == "gpio" {
		// Critical: a kiosk whose button does not work cannot start
		// sessions. Escalates to REQUIRES_ATTENTION after repeated checks.
		a.mon.Register(health.Component{Name: compButton, Critical: true, Interval: 10 * time.Second, Timeout: time.Second,
			Check: func(context.Context) health.Result {
				r := health.Result{State: health.Healthy, Data: map[string]any{"failures": a.btn.failures.Load()}}
				switch {
				case a.btn.held.Load():
				case a.btn.failures.Load() == 0:
					r.State, r.Detail = health.Recovering, "button_starting"
				default:
					r.State, r.Detail = health.Unavailable, "button_gpio_unavailable"
				}
				return r
			}})
	}

	ri := h.ResourceInterval.D()
	a.mon.Register(health.Component{Name: compDisk, Interval: ri, Timeout: 3 * time.Second, Check: resources.DiskCheck(a.cfg.DataDir, th)})
	a.mon.Register(health.Component{Name: compMemory, Interval: ri, Timeout: 3 * time.Second, Check: resources.MemoryCheck(th)})
	a.mon.Register(health.Component{Name: compTemperature, Interval: ri, Timeout: 3 * time.Second, Check: resources.TemperatureCheck(th)})
	a.mon.Register(health.Component{Name: compGoroutines, Interval: ri, Timeout: time.Second, Check: resources.GoroutineCheck(th)})
}

// clientCheck wraps an AI client's health probe; the error text (which can
// embed upstream response bytes) is logged, the detail is a fixed code.
func (a *agent) clientCheck(name string, fn func(context.Context) error) health.CheckFunc {
	return func(ctx context.Context) health.Result {
		if err := fn(ctx); err != nil {
			a.log.Debug("health: runtime check failed", "component", name, "err", err)
			return health.Result{State: health.Unavailable, Detail: name + "_unreachable"}
		}
		return health.Result{State: health.Healthy}
	}
}

// speechWorkerHealth maps the worker's GET /health to a state and code:
// 200 ok; 503 {"status":"loading"} while models load; 503
// {"status":"wedged"} when an inference is hung (the worker then exits and
// systemd restarts it).
func speechWorkerHealth(ctx context.Context, client *http.Client, url string) (health.State, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return health.Unavailable, "speech_unreachable"
	}
	resp, err := client.Do(req)
	if err != nil {
		return health.Unavailable, "speech_unreachable"
	}
	defer resp.Body.Close()
	var body struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&body)
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
	switch {
	case resp.StatusCode == http.StatusOK:
		return health.Healthy, ""
	case resp.StatusCode == http.StatusServiceUnavailable && body.Status == "wedged":
		return health.Unavailable, "speech_engine_hung"
	case resp.StatusCode == http.StatusServiceUnavailable:
		return health.Recovering, "speech_loading"
	default:
		return health.Unavailable, fmt.Sprintf("speech_http_%d", resp.StatusCode)
	}
}

// healthExtra adds technician-facing counters to GET /api/v1/health.
func (a *agent) healthExtra(ctx context.Context) map[string]any {
	es := a.engine.Stats()
	return map[string]any{
		"profile": a.cfg.Profile,
		"dialogue": map[string]any{
			"classify_calls": es.ClassifyCalls, "classify_failed": es.ClassifyFailed,
			"rephrase_accepted": es.RephraseAccepted, "rephrase_rejected": es.RephraseRejected, "rephrase_failed": es.RephraseFailed,
		},
		"sync":   a.sync.Status(),
		"memory": resources.ReadMemory(ctx),
	}
}
