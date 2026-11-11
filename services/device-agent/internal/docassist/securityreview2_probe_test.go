//go:build securityreview2

// Independent security-review probes (build tag securityreview2). All text
// is synthetic and FAKE; numbers are made up.
//
//	go test -tags securityreview2 -race -count=1 -run Probe ./services/device-agent/internal/docassist/
package docassist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// visible returns every string the UI can receive from a Result.
func visible(r Result) string {
	b, _ := json.Marshal(r)
	return string(b)
}

// digitsOnly maps Devanagari digits to ASCII and drops everything else.
func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= '०' && r <= '९':
			b.WriteRune('0' + (r - '०'))
		}
	}
	return b.String()
}

// Probe: identifiers inside a free-text field (issuing authority) must be
// masked whatever separators OCR put between the digit groups.
func TestProbeAuthorityFieldMasking(t *testing.T) {
	const head = "नमूना — नकली दस्तावेज़ / FAKE SPECIMEN\nआय प्रमाण पत्र\nनाम: टेस्ट नाम\n"
	cases := []struct {
		name, line, secret string
	}{
		{"aadhaar single spaces (baseline)", "जारी करने वाला कार्यालय: अंचल कार्यालय 1234 5678 9012", "123456789012"},
		{"aadhaar double spaces", "जारी करने वाला कार्यालय: अंचल कार्यालय 1234  5678  9012", "123456789012"},
		{"aadhaar dots", "जारी करने वाला कार्यालय: अंचल कार्यालय 1234.5678.9012", "123456789012"},
		{"aadhaar slashes", "Issued by: Circle Office 1234/5678/9012", "123456789012"},
		{"aadhaar en-dash", "Issued by: Circle Office 1234–5678–9012", "123456789012"},
		{"aadhaar devanagari digits", "जारी करने वाला कार्यालय: अंचल कार्यालय १२३४ ५६७८ ९०१२", "123456789012"},
		{"aadhaar hint line, double spaces", "अंचल कार्यालय टेस्टपुर 1234  5678  9012", "123456789012"},
		{"phone +91", "Issued by: Circle Office +91 98765 43210", "9876543210"},
		{"account 3 groups of 4 with double spaces", "Issued by: Branch Office 4321  8765  2109  6543", "4321876521096543"},
		{"PAN lowercase", "Issued by: Circle Office abcde1234f", "1234"},
		{"PAN OCR l-for-1", "Issued by: Circle Office ABCDEl234F", "ABCDEl234F"},
	}
	var leaks []string
	for _, tc := range cases {
		res := Analyze(head + tc.line + "\n")
		v := visible(res)
		leaked := false
		if regexp.MustCompile(`^[0-9]+$`).MatchString(tc.secret) {
			// Leak = more than the last four digits of the secret visible.
			d := digitsOnly(res.Authority)
			if len(tc.secret) > 4 && strings.Contains(d, tc.secret[:len(tc.secret)-4]) {
				leaked = true
			}
		} else if strings.Contains(v, tc.secret) {
			leaked = true
		}
		t.Logf("%-40s type=%s authority=%q leaked=%v", tc.name, res.Type, res.Authority, leaked)
		if leaked {
			leaks = append(leaks, tc.name)
		}
	}
	if len(leaks) > 0 {
		t.Errorf("identifier digits visible in issuing_authority for: %s", strings.Join(leaks, "; "))
	}
}

// Probe: the unlabelled authority fallback picks any line naming an
// "office"/"कार्यालय", which on real cards is often the citizen's address.
func TestProbeAuthorityFallbackPicksAddressLine(t *testing.T) {
	text := "FAKE SPECIMEN\nभारत सरकार\nमेरा आधार, मेरी पहचान\nपता:\nग्राम टेस्टपुर, प्रखंड कार्यालय के पास, पिन 854301\nXXXX XXXX 9012\n"
	res := Analyze(text)
	t.Logf("type=%s authority=%q masked=%v", res.Type, res.Authority, res.MaskedIDs)
	if strings.Contains(res.Authority, "ग्राम टेस्टपुर") {
		t.Errorf("citizen address line reported as issuing authority: %q", res.Authority)
	}
}

