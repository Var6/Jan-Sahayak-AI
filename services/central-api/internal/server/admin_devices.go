package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/janmanindia/jan-sahayak/services/central-api/internal/core"
)

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	list, err := core.ListDevices(r.Context(), s.pool)
	if err != nil {
		s.writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": list, "limit": core.MaxDeviceList})
}

type heartbeatDetail struct {
	ReceivedAt              time.Time       `json:"received_at"`
	DeviceTime              time.Time       `json:"device_time"`
	AgentVersion            string          `json:"agent_version"`
	OS                      string          `json:"os"`
	BundleVersion           string          `json:"bundle_version"`
	Models                  json.RawMessage `json:"models"`
	OverallHealth           string          `json:"overall_health"`
	Components              json.RawMessage `json:"components"`
	OutboxPending           int             `json:"outbox_pending"`
	OutboxUploading         int             `json:"outbox_uploading"`
	OutboxAcknowledged      int             `json:"outbox_acknowledged"`
	OutboxDeleteEligible    int             `json:"outbox_delete_eligible"`
	OutboxFailed            int             `json:"outbox_failed"`
	OutboxConflict          int             `json:"outbox_conflict"`
	OutboxTotal             int             `json:"outbox_total"`
	OutboxCapacity          int             `json:"outbox_capacity"`
	OutboxOldestPendingAt   *time.Time      `json:"outbox_oldest_pending_at,omitempty"`
	SyncLastSuccessAt       *time.Time      `json:"sync_last_success_at,omitempty"`
	SyncConsecutiveFailures int             `json:"sync_consecutive_failures"`
	DiskFreeBytes           *int64          `json:"disk_free_bytes,omitempty"`
	DiskTotalBytes          *int64          `json:"disk_total_bytes,omitempty"`
	MemAvailableBytes       *int64          `json:"mem_available_bytes,omitempty"`
	MemTotalBytes           *int64          `json:"mem_total_bytes,omitempty"`
	CPUTempC                *float32        `json:"cpu_temp_c,omitempty"`
	Throttled               *bool           `json:"throttled,omitempty"`
	UptimeSeconds           *int64          `json:"uptime_seconds,omitempty"`
}

type syncStats struct {
	EventsTotal    int64      `json:"events_total"`
	Events24h      int64      `json:"events_24h"`
	LastEventAt    *time.Time `json:"last_event_received_at,omitempty"`
	HeartbeatsKept int64      `json:"heartbeats_kept"`
}

func (s *Server) handleGetDevice(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, err := core.GetDevice(ctx, s.pool, r.PathValue("id"))
	if err != nil {
		s.writeCoreError(w, err)
		return
	}
	rows, err := s.pool.Query(ctx, `SELECT received_at, device_time, agent_version, os_info, bundle_version, models::text,
		overall_health, components::text, outbox_pending, outbox_uploading, outbox_acknowledged, outbox_delete_eligible,
		outbox_failed, outbox_conflict, outbox_total, outbox_capacity, outbox_oldest_pending_at, sync_last_success_at,
		sync_consecutive_failures, disk_free_bytes, disk_total_bytes, mem_available_bytes, mem_total_bytes, cpu_temp_c,
		throttled, uptime_seconds
		FROM device_heartbeats WHERE device_id = $1 ORDER BY received_at DESC LIMIT 20`, d.ID)
	if err != nil {
		s.writeCoreError(w, err)
		return
	}
	defer rows.Close()
	hbs := []heartbeatDetail{}
	for rows.Next() {
		var h heartbeatDetail
		var models, comps string
		if err := rows.Scan(&h.ReceivedAt, &h.DeviceTime, &h.AgentVersion, &h.OS, &h.BundleVersion, &models,
			&h.OverallHealth, &comps, &h.OutboxPending, &h.OutboxUploading, &h.OutboxAcknowledged, &h.OutboxDeleteEligible,
			&h.OutboxFailed, &h.OutboxConflict, &h.OutboxTotal, &h.OutboxCapacity, &h.OutboxOldestPendingAt,
			&h.SyncLastSuccessAt, &h.SyncConsecutiveFailures, &h.DiskFreeBytes, &h.DiskTotalBytes, &h.MemAvailableBytes,
			&h.MemTotalBytes, &h.CPUTempC, &h.Throttled, &h.UptimeSeconds); err != nil {
			s.writeCoreError(w, err)
			return
		}
		h.Models, h.Components = json.RawMessage(models), json.RawMessage(comps)
		hbs = append(hbs, h)
	}
	if err := rows.Err(); err != nil {
		s.writeCoreError(w, err)
		return
	}
	rows.Close()
	var st syncStats
	if err := s.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE received_at > now() - interval '24 hours'),
		max(received_at), (SELECT count(*) FROM device_heartbeats WHERE device_id = $1)
		FROM sync_events WHERE device_id = $1`, d.ID).Scan(&st.EventsTotal, &st.Events24h, &st.LastEventAt, &st.HeartbeatsKept); err != nil {
		s.writeCoreError(w, err)
		return
	}
	resp := map[string]any{"device": d, "sync": st, "recent_heartbeats": hbs}
	if len(hbs) > 0 {
		resp["last_heartbeat"] = hbs[0]
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleRegisterDevice(w http.ResponseWriter, r *http.Request) {
	var req core.NewDevice
	if !decodeAdmin(w, r, &req) {
		return
	}
	if err := core.RegisterDevice(r.Context(), s.pool, principalFrom(r).actor(), req); err != nil {
		s.writeCoreError(w, err)
		return
	}
	d, err := core.GetDevice(r.Context(), s.pool, req.ID)
	if err != nil {
		s.writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"device": d})
}

type revokeReq struct {
	Reason string `json:"reason"`
}

func (s *Server) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	var req revokeReq
	if !decodeAdmin(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if err := core.RevokeDevice(r.Context(), s.pool, principalFrom(r).actor(), id, req.Reason); err != nil {
		s.writeCoreError(w, err)
		return
	}
	d, err := core.GetDevice(r.Context(), s.pool, id)
	if err != nil {
		s.writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": d})
}
