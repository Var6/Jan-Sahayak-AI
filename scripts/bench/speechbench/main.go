// Command speechbench starts the speech worker, measures STT and TTS
// latency / real-time factor, the worker's resident memory, and a
// TTS → STT round trip, then stops the worker. Stdlib only.
//
//	go run ./scripts/bench/speechbench -worker services/speech-worker/bin/speech-worker
//
// It is a development tool; nothing here runs on the kiosk.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

// References for the synthetic clips in var/testaudio (generated with the
// macOS "Lekha" voice, 16 kHz mono); these are the texts that were spoken.
var clips = []struct{ file, ref string }{
	{"t1.wav", "मुझे लोन चाहिए।"},
	{"t2.wav", "मुझे जाति प्रमाण पत्र बनवाना है।"},
	{"t3.wav", "मेरी माँ की उम्र सत्तर साल है, उन्हें वृद्धावस्था पेंशन कैसे मिलेगी?"},
	{"t4.wav", "मैं गया जिले का किसान हूँ, पीएम किसान का पैसा नहीं आया।"},
}

// Neutral kiosk phrases (no government facts).
var sentences = []string{
	"नमस्ते, जन सहायक में आपका स्वागत है।",
	"कृपया बटन दबाकर अपना सवाल पूछिए।",
	"आपकी बात समझने में थोड़ी दिक्कत हुई, कृपया एक बार फिर से धीरे बोलिए।",
}

type rssSampler struct {
	pid  int
	mu   sync.Mutex
	peak int64 // KiB
	last int64
	stop chan struct{}
	done chan struct{}
}

func rssKiB(pid int) (int64, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

func startSampler(pid int, every time.Duration) *rssSampler {
	s := &rssSampler{pid: pid, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			if v, err := rssKiB(pid); err == nil {
				s.mu.Lock()
				s.last = v
				s.peak = max(s.peak, v)
				s.mu.Unlock()
			}
			select {
			case <-s.stop:
				return
			case <-t.C:
			}
		}
	}()
	return s
}

func (s *rssSampler) read() (last, peak int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last, s.peak
}

func (s *rssSampler) close() { close(s.stop); <-s.done }

