package docassist

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// OCR reads text from an image held in memory.
type OCR interface {
	// Recognize returns the text in img. It returns ErrBusy at once when
	// another image is being read (one at a time: a kiosk serves one
	// citizen), ErrTimeout when the engine took too long, ErrUnavailable
	// when it could not run.
	Recognize(ctx context.Context, img []byte) (string, error)
	// Health runs a version probe and a tiny OCR self-test.
	Health(ctx context.Context) error
}

// TesseractConfig configures the tesseract subprocess.
type TesseractConfig struct {
	// Path to the tesseract binary (a bare name is looked up in PATH).
	Path string
	// Languages for -l, e.g. "eng+hin". English first: measured on the
	// synthetic fixtures, "hin+eng" drops the digit 1 ("1234" → "234")
	// while "eng+hin" reads both scripts correctly.
	Languages string
	// TessdataDir sets TESSDATA_PREFIX ("" = tesseract's default).
	TessdataDir string
	// PSM is the page segmentation mode (3 = fully automatic).
	PSM int
	// Timeout bounds one recognition (the process is killed after it).
	Timeout time.Duration
	// MaxOutputBytes caps the text read from stdout; the rest is discarded.
	MaxOutputBytes int
	// MaxMemoryMB caps the process's address space (RLIMIT_AS) on Linux;
	// 0 = no cap. On other systems the cap is not applied.
	MaxMemoryMB int
}

// Tesseract runs `tesseract - - -l <langs> --psm <n>` per image: the image
// goes in on stdin, the text comes out on stdout, nothing touches the disk.
type Tesseract struct {
	cfg  TesseractConfig
	path string
	sem  chan struct{}

	mu       sync.Mutex
	langsOK  bool // the configured languages were found installed
	selfTest time.Duration
}

// NewTesseract resolves the binary and returns a runner. It does not start
// tesseract; Health does.
func NewTesseract(cfg TesseractConfig) (*Tesseract, error) {
	if cfg.Path == "" {
		cfg.Path = "tesseract"
	}
	if cfg.Languages == "" {
		cfg.Languages = "eng+hin"
	}
	if !reLangs.MatchString(cfg.Languages) {
		return nil, fmt.Errorf("docassist: bad tesseract languages %q", cfg.Languages)
	}
	if cfg.PSM <= 0 || cfg.PSM > 13 {
		cfg.PSM = 3
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = 64 << 10
	}
	path, err := exec.LookPath(cfg.Path)
	if err != nil {
		// Keep the name: Health reports the engine unavailable until it
		// is installed (the agent need not restart).
		path = cfg.Path
	}
	return &Tesseract{cfg: cfg, path: path, sem: make(chan struct{}, 1)}, nil
}

var reLangs = regexp.MustCompile(`^[a-z_]{3,16}(\+[a-z_]{3,16}){0,3}$`)

// Recognize implements OCR.
func (t *Tesseract) Recognize(ctx context.Context, img []byte) (string, error) {
	select {
	case t.sem <- struct{}{}:
	default:
		return "", ErrBusy
	}
	defer func() { <-t.sem }()
	return t.run(ctx, img)
}

func (t *Tesseract) env() []string {
	// A minimal environment: one OpenMP thread (bounded CPU and memory on
	// a Pi that also runs speech and the LLM), UTF-8 output.
	env := []string{"OMP_THREAD_LIMIT=1", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	if t.cfg.TessdataDir != "" {
		env = append(env, "TESSDATA_PREFIX="+t.cfg.TessdataDir)
	}
	return env
}

func (t *Tesseract) run(ctx context.Context, img []byte) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, t.cfg.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.path, "-", "-", "-l", t.cfg.Languages, "--psm", fmt.Sprint(t.cfg.PSM))
	cmd.Env = t.env()
	cmd.Dir = "/"
	setProcAttrs(cmd)
	stdout := &cappedBuffer{max: t.cfg.MaxOutputBytes}
	stderr := &cappedBuffer{max: 2 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 2 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	// Cap the process's memory before it receives the image (tesseract
	// only allocates image buffers after reading stdin).
	if t.cfg.MaxMemoryMB > 0 {
		if err := limitMemory(cmd.Process.Pid, uint64(t.cfg.MaxMemoryMB)<<20); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return "", fmt.Errorf("%w: memory limit: %v", ErrUnavailable, err)
		}
	}
	werr := make(chan error, 1)
	go func() {
		_, err := io.Copy(stdin, bytes.NewReader(img))
		cerr := stdin.Close()
		if err == nil {
			err = cerr
		}
		werr <- err
	}()
	err = cmd.Wait()
	<-werr // the writer ends once the process's stdin closes
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", ErrTimeout
		}
		return "", ctx.Err()
	}
	if err != nil {
		// stderr may echo nothing personal, but keep it out of errors
		// that reach logs anyway: report its size only.
		return "", fmt.Errorf("%w: tesseract: %v (stderr %d bytes)", ErrUnavailable, err, stderr.n)
	}
	return strings.ToValidUTF8(stdout.String(), ""), nil
}

