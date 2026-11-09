package deviceapi

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/ids"
)

// HealthStates mirrors the device health states.
var HealthStates = []string{"HEALTHY", "DEGRADED", "UNAVAILABLE", "RECOVERING", "REQUIRES_ATTENTION"}

// Heartbeat limits.
const (
	MaxComponents    = 32
	MaxModels        = 16
	MaxShortText     = 64
	MaxDetailText    = 200
	MinHeartbeatSecs = 20 // server refuses heartbeats more frequent than this
)

// ComponentHealth is one entry of the device health snapshot.
type ComponentHealth struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Critical bool   `json:"critical,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// OutboxCounts summarises the device outbox (from outbox.Stats).
type OutboxCounts struct {
	Pending         int    `json:"pending"`
	Uploading       int    `json:"uploading"`
	Acknowledged    int    `json:"acknowledged"`
	DeleteEligible  int    `json:"delete_eligible"`
	Failed          int    `json:"failed"`
	Conflict        int    `json:"conflict"`
	Total           int    `json:"total"`
	Capacity        int    `json:"capacity"`
	OldestPendingAt string `json:"oldest_pending_at,omitempty"` // RFC 3339
}

// SyncStatus summarises the device sync worker.
type SyncStatus struct {
	LastSuccessAt       string `json:"last_success_at,omitempty"` // RFC 3339
	ConsecutiveFailures int    `json:"consecutive_failures"`
}

// Resources is a resource snapshot. Pointers are nil when unknown.
type Resources struct {
	DiskFreeBytes     *int64   `json:"disk_free_bytes,omitempty"`
	DiskTotalBytes    *int64   `json:"disk_total_bytes,omitempty"`
	MemAvailableBytes *int64   `json:"mem_available_bytes,omitempty"`
	MemTotalBytes     *int64   `json:"mem_total_bytes,omitempty"`
	CPUTempC          *float64 `json:"cpu_temp_c,omitempty"`
	Throttled         *bool    `json:"throttled,omitempty"`
	UptimeSeconds     *int64   `json:"uptime_seconds,omitempty"`
}

// Heartbeat is the body of POST /v1/devices/heartbeat.
type Heartbeat struct {
	DeviceID      string            `json:"device_id"`
	SentAt        string            `json:"sent_at"` // RFC 3339, device clock
	AgentVersion  string            `json:"agent_version"`
	OS            string            `json:"os,omitempty"`
	BundleVersion string            `json:"bundle_version,omitempty"`
	Models        map[string]string `json:"models,omitempty"` // component -> model/version label
	Overall       string            `json:"overall"`
	Components    []ComponentHealth `json:"components,omitempty"`
	Outbox        OutboxCounts      `json:"outbox"`
	Sync          SyncStatus        `json:"sync"`
	Resources     Resources         `json:"resources"`
}

// HeartbeatResponse is returned for an accepted heartbeat.
type HeartbeatResponse struct {
	ServerTime          string `json:"server_time"`
	LatestBundleVersion string `json:"latest_bundle_version,omitempty"`
}

func shortText(field, s string, max int) error {
	if len(s) > max {
		return fmt.Errorf("%s too long", field)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s contains control characters", field)
		}
	}
	return nil
}

func optTime(field, s string) error {
	if s == "" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
		return fmt.Errorf("%s must be RFC 3339", field)
	}
	return nil
}

func nonNeg(field string, p *int64) error {
	if p != nil && *p < 0 {
		return fmt.Errorf("%s must be >= 0", field)
	}
	return nil
}

// Validate checks structure and bounds.
func (h Heartbeat) Validate() error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	if ids.Validate(h.DeviceID) != nil {
		add(errors.New("device_id must be a uuid"))
	}
	if _, err := time.Parse(time.RFC3339Nano, h.SentAt); err != nil {
		add(errors.New("sent_at must be RFC 3339"))
	}
	if h.AgentVersion == "" {
		add(errors.New("agent_version required"))
	}
	add(shortText("agent_version", h.AgentVersion, MaxShortText))
	add(shortText("os", h.OS, MaxShortText))
	add(shortText("bundle_version", h.BundleVersion, MaxShortText))
	if len(h.Models) > MaxModels {
		add(errors.New("too many models"))
	}
	for k, v := range h.Models {
		add(shortText("models key", k, MaxShortText))
		add(shortText("models value", v, MaxShortText*2))
	}
	if !slices.Contains(HealthStates, h.Overall) {
		add(errors.New("bad overall state"))
	}
	if len(h.Components) > MaxComponents {
		add(errors.New("too many components"))
	}
	for _, c := range h.Components {
		if c.Name == "" {
			add(errors.New("component name required"))
		}
		add(shortText("component name", c.Name, MaxShortText))
		add(shortText("component detail", c.Detail, MaxDetailText))
		if !slices.Contains(HealthStates, c.State) {
			add(fmt.Errorf("component %q bad state", c.Name))
		}
	}
	o := h.Outbox
	for _, n := range []int{o.Pending, o.Uploading, o.Acknowledged, o.DeleteEligible, o.Failed, o.Conflict, o.Total, o.Capacity} {
		if n < 0 {
			add(errors.New("outbox counts must be >= 0"))
			break
		}
	}
	add(optTime("outbox.oldest_pending_at", o.OldestPendingAt))
	add(optTime("sync.last_success_at", h.Sync.LastSuccessAt))
	if h.Sync.ConsecutiveFailures < 0 {
		add(errors.New("sync.consecutive_failures must be >= 0"))
	}
	r := h.Resources
	add(nonNeg("disk_free_bytes", r.DiskFreeBytes))
	add(nonNeg("disk_total_bytes", r.DiskTotalBytes))
	add(nonNeg("mem_available_bytes", r.MemAvailableBytes))
	add(nonNeg("mem_total_bytes", r.MemTotalBytes))
	add(nonNeg("uptime_seconds", r.UptimeSeconds))
	if r.CPUTempC != nil && (*r.CPUTempC < -50 || *r.CPUTempC > 150) {
		add(errors.New("cpu_temp_c out of range"))
	}
	return errors.Join(errs...)
}