func waitHealthy(ctx context.Context, base string) (map[string]any, error) {
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var h map[string]any
				_ = json.Unmarshal(b, &h)
				return h, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

type sttResp struct {
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
	Source     string  `json:"confidence_source"`
	NoSpeech   bool    `json:"no_speech"`
	AudioSecs  float64 `json:"audio_secs"`
	SpeechSecs float64 `json:"speech_secs"`
	LatencyMS  int64   `json:"latency_ms"`
}

func transcribe(base string, wav []byte) (sttResp, time.Duration, error) {
	start := time.Now()
	resp, err := http.Post(base+"/v1/transcribe?lang=hi", "audio/wav", bytes.NewReader(wav))
	if err != nil {
		return sttResp{}, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	el := time.Since(start)
	if resp.StatusCode != 200 {
		return sttResp{}, el, fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	var r sttResp
	return r, el, json.Unmarshal(b, &r)
}

func synthesize(base, text string) ([]byte, float64, time.Duration, error) {
	body, _ := json.Marshal(map[string]any{"text": text, "lang": "hi"})
	start := time.Now()
	resp, err := http.Post(base+"/v1/synthesize", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	el := time.Since(start)
	if resp.StatusCode != 200 {
		return nil, 0, el, fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	secs, _ := strconv.ParseFloat(resp.Header.Get("X-Audio-Secs"), 64)
	return b, secs, el, nil
}

// --- audio helpers -----------------------------------------------------

func decodeWAV(b []byte) ([]float32, int, error) {
	if len(b) < 44 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, 0, errors.New("not wav")
	}
	off, rate := 12, 0
	for off+8 <= len(b) {
		id := string(b[off : off+4])
		size := int(binary.LittleEndian.Uint32(b[off+4:]))
		body := off + 8
		if body+size > len(b) {
			size = len(b) - body
		}
		switch id {
		case "fmt ":
			rate = int(binary.LittleEndian.Uint32(b[body+4:]))
		case "data":
			out := make([]float32, size/2)
			for i := range out {
				out[i] = float32(int16(binary.LittleEndian.Uint16(b[body+2*i:]))) / 32768
			}
			return out, rate, nil
		}
		off = body + size + size%2
	}
	return nil, 0, errors.New("no data chunk")
}

func encodeWAV16k(s []float32) []byte {
	n := len(s)
	b := make([]byte, 44+2*n)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+2*n))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 16000)
	binary.LittleEndian.PutUint32(b[28:], 32000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(2*n))
	for i, v := range s {
		x := math.Max(-1, math.Min(1, float64(v)))
		binary.LittleEndian.PutUint16(b[44+2*i:], uint16(int16(x*32767)))
	}
	return b
}

// resample with a Hann-windowed sinc low-pass (cut-off 0.45·outRate).
func resample(in []float32, inRate, outRate int) []float32 {
	const W = 32
	ratio := float64(inRate) / float64(outRate)
	fc := 0.45 * float64(outRate) / float64(inRate) // cycles per input sample
	n := int(float64(len(in)) / ratio)
	out := make([]float32, n)
	for i := range out {
		c := float64(i) * ratio
		k0 := int(math.Floor(c))
		var acc float64
		for k := k0 - W; k <= k0+W; k++ {
			if k < 0 || k >= len(in) {
				continue
			}
			x := float64(k) - c
			h := 2 * fc
			if x != 0 {
				h = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
			}
			h *= 0.5 * (1 + math.Cos(math.Pi*x/(W+1)))
			acc += h * float64(in[k])
		}
		out[i] = float32(acc)
	}
	return out
}

// --- text similarity ----------------------------------------------------

func normText(s string) []rune {
	var out []rune
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r) || unicode.IsDigit(r) {
			out = append(out, r)
		}
	}
	return out
}

func words(s string) []string {
	f := strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r) || r == '।'
	})
	return f
}

func lev[T comparable](a, b []T) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func cer(ref, hyp string) float64 {
	r := normText(ref)
	if len(r) == 0 {
		return 0
	}
	return float64(lev(r, normText(hyp))) / float64(len(r))
}

func wer(ref, hyp string) float64 {
	r := words(ref)
	if len(r) == 0 {
		return 0
	}
	return float64(lev(r, words(hyp))) / float64(len(r))
}

func median(d []time.Duration) time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

func main() {
	worker := flag.String("worker", "services/speech-worker/bin/speech-worker", "speech-worker binary")
	models := flag.String("models-dir", "models/weights", "models root")
	audioDir := flag.String("audio-dir", "var/testaudio", "test clips")
	addr := flag.String("addr", "127.0.0.1:8743", "worker address")
	threads := flag.Int("threads", 4, "worker threads")
	runs := flag.Int("runs", 3, "repetitions per item (median reported)")
	flag.Parse()

	if err := run(*worker, *models, *audioDir, *addr, *threads, *runs); err != nil {
		fmt.Fprintln(os.Stderr, "speechbench:", err)
		os.Exit(1)
	}
}

