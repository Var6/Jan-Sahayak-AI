//go:build securityreview2

// Independent security-review probe for the heartbeat (build tag
// securityreview2).
//
//	go test -tags securityreview2 -race -count=1 -run Probe ./services/device-agent/internal/heartbeat/
package heartbeat

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/health"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/identity"
)

// Probe: health details are copied into the heartbeat verbatim (only
// clipped), so the "fixed reason codes only" rule is a convention, not an
// enforced allowlist. A future check that put an error string (which can
// quote input) into Detail would ship it centrally.
func TestProbeDetailIsNotAllowlisted(t *testing.T) {
	id, _, err := identity.LoadOrCreate(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	mon := health.NewMonitor()
	free := "stt failed on: मेरा नाम राम है 9876543210"
	mon.Register(health.Component{Name: "stt", Interval: time.Hour, Timeout: time.Second,
		Check: func(context.Context) health.Result { return health.Result{State: health.Unavailable, Detail: free} }})
	mon.CheckAll(context.Background())
	w := New(Config{BaseURL: "http://127.0.0.1:1", AgentVersion: "t"}, id, Sources{Health: mon.Snapshot}, nil)
	hb := w.Build(context.Background())
	b, _ := json.Marshal(hb)
	if err := hb.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if strings.Contains(string(b), "9876543210") {
		t.Logf("heartbeat JSON carries arbitrary detail text: %s", b)
		t.Error("FINDING (latent): component detail is not restricted to reason codes")
	}
}
