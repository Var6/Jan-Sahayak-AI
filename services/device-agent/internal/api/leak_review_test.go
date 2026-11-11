package api

// Reliability review (goroutine leaks / bounded SSE): SSE streams over a
// real HTTP server must end on Server.Close (shutdown), client disconnects
// must free their slots, and nothing may outlive the server.

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func openSSE(t *testing.T, ctx context.Context, url string) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/v1/events", nil)
	req.Host = "127.0.0.1:8730"
	return http.DefaultClient.Do(req)
}

func openKioskSSE(t *testing.T, ctx context.Context, url string) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/v1/events", nil)
	req.Host = "127.0.0.1:8730"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	return http.DefaultClient.Do(req)
}

func waitLine(t *testing.T, r *bufio.Reader, prefix string) {
	t.Helper()
	for {
		l, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended before %q: %v", prefix, err)
		}
		if strings.HasPrefix(l, prefix) {
			return
		}
	}
}

func TestReliabilitySSESlotsAndShutdownNoLeak(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) }) // runs last (after store close)
	h := newHarness(t, hopts{})
	ts := httptest.NewServer(h.h)
	defer ts.Close()
	max := h.srv.d.Limits.SSEMaxClients

	// Fill every slot, then drop the clients: the slots must come back.
	// (Security review #16: one slot is reserved for the kiosk page, so the
	// last stream is a same-origin browser stream; updated from "max plain
	// streams" when the reservation was added.)
	var cancels []context.CancelFunc
	for i := 0; i < max; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		var resp *http.Response
		var err error
		if i == max-1 {
			if over, oerr := openSSE(t, context.Background(), ts.URL); oerr != nil || over.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("non-kiosk stream into the reserved slot should get 503, got %v %v", oerr, over)
			} else {
				over.Body.Close()
			}
			resp, err = openKioskSSE(t, ctx, ts.URL)
		} else {
			resp, err = openSSE(t, ctx, ts.URL)
		}
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("stream %d: %v %v", i, err, resp)
		}
		waitLine(t, bufio.NewReader(resp.Body), "event: health")
		cancels = append(cancels, func() { cancel(); resp.Body.Close() })
	}
	if resp, err := openSSE(t, context.Background(), ts.URL); err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("stream over the limit should get 503, got %v %v", err, resp)
	} else {
		resp.Body.Close()
	}
	for _, c := range cancels {
		c()
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.srv.sseMu.Lock()
		n := len(h.srv.sseClients)
		h.srv.sseMu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d SSE slots still held 2 s after clients disconnected", n)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Shutdown: Close must end a live stream.
	resp, err := openSSE(t, context.Background(), ts.URL)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("stream after release: %v %v", err, resp)
	}
	br := bufio.NewReader(resp.Body)
	waitLine(t, br, "event: health")
	ended := make(chan struct{})
	go func() {
		for {
			if _, err := br.ReadString('\n'); err != nil {
				close(ended)
				return
			}
		}
	}()
	h.srv.Close()
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE stream still open 2 s after Server.Close")
	}
	resp.Body.Close()
	ts.Close()
	http.DefaultClient.CloseIdleConnections()
}