func run(worker, models, audioDir, addr string, threads, runs int) error {
	base := "http://" + addr
	if resp, err := http.Get(base + "/health"); err == nil {
		resp.Body.Close()
		return fmt.Errorf("something is already listening on %s", addr)
	}
	logf, err := os.CreateTemp("", "speech-worker-*.log")
	if err != nil {
		return err
	}
	defer os.Remove(logf.Name())
	defer logf.Close()
	cmd := exec.Command(worker, "-addr", addr, "-models-dir", models, "-threads", strconv.Itoa(threads))
	cmd.Stdout, cmd.Stderr = logf, logf
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	// Always stop the worker, gracefully first.
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-exited:
			fmt.Printf("\nworker stopped (SIGTERM): %v\n", errOrOK(err))
		case <-time.After(20 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
			fmt.Println("\nworker killed (SIGTERM timeout)")
		}
	}()
	// Ctrl-C / SIGTERM: never leave the worker running.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		if _, ok := <-sigs; ok {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			time.Sleep(3 * time.Second)
			_ = cmd.Process.Kill()
			os.Exit(130)
		}
	}()
	rss := startSampler(cmd.Process.Pid, 100*time.Millisecond)
	defer rss.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	hctx, hcancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-exited:
			hcancel()
		case <-hctx.Done():
		}
	}()
	health, err := waitHealthy(hctx, base)
	if err != nil {
		b, _ := os.ReadFile(logf.Name())
		return fmt.Errorf("worker did not become healthy: %v\n%s", err, b)
	}
	boot := time.Since(t0)
	time.Sleep(300 * time.Millisecond)
	idle, _ := rss.read()
	hj, _ := json.Marshal(health["models"])
	fmt.Printf("speech worker ready in %.2f s (threads=%d)\nmodels: %s\nRSS after load+warm-up: %.0f MiB\n\n",
		boot.Seconds(), threads, hj, float64(idle)/1024)

	fmt.Println("== STT (IndicConformer-hi CTC + Silero VAD) ==")
	fmt.Printf("%-7s %6s %9s %9s %6s %5s %5s %5s  %s\n", "clip", "audio", "lat_med", "worker", "RTF", "conf", "CER", "WER", "transcript")
	for _, c := range clips {
		wav, err := os.ReadFile(filepath.Join(audioDir, c.file))
		if err != nil {
			return err
		}
		var lats []time.Duration
		var last sttResp
		for range runs {
			r, el, err := transcribe(base, wav)
			if err != nil {
				return fmt.Errorf("%s: %w", c.file, err)
			}
			lats = append(lats, el)
			last = r
		}
		m := median(lats)
		fmt.Printf("%-7s %5.2fs %7.0fms %7dms %6.3f %5.2f %5.2f %5.2f  %s\n", c.file, last.AudioSecs, float64(m.Microseconds())/1000,
			last.LatencyMS, m.Seconds()/last.AudioSecs, last.Confidence, cer(c.ref, last.Text), wer(c.ref, last.Text), last.Text)
	}
	// silence
	r, el, err := transcribe(base, encodeWAV16k(make([]float32, 32000)))
	if err != nil {
		return err
	}
	fmt.Printf("silence 2.00s: no_speech=%v text=%q latency=%dms\n\n", r.NoSpeech, r.Text, el.Milliseconds())

	fmt.Println("== TTS (Supertonic-3, lang=hi) + round trip TTS→STT ==")
	fmt.Printf("%-3s %6s %9s %6s %6s  %-6s %s\n", "#", "audio", "lat_med", "RTF", "CER", "sim", "STT of TTS output")
	for i, s := range sentences {
		var lats []time.Duration
		var wav []byte
		var secs float64
		for range runs {
			w, sec, el, err := synthesize(base, s)
			if err != nil {
				return fmt.Errorf("tts %d: %w", i, err)
			}
			lats = append(lats, el)
			wav, secs = w, sec
		}
		m := median(lats)
		pcm, rate, err := decodeWAV(wav)
		if err != nil {
			return err
		}
		pcm16 := resample(pcm, rate, 16000)
		if len(pcm16) > 20*16000 {
			pcm16 = pcm16[:20*16000]
		}
		tr, _, err := transcribe(base, encodeWAV16k(pcm16))
		if err != nil {
			return err
		}
		c := cer(s, tr.Text)
		fmt.Printf("%-3d %5.2fs %7.0fms %6.3f %6.2f  %-6.2f %s\n", i+1, secs, float64(m.Microseconds())/1000, m.Seconds()/secs, c, 1-c, tr.Text)
	}
	_, peak := rss.read()
	fmt.Printf("\nworker RSS: after load %.0f MiB, peak %.0f MiB (ps rss, sampled every 100 ms)\n", float64(idle)/1024, float64(peak)/1024)
	return nil
}

func errOrOK(err error) string {
	if err == nil {
		return "exit 0"
	}
	return err.Error()
}
