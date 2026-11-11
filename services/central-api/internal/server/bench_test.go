package server

import (
	"net/http"
	"testing"

	"github.com/janmanindia/jan-sahayak/internal/ids"
	"github.com/janmanindia/jan-sahayak/internal/syncproto"
)

// BenchmarkIngestBatch200 measures one full signed upload of a maximum-size
// batch (200 new events): HTTP + signature + nonce + one DB transaction +
// signed ack. Run: go test -run x -bench IngestBatch200 -benchtime 30x
func BenchmarkIngestBatch200(b *testing.B) {
	e := newEnv(b)
	d := e.newDevice("bench")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		evs := make([]syncproto.Event, 0, syncproto.MaxBatchEvents)
		for j := 0; j < syncproto.MaxBatchEvents/2; j++ {
			sid := ids.NewV4()
			evs = append(evs, mkEvent("dialogue.turn", turn(sid, 1, "pension", "answered")),
				mkEvent("session.ended", sessionEnded(sid, "hi", 1)))
		}
		br := d.batch(evs...)
		b.StartTimer()
		if code, body := e.upload(d, br); code != http.StatusOK {
			b.Fatalf("%d %s", code, body)
		}
	}
}
