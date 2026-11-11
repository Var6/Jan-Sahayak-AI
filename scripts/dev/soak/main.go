// Command soak drives many text-only kiosk sessions against a running
// device agent while sampling its RSS and goroutine count, to spot leaks.
//
//	go run ./scripts/dev/soak -duration 10m -sessions 100 -pid $(cat var/run/agent.pid)
//
// Each session: start (simulated button) → speaking-done → 3 text turns
// (loan, pension, an off-topic question) → end. Sessions are paced evenly
// over -duration; no new session starts after it. Stdlib only.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var texts = []string{"मुझे लोन चाहिए", "बुढ़ापा पेंशन के बारे में बताओ", "cricket score batao"}

type client struct {
	base string
	http *http.Client
}

func (c *client) post(path string, body any) (map[string]any, int, time.Duration, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(http.MethodPost, c.base+path, rd)
	req.Header.Set("X-JS-Kiosk", "1")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	el := time.Since(start)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m, resp.StatusCode, el, nil
}

func (c *client) get(path string) ([]byte, int, error) {
	resp, err := c.http.Get(c.base + path)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return b, resp.StatusCode, err
}

func activity(m map[string]any) string {
	s, _ := m["session"].(map[string]any)
	a, _ := s["activity"].(string)
	return a
}

func rssMB(pid int) float64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return -1
	}
	kb, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return kb / 1024
}

func goroutines(c *client) int {
	b, _, err := c.get("/api/v1/health")
	if err != nil {
		return -1
	}
	var h struct {
		Components []struct {
			Name string         `json:"name"`
			Data map[string]any `json:"data"`
		} `json:"components"`
	}
	if json.Unmarshal(b, &h) != nil {
		return -1
	}
	for _, comp := range h.Components {
		if comp.Name == "goroutines" {
			if v, ok := comp.Data["goroutines"].(float64); ok {
				return int(v)
			}
		}
	}
	return -1
}

type sample struct {
	at   time.Duration
	rss  float64
	gor  int
	sess int
}

func main() {
	base := flag.String("url", "http://127.0.0.1:8730", "agent base URL")
	dur := flag.Duration("duration", 10*time.Minute, "soak duration")
	n := flag.Int("sessions", 100, "sessions to run")
	every := flag.Duration("sample", 10*time.Second, "sampling interval")
	pid := flag.Int("pid", 0, "agent pid (for RSS)")
	flag.Parse()
	c := &client{base: *base, http: &http.Client{Timeout: 70 * time.Second}}
	ctx, cancel := context.WithTimeout(context.Background(), *dur+2*time.Minute)
	defer cancel()

	var mu sync.Mutex
	var samples []sample
	done := 0
	start := time.Now()
	stopSampling := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(*every)
		defer t.Stop()
		for {
			mu.Lock()
			s := sample{at: time.Since(start).Round(time.Second), rss: rssMB(*pid), gor: goroutines(c), sess: done}
			samples = append(samples, s)
			mu.Unlock()
			fmt.Printf("sample t=%-6s rss=%.1fMB goroutines=%d sessions=%d\n", s.at, s.rss, s.gor, s.sess)
			select {
			case <-stopSampling:
				return
			case <-t.C:
			}
		}
	}()

	var lat []time.Duration
	errs := map[string]int{}
	pace := *dur / time.Duration(*n)
	for i := 0; i < *n && time.Since(start) < *dur && ctx.Err() == nil; i++ {
		next := start.Add(time.Duration(i+1) * pace)
		m, code, _, err := c.post("/api/v1/session/start", map[string]string{"language": "hi"})
		if err != nil || code != 200 {
			errs[fmt.Sprintf("start %d %v", code, err)]++
			time.Sleep(time.Second)
			continue
		}
		s, _ := m["session"].(map[string]any)
		sid, _ := s["id"].(string)
		if activity(m) == "SPEAKING" {
			_, _, _, _ = c.post("/api/v1/session/"+sid+"/speaking-done", nil)
		}
		for _, t := range texts {
			r, code, el, err := c.post("/api/v1/session/"+sid+"/text", map[string]string{"text": t})
			if err != nil || code != 200 {
				errs[fmt.Sprintf("text %d %v", code, err)]++
				continue
			}
			lat = append(lat, el)
			if au, _ := r["audio_url"].(string); au != "" {
				if _, code, err := c.get(au); err != nil || code != 200 {
					errs[fmt.Sprintf("clip %d %v", code, err)]++
				}
			}
			if activity(r) == "SPEAKING" {
				_, _, _, _ = c.post("/api/v1/session/"+sid+"/speaking-done", nil)
			}
		}
		if _, code, _, err := c.post("/api/v1/session/"+sid+"/end", nil); err != nil || code != 200 {
			errs[fmt.Sprintf("end %d %v", code, err)]++
		}
		mu.Lock()
		done++
		mu.Unlock()
		if d := time.Until(next); d > 0 {
			time.Sleep(d)
		}
	}
	close(stopSampling)
	wg.Wait()
	mu.Lock()
	samples = append(samples, sample{at: time.Since(start).Round(time.Second), rss: rssMB(*pid), gor: goroutines(c), sess: done})
	mu.Unlock()

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		return lat[int(p*float64(len(lat)-1))]
	}
	first, last := samples[0], samples[len(samples)-1]
	maxRSS, maxG := 0.0, 0
	for _, s := range samples {
		if s.rss > maxRSS {
			maxRSS = s.rss
		}
		if s.gor > maxG {
			maxG = s.gor
		}
	}
	fmt.Printf("\nSUMMARY sessions=%d turns=%d elapsed=%s errors=%v\n", done, len(lat), time.Since(start).Round(time.Second), errs)
	fmt.Printf("turn latency p50=%s p95=%s max=%s\n", pct(0.5).Round(time.Millisecond), pct(0.95).Round(time.Millisecond), pct(1).Round(time.Millisecond))
	fmt.Printf("rss first=%.1fMB last=%.1fMB max=%.1fMB; goroutines first=%d last=%d max=%d\n", first.rss, last.rss, maxRSS, first.gor, last.gor, maxG)
	if len(errs) > 0 {
		os.Exit(1)
	}
}
