package hal

// Linux GPIO character-device uAPI v2 (include/uapi/linux/gpio.h).
// The structs are defined here without a build tag so their sizes can be
// checked by tests on any platform; only the ioctl code is linux-only.

const (
	gpioV2LinesMax        = 64
	gpioMaxNameSize       = 32
	gpioV2LineNumAttrsMax = 10

	gpioV2LineFlagActiveLow   = 1 << 1
	gpioV2LineFlagInput       = 1 << 2
	gpioV2LineFlagEdgeRising  = 1 << 4
	gpioV2LineFlagEdgeFalling = 1 << 5
	gpioV2LineFlagBiasPullUp  = 1 << 8

	gpioV2LineAttrIDDebounce = 3

	gpioV2LineEventRisingEdge  = 1
	gpioV2LineEventFallingEdge = 2
)

type gpioV2LineAttribute struct {
	ID      uint32
	Padding uint32
	Value   uint64 // union: flags | values | debounce_period_us (u32)
}

type gpioV2LineConfigAttribute struct {
	Attr gpioV2LineAttribute
	Mask uint64
}

type gpioV2LineConfig struct {
	Flags    uint64
	NumAttrs uint32
	Padding  [5]uint32
	Attrs    [gpioV2LineNumAttrsMax]gpioV2LineConfigAttribute
}

type gpioV2LineRequest struct {
	Offsets         [gpioV2LinesMax]uint32
	Consumer        [gpioMaxNameSize]byte
	Config          gpioV2LineConfig
	NumLines        uint32
	EventBufferSize uint32
	Padding         [5]uint32
	Fd              int32
}

type gpioV2LineEvent struct {
	TimestampNs uint64
	ID          uint32
	Offset      uint32
	Seqno       uint32
	LineSeqno   uint32
	Padding     [6]uint32
}

// Expected C sizes (x86_64 and arm64 agree: all members naturally aligned).
const (
	sizeofLineRequest = 592
	sizeofLineEvent   = 48
)

// ioctl request number: _IOWR(0xB4, 0x07, struct gpio_v2_line_request).
const gpioV2GetLineIoctl = (3 << 30) | (sizeofLineRequest << 16) | (0xB4 << 8) | 0x07

// buildRequest fills the line request for one input line with edge
// detection on both edges (so we see press and release) and optional
// debounce. With ActiveLow the kernel inverts the logical value, so a
// "rising" logical edge is always "pressed".
func buildRequest(cfg GPIOConfig) gpioV2LineRequest {
	var r gpioV2LineRequest
	r.Offsets[0] = cfg.Line
	r.NumLines = 1
	r.EventBufferSize = 16
	name := cfg.Consumer
	if name == "" {
		name = "jan-sahayak"
	}
	copy(r.Consumer[:gpioMaxNameSize-1], name)
	r.Config.Flags = gpioV2LineFlagInput | gpioV2LineFlagEdgeRising | gpioV2LineFlagEdgeFalling
	if cfg.ActiveLow {
		r.Config.Flags |= gpioV2LineFlagActiveLow
	}
	if cfg.PullUp {
		r.Config.Flags |= gpioV2LineFlagBiasPullUp
	}
	if us := cfg.Debounce.Microseconds(); us > 0 {
		r.Config.NumAttrs = 1
		r.Config.Attrs[0] = gpioV2LineConfigAttribute{
			Attr: gpioV2LineAttribute{ID: gpioV2LineAttrIDDebounce, Value: uint64(us)},
			Mask: 1, // applies to line index 0
		}
	}
	return r
}