// Probe: maskedIDs never emit more than the last four digits.
func TestProbeMaskedIDsNeverExceedLastFour(t *testing.T) {
	texts := []string{
		"मेरा आधार, मेरी पहचान\n1234 5678 9012\n",
		"मेरा आधार, मेरी पहचान\n१२३४ ५६७८ ९०१२\n",
		"मेरा आधार, मेरी पहचान\nVID: 9123 4567 8901 2345\n",
		"PASSBOOK IFSC TEST0001234\nAccount No: 1234-5678-9012-345\n",
		"INCOME TAX DEPARTMENT\nABCDE1234F\n",
	}
	for _, tx := range texts {
		r := Analyze(tx)
		for _, m := range r.MaskedIDs {
			if n := len(digitsOnly(m.Value)); n > 4 {
				t.Errorf("%q: masked id %q shows %d digits", tx, m.Value, n)
			}
		}
		if strings.Contains(visible(r), "5678") || strings.Contains(visible(r), "ABCDE") {
			t.Errorf("%q: raw identifier part visible: %s", tx, visible(r))
		}
	}
}

// Probe: the JSON form of a Result never carries the raw OCR text.
func TestProbeResultCarriesNoRawText(t *testing.T) {
	marker := "SECRETMARKERLINE"
	r := Analyze("INCOME TAX DEPARTMENT\nABCDE1234F\nName: TEST NAME\n" + marker + " पति रोज़ मारता है\n")
	if strings.Contains(visible(r), marker) || strings.Contains(visible(r), "मारता") {
		t.Fatalf("raw OCR line leaked: %s", visible(r))
	}
}

// ---------------------------------------------------------------- subprocess

func fakeTesseract(t *testing.T, script string) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	bin = filepath.Join(dir, "tesseract")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// Probe: language argument cannot inject tesseract options.
func TestProbeLanguageArgInjection(t *testing.T) {
	for _, l := range []string{"eng -c tessedit_write_images=1", "eng+hin --tessdata-dir /tmp", "../../x", "eng;id", "-psm"} {
		if _, err := NewTesseract(TesseractConfig{Path: "/bin/true", Languages: l}); err == nil {
			t.Errorf("languages %q accepted", l)
		}
	}
}

