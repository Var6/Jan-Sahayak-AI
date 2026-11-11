package hal

import (
	"context"
	"testing"
	"time"
	"unsafe"
)

func TestUAPILayout(t *testing.T) {
	if s := unsafe.Sizeof(gpioV2LineRequest{}); s != sizeofLineRequest {
		t.Fatalf("gpio_v2_line_request: %d bytes, kernel expects %d", s, sizeofLineRequest)
	}
	if s := unsafe.Sizeof(gpioV2LineEvent{}); s != sizeofLineEvent {
		t.Fatalf("gpio_v2_line_event: %d bytes, kernel expects %d", s, sizeofLineEvent)
	}
	if s := unsafe.Sizeof(gpioV2LineConfig{}); s != 272 {
		t.Fatalf("gpio_v2_line_config: %d bytes, kernel expects 272", s)
	}
	if off := unsafe.Offsetof(gpioV2LineRequest{}.Fd); off != 588 {
		t.Fatalf("fd offset %d, want 588", off)
	}
	// _IOWR(0xB4, 0x07, 592 bytes) — value from linux/gpio.h on arm64.
	if gpioV2GetLineIoctl != 0xC250B407 {
		t.Fatalf("ioctl number %#x", gpioV2GetLineIoctl)
	}
}

func TestBuildRequest(t *testing.T) {
	r := buildRequest(GPIOConfig{Line: 17, ActiveLow: true, PullUp: true, Debounce: 30 * time.Millisecond})
	if r.Offsets[0] != 17 || r.NumLines != 1 {
		t.Fatal("line")
	}
	want := uint64(gpioV2LineFlagInput | gpioV2LineFlagEdgeRising | gpioV2LineFlagEdgeFalling | gpioV2LineFlagActiveLow | gpioV2LineFlagBiasPullUp)
	if r.Config.Flags != want {
		t.Fatalf("flags %#x want %#x", r.Config.Flags, want)
	}
	if r.Config.NumAttrs != 1 || r.Config.Attrs[0].Attr.ID != gpioV2LineAttrIDDebounce || r.Config.Attrs[0].Attr.Value != 30000 || r.Config.Attrs[0].Mask != 1 {
		t.Fatalf("debounce attr %+v", r.Config.Attrs[0])
	}
	if string(r.Consumer[:11]) != "jan-sahayak" || r.Consumer[gpioMaxNameSize-1] != 0 {
		t.Fatal("consumer")
	}
	if r2 := buildRequest(GPIOConfig{Line: 3}); r2.Config.NumAttrs != 0 || r2.Config.Flags&gpioV2LineFlagActiveLow != 0 {
		t.Fatal("no debounce / active high")
	}
}

func TestPressFilter(t *testing.T) {
	f := pressFilter{minGap: time.Second}
	t0 := time.Unix(1000, 0)
	got := []bool{f.accept(t0), f.accept(t0.Add(200 * time.Millisecond)), f.accept(t0.Add(1100 * time.Millisecond))}
	if !got[0] || got[1] || !got[2] {
		t.Fatalf("got %v", got)
	}
}

func TestSim(t *testing.T) {
	var b Button = Sim{}
	if !b.Simulated() || b.Kind() != "sim" {
		t.Fatal("sim")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- b.Run(ctx, func() { t.Error("sim pressed itself") }) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
