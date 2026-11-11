//go:build !linux

package hal

import "context"

// GPIO is unavailable off Linux.
type GPIO struct{ cfg GPIOConfig }

// NewGPIO always fails off Linux.
func NewGPIO(GPIOConfig) (*GPIO, error) { return nil, ErrUnsupported }

// NewGPIOUnchecked returns a button whose Run reports ErrUnsupported once
// through OnError and then waits for ctx (no GPIO off Linux).
func NewGPIOUnchecked(cfg GPIOConfig) *GPIO { return &GPIO{cfg: cfg} }

// Kind implements Button.
func (*GPIO) Kind() string { return "gpio" }

// Simulated implements Button.
func (*GPIO) Simulated() bool { return false }

// Run implements Button.
func (g *GPIO) Run(ctx context.Context, _ func()) error {
	if g != nil && g.cfg.OnError != nil {
		g.cfg.OnError(ErrUnsupported)
	}
	<-ctx.Done()
	return nil
}
