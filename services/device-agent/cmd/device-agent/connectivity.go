package main

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/bundlesync"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/health"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/heartbeat"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/identity"
)

// processStart is when this agent process started (heartbeat uptime).
var processStart = time.Now()

// metaStartCount is the device_meta key counting agent starts (restarts are
// visible locally; centrally they show as an uptime reset).
const metaStartCount = "agent.start_count"

// connectivity holds the device ↔ central workers besides event upload:
// heartbeats and knowledge-bundle sync. Both are nil on an offline-only
// device (sync.base_url empty).
type connectivity struct {
	hb         *heartbeat.Worker
	bundles    *bundlesync.Worker
	startCount int64
}

// buildConnectivity creates the heartbeat and bundle-sync workers. It runs
// in build after the health monitor, session manager, outbox, sync worker
// and knowledge store exist.
func (a *agent) buildConnectivity(ctx context.Context, id identity.Identity, serverKeys map[string]ed25519.PublicKey) {
	if n, err := bumpStartCount(ctx, a.st.DB); err != nil {
		a.log.Warn("could not record the agent start count", "err", err)
	} else {
		a.conn.startCount = n
	}
	if a.cfg.Sync.BaseURL == "" {
		return
	}
	a.conn.bundles = bundlesync.New(bundlesync.Config{
		BaseURL: a.cfg.Sync.BaseURL, ServerKeys: serverKeys, Interval: a.cfg.Sync.BundleInterval.D(),
		HTTPTimeout: max(a.cfg.Sync.HTTPTimeout.D(), bundlesync.DefaultHTTPTimeout), CachePath: a.cfg.BundleCachePath(),
		// Never change the facts in the middle of a citizen's session.
		Busy: func() bool { return a.sm.Current().ID != "" },
	}, a.ks, id, a.log.With("component", "bundlesync"))
	models := map[string]string{"stt": a.cfg.Speech.STTEngine, "tts": "off", "llm": "off"}
	if a.cfg.Speech.TTSEnabled {
		models["tts"] = "speech-worker"
	}
	if a.cfg.LLM.Enabled {
		models["llm"] = "llama-server"
	}
	a.conn.hb = heartbeat.New(heartbeat.Config{
		BaseURL: a.cfg.Sync.BaseURL, AgentVersion: version, Interval: a.cfg.Sync.HeartbeatInterval.D(),
		HTTPTimeout: a.cfg.Sync.HTTPTimeout.D(),
	}, id, heartbeat.Sources{
		Health: a.mon.Snapshot, Outbox: a.ob.Stats, Sync: a.sync.Status, BundleVersion: a.ks.BundleVersion,
		Models: models, DataDir: a.cfg.DataDir, StartedAt: processStart,
		OnLatestBundle: a.conn.bundles.NotifyLatest,
	}, a.log.With("component", "heartbeat"))
}

// startConnectivity starts the workers on the agent's worker group.
func (a *agent) startConnectivity(ctx context.Context, goFn func(string, func())) {
	if a.conn.bundles != nil {
		goFn("bundlesync", func() { a.conn.bundles.Run(ctx) })
	}
	if a.conn.hb != nil {
		goFn("heartbeat", func() { a.conn.hb.Run(ctx) })
	}
}

// knowledgeFreshness adds bundle age and sync status to the knowledge
// health result; a central bundle older than knowledge.stale_after makes it
// DEGRADED ("bundle_stale").
func (a *agent) knowledgeFreshness(bundleVersion string, r *health.Result) {
	if r.Data == nil {
		r.Data = map[string]any{}
	}
	if age, ok := bundlesync.Age(bundleVersion, time.Now()); ok {
		r.Data["bundle_age_days"] = int(age / (24 * time.Hour))
		if age > a.cfg.Knowledge.StaleAfter.D() && r.State == health.Healthy {
			r.State, r.Detail = health.Degraded, "bundle_stale"
		}
	}
	if a.conn.bundles != nil {
		r.Data["bundle_sync"] = a.conn.bundles.Status()
	}
}

// connectivityStatus is technician-facing detail for GET /api/v1/health.
func (a *agent) connectivityStatus() map[string]any {
	m := map[string]any{"agent_start_count": a.conn.startCount}
	if a.conn.hb != nil {
		m["heartbeat"] = a.conn.hb.Status()
	}
	if a.conn.bundles != nil {
		m["bundle_sync"] = a.conn.bundles.Status()
	}
	return m
}

// bumpStartCount increments and returns device_meta[agent.start_count].
func bumpStartCount(ctx context.Context, db *sql.DB) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var v string
	err := db.QueryRowContext(ctx, `INSERT INTO device_meta(key, value, updated_at) VALUES(?, '1', strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		ON CONFLICT(key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT), updated_at = excluded.updated_at
		RETURNING value`, metaStartCount).Scan(&v)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, errors.New("agent.start_count is not a number")
	}
	return n, nil
}
