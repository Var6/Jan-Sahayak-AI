// Package hal is the hardware abstraction for the kiosk's physical inputs.
//
// Today it covers the session button:
//
//   - Sim: no hardware. Sessions start from the kiosk UI's on-screen button
//     (POST /api/v1/session/start), which the local API allows only when the
//     button is simulated. Used on the developer Mac.
//   - GPIO (Linux only): a momentary push button on a GPIO line, read through
//     the gpiochip character device (GPIO uAPI v2) with kernel edge
//     detection and debounce. No sysfs, no polling, no cgo.
//
// Audio in/out is handled by Chromium (microphone via getUserMedia while the
// agent says LISTENING; playback of reply WAVs), so the agent itself never
// opens an audio device. Camera support is not built yet.
package hal

import (
	"context"
	"errors"
	"time"
)

// Button delivers presses of the session button.
type Button interface {
	// Kind is "sim" or "gpio".
	Kind() string
	// Simulated reports whether presses come from the UI (sim) rather than
	// hardware. The local API enables POST /session/start only when true.
	Simulated() bool
	// Run blocks until ctx is done, calling press for each debounced press.
	// press must not block for long; it is called from Run's goroutine
	// (the agent hands each press to its own single-flight goroutine).
	Run(ctx context.Context, press func()) error
}

// Sim is the simulated button. Run only waits for ctx: presses arrive via
// the local API instead.
type Sim struct{}

// Kind implements Button.
func (Sim) Kind() string { return "sim" }

// Simulated implements Button.
func (Sim) Simulated() bool { return true }

// Run implements Button.
func (Sim) Run(ctx context.Context, _ func()) error {
	<-ctx.Done()
	return nil
}

// GPIOConfig configures a GPIO button.
type GPIOConfig struct {
	Chip      string        // e.g. /dev/gpiochip0
	Line      uint32        // line offset on the chip
	ActiveLow bool          // pressed = line low (button to ground)
	PullUp    bool          // enable the internal pull-up
	Debounce  time.Duration // kernel debounce period (0 = none)
	MinGap    time.Duration // presses closer together than this are ignored
	Consumer  string        // label visible in gpioinfo
	OnError   func(error)   // optional: told about each failed attempt (Run retries)
	OnReady   func()        // optional: told each time the line is held and being read
}

// GPIO retry backoff: doubles from 1 s up to maxBackoff, and resets only
// after the line has been read successfully for stableAfter.
const (
	maxBackoff  = 30 * time.Second
	stableAfter = time.Minute
)

// ErrUnsupported is returned by NewGPIO on platforms without the gpiochip
// character device.
var ErrUnsupported = errors.New("hal: GPIO button is only supported on Linux")

// pressFilter turns raw "pressed" edges into presses, dropping edges that
// arrive within minGap of the previous accepted press (contact bounce that
// slips past the kernel debounce, or a citizen hammering the button).
type pressFilter struct {
	minGap time.Duration
	last   time.Time
	have   bool
}

func (f *pressFilter) accept(at time.Time) bool {
	if f.have && at.Sub(f.last) < f.minGap {
		return false
	}
	f.last, f.have = at, true
	return true
}
