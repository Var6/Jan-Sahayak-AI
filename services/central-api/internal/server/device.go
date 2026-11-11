package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/janmanindia/jan-sahayak/internal/deviceapi"
	"github.com/janmanindia/jan-sahayak/internal/ids"
	"github.com/janmanindia/jan-sahayak/internal/syncproto"
)

var bundleVersionRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}\.[0-9]{1,6}$`)

// acquireDevice takes a device-request slot, waiting briefly. ok=false
// means a 503 was written.
func (s *Server) acquireDevice(w http.ResponseWriter, r *http.Request) bool {
	t := time.NewTimer(2 * time.Second)
	defer t.Stop()
	select {
	case s.devSem <- struct{}{}:
		return true
	case <-t.C:
	case <-r.Context().Done():
	}
	w.Header().Set("Retry-After", "30")
	writeError(w, http.StatusServiceUnavailable, "busy", "server busy, retry later")
	return false
}

func (s *Server) releaseDevice() { <-s.devSem }

// authenticateDevice verifies the signed request headers for body and
// records the nonce. It returns the device id, or writes a response and
// returns ok=false. Order: header syntax → active-device lookup → timestamp
// skew + Ed25519 signature (syncproto.VerifyRequest) → nonce insert
// (replay protection). Only authenticated requests can create nonce rows.
func (s *Server) authenticateDevice(ctx context.Context, w http.ResponseWriter, r *http.Request, body []byte) (string, bool) {
	devID := r.Header.Get(syncproto.HeaderDevice)
	nonce := r.Header.Get(syncproto.HeaderNonce)
	unauthorized := func(reason string) (string, bool) {
		s.log.Warn("device auth failed", "reason", reason, "device", trunc(devID, 36), "path", r.URL.Path)
		writeError(w, http.StatusUnauthorized, "unauthorized", "device authentication failed")
		return "", false
	}
	if ids.Validate(devID) != nil {
		return unauthorized("bad device header")
	}
	if !deviceapi.ValidNonce(nonce) {
		return unauthorized("bad nonce")
	}
	var pub []byte
	err := s.pool.QueryRow(ctx, `SELECT public_key FROM devices WHERE id = $1 AND status = 'active'`, devID).Scan(&pub)
	if errors.Is(err, pgx.ErrNoRows) {
		return unauthorized("unknown or revoked device")
	}
	if err != nil {
		s.log.Error("device auth: lookup", "err", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "temporarily unavailable")
		return "", false
	}
	now := s.now()
	// The signed target includes the query string (deviceapi.SignedTarget),
	// so e.g. ?since on the bundle endpoint cannot be altered in transit.
	err = syncproto.VerifyRequest(ed25519.PublicKey(pub), r.Method, deviceapi.SignedTarget(r.URL),
		r.Header.Get(syncproto.HeaderTS), nonce, r.Header.Get(syncproto.HeaderSig), body, now)
	if errors.Is(err, syncproto.ErrClockSkew) {
		s.log.Warn("device auth failed", "reason", "clock skew", "device", devID)
		w.Header().Set("X-JS-Server-Time", strconv.FormatInt(now.Unix(), 10))
		writeError(w, http.StatusUnauthorized, "clock_skew", "device clock is outside the allowed skew")
		return "", false
	}
	if err != nil {
		return unauthorized(err.Error())
	}
	// The nonce is remembered until the timestamp can no longer pass the
	// skew check (ts ± MaxClockSkew), plus margin.
	ts, _ := strconv.ParseInt(r.Header.Get(syncproto.HeaderTS), 10, 64)
	expires := time.Unix(ts, 0).Add(2 * syncproto.MaxClockSkew)
	tag, err := s.pool.Exec(ctx, `INSERT INTO sync_nonces (device_id, nonce, expires_at) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`, devID, nonce, expires)
	if err != nil {
		s.log.Error("device auth: nonce", "err", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "temporarily unavailable")
		return "", false
	}
	if tag.RowsAffected() == 0 {
		return unauthorized("replayed nonce")
	}
	if hook := s.afterDeviceAuth.Load(); hook != nil {
		(*hook)()
	}
	return devID, true
}

// errDeviceInactive means the device was revoked after its request was
// authenticated (security review finding 8).
var errDeviceInactive = errors.New("device is no longer active")

// lockActiveDevice re-checks inside tx that the device is still active and
// locks its row until commit, so a concurrent revocation (which takes the
// row lock FOR UPDATE in core.RevokeDevice) either completes before this
// check — and the request is refused — or waits until the transaction has
// committed. It also stamps last_seen_at, which needs the row lock anyway;
// taking it up front avoids upgrading a FOR SHARE lock later, which could
// deadlock against a waiting revocation.
func lockActiveDevice(ctx context.Context, tx pgx.Tx, devID string) error {
	tag, err := tx.Exec(ctx, `UPDATE devices SET last_seen_at = now() WHERE id = $1 AND status = 'active'`, devID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errDeviceInactive
	}
	return nil
}

// handleSyncEvents implements POST /v1/sync/events (see internal/syncproto).
// On any database error it returns 5xx and NO acknowledgement, so the
// device keeps its events and retries.
func (s *Server) handleSyncEvents(w http.ResponseWriter, r *http.Request) {
	if !s.acquireDevice(w, r) {
		return
	}
	defer s.releaseDevice()
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	body, ok := readBody(w, r, syncproto.MaxBodyBytes)
	if !ok {
		return
	}
	devID, ok := s.authenticateDevice(ctx, w, r, body)
	if !ok {
		return
	}
	var req syncproto.BatchRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "body is not a valid BatchRequest")
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.DeviceID != devID {
		writeError(w, http.StatusBadRequest, "bad_request", "device_id does not match the authenticated device")
		return
	}
	// Event ids must be well-formed and unique within the batch: the ack
	// contains exactly one result per id and its signing encoding relies on
	// ids having no separators. A malformed id is a device bug → 400.
	seen := make(map[string]struct{}, len(req.Events))
	for _, e := range req.Events {
		if ids.Validate(e.ID) != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "every event id must be a lowercase uuid")
			return
		}
		if _, dup := seen[e.ID]; dup {
			writeError(w, http.StatusBadRequest, "bad_request", "duplicate event id in batch")
			return
		}
		seen[e.ID] = struct{}{}
	}

	results, err := s.ingest(ctx, devID, req)
	if errors.Is(err, errDeviceInactive) {
		s.log.Warn("device auth failed", "reason", "revoked during request", "device", devID)
		writeError(w, http.StatusUnauthorized, "unauthorized", "device authentication failed")
		return
	}
	if err != nil {
		s.log.Error("sync: ingest failed, no ack sent", "device", devID, "batch", req.BatchID, "err", err)
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusServiceUnavailable, "unavailable", "events not stored; retry later")
		return
	}
	signer := s.keys.Load().Active()
	ack := syncproto.BatchAck{
		BatchID:    req.BatchID,
		DeviceID:   devID,
		ServerTime: s.now().UTC().Format(time.RFC3339),
		Results:    results,
		KeyID:      signer.KeyID,
	}
	syncproto.SignAck(signer.Priv, &ack)
	writeJSON(w, http.StatusOK, ack)
}

type pendingInsert struct {
	idx     int
	ev      syncproto.Event
	created time.Time
	receipt string
}

// ingest stores the valid events of a batch in one transaction and returns
// one result per request event, in request order.
func (s *Server) ingest(ctx context.Context, devID string, req syncproto.BatchRequest) ([]syncproto.Result, error) {
	results := make([]syncproto.Result, len(req.Events))
	var todo []pendingInsert
	for i, e := range req.Events {
		results[i].ID = e.ID
		reason := ""
		if err := e.Validate(); err != nil {
			reason = err.Error()
		} else if err := deviceapi.ValidatePayload(e.Type, e.SchemaVersion, e.Payload); err != nil {
			reason = err.Error()
		}
		if reason != "" {
			results[i].Status = syncproto.StatusRejected
			results[i].Reason = trunc(reason, syncproto.MaxReasonLength)
			continue
		}
		created, _ := time.Parse(time.RFC3339Nano, e.CreatedAt) // validated above
		todo = append(todo, pendingInsert{idx: i, ev: e, created: created, receipt: ids.NewV7()})
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := lockActiveDevice(ctx, tx, devID); err != nil {
		return nil, err
	}

	if len(todo) > 0 {
		b := &pgx.Batch{}
		for _, p := range todo {
			b.Queue(`INSERT INTO sync_events (id, device_id, batch_id, type, schema_version, created_at, payload, payload_sha256, receipt)
				VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9)
				ON CONFLICT (id) DO NOTHING`,
				p.ev.ID, devID, req.BatchID, p.ev.Type, p.ev.SchemaVersion, p.created, string(p.ev.Payload), p.ev.PayloadSHA256, p.receipt)
		}
		br := tx.SendBatch(ctx, b)
		var existing []string
		for _, p := range todo {
			tag, err := br.Exec()
			if err != nil {
				br.Close()
				return nil, err
			}
			if tag.RowsAffected() == 1 {
				results[p.idx].Status = syncproto.StatusAccepted
				results[p.idx].PayloadSHA256 = p.ev.PayloadSHA256
				results[p.idx].Receipt = p.receipt
			} else {
				existing = append(existing, p.ev.ID)
			}
		}
		if err := br.Close(); err != nil {
			return nil, err
		}
		if len(existing) > 0 {
			if err := s.classifyExisting(ctx, tx, devID, todo, existing, results); err != nil {
				return nil, err
			}
		}
	}
	if hook := s.beforeCommit.Load(); hook != nil {
		if err := (*hook)(ctx, tx); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return results, nil
}

// classifyExisting handles events whose id was already stored: same device
// and same hash → duplicate with the ORIGINAL receipt; otherwise conflict.
func (s *Server) classifyExisting(ctx context.Context, tx pgx.Tx, devID string, todo []pendingInsert, existing []string, results []syncproto.Result) error {
	type stored struct{ device, hash, receipt string }
	got := make(map[string]stored, len(existing))
	rows, err := tx.Query(ctx, `SELECT id::text, device_id::text, payload_sha256, receipt::text FROM sync_events WHERE id = ANY($1::uuid[])`, existing)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var st stored
		if err := rows.Scan(&id, &st.device, &st.hash, &st.receipt); err != nil {
			rows.Close()
			return err
		}
		got[id] = st
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	byID := make(map[string]pendingInsert, len(todo))
	for _, p := range todo {
		byID[p.ev.ID] = p
	}
	for _, id := range existing {
		p := byID[id]
		st, ok := got[id]
		if !ok {
			// Cannot happen (row exists, ON CONFLICT fired) unless the DB is
			// inconsistent; refuse to ack anything.
			return errors.New("sync: conflicting row vanished")
		}
		r := &results[p.idx]
		switch {
		case st.device == devID && st.hash == p.ev.PayloadSHA256:
			r.Status = syncproto.StatusDuplicate
			r.PayloadSHA256 = st.hash
			r.Receipt = st.receipt
		case st.device == devID:
			r.Status = syncproto.StatusConflict
			r.PayloadSHA256 = st.hash
			r.Reason = "event id already stored with a different payload"
		default:
			r.Status = syncproto.StatusConflict
			r.Reason = "event id already used"
		}
	}
	return nil
}

// handleHeartbeat implements POST /v1/devices/heartbeat.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if !s.acquireDevice(w, r) {
		return
	}
	defer s.releaseDevice()
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	body, ok := readBody(w, r, deviceapi.MaxHeartbeatBytes)
	if !ok {
		return
	}
	devID, ok := s.authenticateDevice(ctx, w, r, body)
	if !ok {
		return
	}
	var hb deviceapi.Heartbeat
	if err := json.Unmarshal(body, &hb); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "body is not a valid heartbeat")
		return
	}
	if err := hb.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", trunc(err.Error(), 500))
		return
	}
	if hb.DeviceID != devID {
		writeError(w, http.StatusBadRequest, "bad_request", "device_id does not match the authenticated device")
		return
	}
	devTime, _ := time.Parse(time.RFC3339Nano, hb.SentAt)
	models := hb.Models
	if models == nil {
		models = map[string]string{}
	}
	comps := hb.Components
	if comps == nil {
		comps = []deviceapi.ComponentHealth{}
	}
	mj, _ := json.Marshal(models)
	cj, _ := json.Marshal(comps)
	parseOpt := func(s string) *time.Time {
		if s == "" {
			return nil
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return nil
		}
		return &t
	}
	o, res := hb.Outbox, hb.Resources

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.dbUnavailable(w, "heartbeat: begin", err)
		return
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	// Serialise per device, re-check it is still active, and enforce the
	// minimum interval.
	if err := lockActiveDevice(ctx, tx, devID); errors.Is(err, errDeviceInactive) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "device authentication failed")
		return
	} else if err != nil {
		s.dbUnavailable(w, "heartbeat: lock", err)
		return
	}
	var recent bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM device_heartbeats
		WHERE device_id = $1 AND received_at > now() - make_interval(secs => $2))`,
		devID, float64(deviceapi.MinHeartbeatSecs)).Scan(&recent); err != nil {
		s.dbUnavailable(w, "heartbeat: rate check", err)
		return
	}
	if recent {
		w.Header().Set("Retry-After", strconv.Itoa(deviceapi.MinHeartbeatSecs))
		writeError(w, http.StatusTooManyRequests, "too_frequent", "heartbeat sent too frequently")
		return
	}
	_, err = tx.Exec(ctx, `INSERT INTO device_heartbeats (id, device_id, device_time, agent_version, os_info, bundle_version,
		models, overall_health, components, outbox_pending, outbox_uploading, outbox_acknowledged, outbox_delete_eligible,
		outbox_failed, outbox_conflict, outbox_total, outbox_capacity, outbox_oldest_pending_at, sync_last_success_at,
		sync_consecutive_failures, disk_free_bytes, disk_total_bytes, mem_available_bytes, mem_total_bytes, cpu_temp_c,
		throttled, uptime_seconds)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9::jsonb,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)`,
		ids.NewV7(), devID, devTime, hb.AgentVersion, hb.OS, hb.BundleVersion, string(mj), hb.Overall, string(cj),
		o.Pending, o.Uploading, o.Acknowledged, o.DeleteEligible, o.Failed, o.Conflict, o.Total, o.Capacity,
		parseOpt(o.OldestPendingAt), parseOpt(hb.Sync.LastSuccessAt), hb.Sync.ConsecutiveFailures,
		res.DiskFreeBytes, res.DiskTotalBytes, res.MemAvailableBytes, res.MemTotalBytes, res.CPUTempC, res.Throttled, res.UptimeSeconds)
	if err != nil {
		s.dbUnavailable(w, "heartbeat: insert", err)
		return
	}
	var latest *string
	if err := tx.QueryRow(ctx, `SELECT bundle_version FROM knowledge_bundles ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&latest); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.dbUnavailable(w, "heartbeat: latest bundle", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.dbUnavailable(w, "heartbeat: commit", err)
		return
	}
	resp := deviceapi.HeartbeatResponse{ServerTime: s.now().UTC().Format(time.RFC3339)}
	if latest != nil {
		resp.LatestBundleVersion = *latest
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) dbUnavailable(w http.ResponseWriter, what string, err error) {
	s.log.Error(what, "err", err)
	writeError(w, http.StatusServiceUnavailable, "unavailable", "temporarily unavailable")
}

// handleKnowledgeBundle implements GET /v1/devices/knowledge-bundle. The
// request is signed with an empty body; the signature covers the query
// string. If ?since equals the latest bundle version the answer is 204 No
// Content.
func (s *Server) handleKnowledgeBundle(w http.ResponseWriter, r *http.Request) {
	if !s.acquireDevice(w, r) {
		return
	}
	defer s.releaseDevice()
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	if r.ContentLength > 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "GET must not have a body")
		return
	}
	q := r.URL.Query()
	since := q.Get("since")
	if len(q) > 1 || (len(q) == 1 && !q.Has("since")) || len(q["since"]) > 1 || (since != "" && !bundleVersionRe.MatchString(since)) {
		writeError(w, http.StatusBadRequest, "bad_request", "only ?since=YYYY-MM-DD.N is accepted")
		return
	}
	devID, ok := s.authenticateDevice(ctx, w, r, nil)
	if !ok {
		return
	}
	if _, err := s.pool.Exec(ctx, `UPDATE devices SET last_seen_at = now() WHERE id = $1`, devID); err != nil {
		s.dbUnavailable(w, "bundle: last_seen", err)
		return
	}
	var version, sum string
	var signed []byte
	err := s.pool.QueryRow(ctx, `SELECT bundle_version, sha256, signed_json FROM knowledge_bundles
		ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&version, &sum, &signed)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no_bundle", "no knowledge bundle has been published")
		return
	}
	if err != nil {
		s.dbUnavailable(w, "bundle: select", err)
		return
	}
	if since != "" && since == version {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h := sha256.Sum256(signed)
	if hex.EncodeToString(h[:]) != sum {
		s.log.Error("bundle: stored bytes fail checksum", "bundle_version", version)
		writeError(w, http.StatusInternalServerError, "internal", "stored bundle failed integrity check")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-JS-Bundle-Version", version)
	w.Header().Set("X-JS-Bundle-SHA256", sum)
	w.WriteHeader(http.StatusOK)
	_, _ = bytes.NewReader(signed).WriteTo(w)
}