// Health implements OCR: `tesseract --version`, the configured languages
// installed, and a self-test that reads a tiny embedded image. When an
// image is being read right now the self-test is skipped (the engine is
// evidently working) — the semaphore stays at one process.
func (t *Tesseract) Health(ctx context.Context) error {
	vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := t.short(vctx, "--version")
	if err != nil || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(out)), "tesseract ") {
		return fmt.Errorf("%w: version probe failed", ErrUnavailable)
	}
	t.mu.Lock()
	langsOK := t.langsOK
	t.mu.Unlock()
	if !langsOK {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, err := t.short(lctx, "--list-langs")
		if err != nil {
			return fmt.Errorf("%w: list-langs failed", ErrUnavailable)
		}
		have := map[string]bool{}
		for _, l := range strings.Fields(out) {
			have[l] = true
		}
		for _, l := range strings.Split(t.cfg.Languages, "+") {
			if !have[l] {
				return fmt.Errorf("%w: language %q not installed", ErrUnavailable, l)
			}
		}
		t.mu.Lock()
		t.langsOK = true
		t.mu.Unlock()
	}
	select {
	case t.sem <- struct{}{}:
	default:
		return nil
	}
	defer func() { <-t.sem }()
	start := time.Now()
	text, err := t.run(ctx, selfTestPNG)
	if err != nil {
		return err
	}
	if !strings.Contains(squash(text), squash(selfTestText)) {
		return fmt.Errorf("%w: self-test read the wrong text", ErrUnavailable)
	}
	t.mu.Lock()
	t.selfTest = time.Since(start)
	t.mu.Unlock()
	return nil
}

// SelfTestDuration is how long the last successful self-test took.
func (t *Tesseract) SelfTestDuration() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.selfTest
}

// short runs tesseract with one argument and returns stdout (capped).
func (t *Tesseract) short(ctx context.Context, arg string) (string, error) {
	cmd := exec.CommandContext(ctx, t.path, arg)
	cmd.Env = t.env()
	cmd.Dir = "/"
	setProcAttrs(cmd)
	out := &cappedBuffer{max: 16 << 10}
	cmd.Stdout = out
	cmd.Stderr = out // older versions print --version to stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

// selfTestPNG is a 360-px image of selfTestText rendered by
// testdata/render.swift.txt (synthetic, no personal data).
//
//go:embed selftest.png
var selfTestPNG []byte

const selfTestText = "OCR TEST 2580"

// cappedBuffer keeps the first max bytes written and discards the rest
// (the process is never blocked on a full pipe).
type cappedBuffer struct {
	buf bytes.Buffer
	max int
	n   int // total bytes written
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.n += len(p)
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }

// ----------------------------------------------------------------- image checks

// ImageLimits bound an uploaded photo.
type ImageLimits struct {
	MaxBytes int64 // default 2 MiB
	MaxSide  int   // longest side in pixels (default 1600)
	MinSide  int   // shortest side in pixels (default 200)
}

// CheckJPEG verifies that b is a baseline/progressive JPEG within limits,
// reading only its header (no full decode).
func CheckJPEG(b []byte, lim ImageLimits) error {
	if lim.MaxBytes <= 0 {
		lim.MaxBytes = 2 << 20
	}
	if lim.MaxSide <= 0 {
		lim.MaxSide = 1600
	}
	if lim.MinSide <= 0 {
		lim.MinSide = 200
	}
	if int64(len(b)) > lim.MaxBytes {
		return ErrTooLarge
	}
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 || b[2] != 0xFF {
		return ErrBadImage
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return ErrBadImage
	}
	long, short := max(cfg.Width, cfg.Height), min(cfg.Width, cfg.Height)
	if long > lim.MaxSide {
		return ErrTooLarge
	}
	if short < lim.MinSide {
		return ErrBadImage
	}
	return nil
}
