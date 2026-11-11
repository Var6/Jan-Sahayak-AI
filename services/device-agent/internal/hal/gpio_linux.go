//go:build linux

package hal

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// GPIO reads a push button through /dev/gpiochipN.
type GPIO struct {
	cfg GPIOConfig
}

// NewGPIO checks that the chip exists; the line is requested in Run so a
// crashed agent never leaves it held.
func NewGPIO(cfg GPIOConfig) (*GPIO, error) {
	fi, err := os.Stat(cfg.Chip)
	if err != nil {
		return nil, fmt.Errorf("hal: %w", err)
	}
	if fi.Mode()&os.ModeCharDevice == 0 {
		return nil, fmt.Errorf("hal: %s is not a character device", cfg.Chip)
	}
	return &GPIO{cfg: cfg}, nil
}

// NewGPIOUnchecked returns a GPIO button without checking that the chip
// exists. Run keeps retrying to request the line (with backoff, reporting
// each failure to OnError), so a chip that appears late at boot or a
// missing button degrades the kiosk instead of failing agent startup.
func NewGPIOUnchecked(cfg GPIOConfig) *GPIO { return &GPIO{cfg: cfg} }

// Kind implements Button.
func (g *GPIO) Kind() string { return "gpio" }

// Simulated implements Button.
func (g *GPIO) Simulated() bool { return false }

// requestLine opens the chip and requests the line; it returns the line fd
// wrapped in a non-blocking *os.File so Close unblocks a pending Read.
func (g *GPIO) requestLine() (*os.File, error) {
	chip, err := os.OpenFile(g.cfg.Chip, os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("hal: open %s: %w", g.cfg.Chip, err)
	}
	defer chip.Close()
	req := buildRequest(g.cfg)
	if unsafe.Sizeof(req) != sizeofLineRequest {
		return nil, errors.New("hal: gpio_v2_line_request layout mismatch")
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, chip.Fd(), uintptr(gpioV2GetLineIoctl), uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		return nil, fmt.Errorf("hal: GPIO_V2_GET_LINE_IOCTL line %d: %w", g.cfg.Line, errno)
	}
	if req.Fd <= 0 {
		return nil, errors.New("hal: kernel returned no line fd")
	}
	fd := int(req.Fd)
	syscall.CloseOnExec(fd)
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "gpio-line"), nil
}

// Run implements Button. It re-requests the line after errors with a
// bounded backoff so a transient failure doesn't kill the button forever.
// The backoff only resets after the line has worked for a while, so a line
// fd that keeps returning EOF or errors immediately cannot become a busy
// loop.
func (g *GPIO) Run(ctx context.Context, press func()) error {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := g.runOnce(ctx, press)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			err = errors.New("hal: line event stream ended unexpectedly")
		}
		if g.cfg.OnError != nil {
			g.cfg.OnError(err)
		}
		if time.Since(started) >= stableAfter {
			backoff = time.Second
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
	return nil
}

// runOnce requests the line and delivers presses until an error or ctx end.
func (g *GPIO) runOnce(ctx context.Context, press func()) error {
	f, err := g.requestLine()
	if err != nil {
		return err
	}
	if g.cfg.OnReady != nil {
		g.cfg.OnReady()
	}
	done := make(chan struct{})
	defer close(done)
	go func() { // closes the line when ctx ends, unblocking Read
		select {
		case <-ctx.Done():
		case <-done:
		}
		f.Close()
	}()
	filt := pressFilter{minGap: g.cfg.MinGap}
	buf := make([]byte, sizeofLineEvent*16)
	for {
		n, err := f.Read(buf)
		if err != nil {
			if ctx.Err() != nil && errors.Is(err, os.ErrClosed) {
				return nil // closed by the ctx watcher above
			}
			if errors.Is(err, io.EOF) {
				return errors.New("hal: line event stream returned EOF")
			}
			return fmt.Errorf("hal: read line events: %w", err)
		}
		for off := 0; off+sizeofLineEvent <= n; off += sizeofLineEvent {
			id := binary.NativeEndian.Uint32(buf[off+8:])
			if id != gpioV2LineEventRisingEdge { // logical rising = pressed
				continue
			}
			if filt.accept(time.Now()) {
				press()
			}
		}
	}
}