// Probe: argv is fixed, the image goes on stdin only, and the agent's
// environment is not inherited.
func TestProbeArgvAndEnv(t *testing.T) {
	t.Setenv("JS_PROBE_SECRET", "agent-secret")
	bin, dir := fakeTesseract(t, `printf '%s\n' "$@" > "$TESSDATA_PREFIX/argv"; env > "$TESSDATA_PREFIX/env"; cat > /dev/null; echo ok`)
	tess, err := NewTesseract(TesseractConfig{Path: bin, Languages: "eng+hin", TessdataDir: dir, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tess.Recognize(context.Background(), bytes.Repeat([]byte{0xff}, 4096)); err != nil {
		t.Fatal(err)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	if got := strings.Fields(string(argv)); strings.Join(got, " ") != "- - -l eng+hin --psm 3" {
		t.Errorf("argv = %q", got)
	}
	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	if strings.Contains(string(env), "agent-secret") {
		t.Error("agent environment inherited by tesseract")
	}
	t.Logf("env seen by tesseract:\n%s", env)
}

// Probe: on timeout the whole process group dies (no orphan holding the
// image), and Recognize returns within Timeout + WaitDelay.
func TestProbeTimeoutKillsGroup(t *testing.T) {
	bin, dir := fakeTesseract(t, `sleep 60 & echo $! > "$TESSDATA_PREFIX/child"; sleep 60`)
	// 2 s (was 0.7 s): under -race with parallel packages on a swapping
	// machine, sh sometimes had not yet written the child pid at 0.7 s.
	const timeout = 2 * time.Second
	tess, _ := NewTesseract(TesseractConfig{Path: bin, Languages: "eng", TessdataDir: dir, Timeout: timeout})
	start := time.Now()
	_, err := tess.Recognize(context.Background(), []byte("x"))
	el := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
	if el > timeout+3*time.Second {
		t.Errorf("returned after %v", el)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "child"))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 {
		t.Fatal("no child pid")
	}
	deadline := time.Now().Add(2 * time.Second)
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if alive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("grandchild %d survived the timeout", pid)
	}
	t.Logf("timeout returned in %v", el)
}

// Probe: a process that never reads stdin (pipe full) is still bounded.
func TestProbeStdinNeverRead(t *testing.T) {
	bin, dir := fakeTesseract(t, `sleep 60`)
	tess, _ := NewTesseract(TesseractConfig{Path: bin, Languages: "eng", TessdataDir: dir, Timeout: 500 * time.Millisecond})
	start := time.Now()
	_, err := tess.Recognize(context.Background(), make([]byte, 2<<20))
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
	if el := time.Since(start); el > 4*time.Second {
		t.Errorf("returned after %v", el)
	}
}

// Probe: stdout flood is capped.
func TestProbeStdoutCapped(t *testing.T) {
	bin, dir := fakeTesseract(t, `cat >/dev/null; head -c 5000000 /dev/zero | tr '\000' 'A'`)
	tess, _ := NewTesseract(TesseractConfig{Path: bin, Languages: "eng", TessdataDir: dir, Timeout: 10 * time.Second, MaxOutputBytes: 64 << 10})
	out, err := tess.Recognize(context.Background(), []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 64<<10 {
		t.Errorf("output %d bytes", len(out))
	}
}

// Probe: a second concurrent image is refused at once (one process max).
func TestProbeOneProcessAtATime(t *testing.T) {
	bin, dir := fakeTesseract(t, `cat >/dev/null; sleep 1; echo ok`)
	tess, _ := NewTesseract(TesseractConfig{Path: bin, Languages: "eng", TessdataDir: dir, Timeout: 5 * time.Second})
	done := make(chan error, 1)
	go func() { _, err := tess.Recognize(context.Background(), []byte("x")); done <- err }()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	if _, err := tess.Recognize(context.Background(), []byte("y")); !errors.Is(err, ErrBusy) {
		t.Errorf("second call err = %v", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Error("ErrBusy was not immediate")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Probe (real tesseract, skipped when absent): no new temp files appear in
// the usual temp directories while a fixture photo is read.
func TestProbeRealTesseractNoTempFiles(t *testing.T) {
	bin, err := exec.LookPath("tesseract")
	if err != nil {
		t.Skip("tesseract not installed")
	}
	img, err := os.ReadFile("testdata/fake_income.jpg")
	if err != nil {
		t.Fatal(err)
	}
	dirs := []string{"/tmp", "/private/tmp", "/var/tmp", os.TempDir()}
	snap := func() map[string]bool {
		m := map[string]bool{}
		for _, d := range dirs {
			es, _ := os.ReadDir(d)
			for _, e := range es {
				m[filepath.Join(d, e.Name())] = true
			}
		}
		return m
	}
	before := snap()
	tess, _ := NewTesseract(TesseractConfig{Path: bin, Languages: "eng+hin", Timeout: 20 * time.Second})
	text, err := tess.Recognize(context.Background(), img)
	if err != nil {
		t.Fatal(err)
	}
	after := snap()
	var added []string
	for p := range after {
		if !before[p] && regexp.MustCompile(`(?i)tess|lept|ocr|\.(jpg|png|tif|pnm)$`).MatchString(p) {
			added = append(added, p)
		}
	}
	t.Logf("read %d chars; candidate new temp entries: %v", len(text), added)
	if len(added) > 0 {
		t.Errorf("tesseract-looking temp files appeared: %v", added)
	}
}
