package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLlamaSendsAPIKeyOnlyWhenConfigured(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		if r.URL.Path == "/health" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ठीक"}}]}`))
	}))
	defer srv.Close()

	withKey := NewLlama(LlamaConfig{BaseURL: srv.URL, APIKey: "s3cret"})
	if err := withKey.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := withKey.Complete(context.Background(), CompletionRequest{Messages: []Message{{Role: "user", Content: "x"}}}); err != nil {
		t.Fatal(err)
	}
	noKey := NewLlama(LlamaConfig{BaseURL: srv.URL})
	if _, err := noKey.Complete(context.Background(), CompletionRequest{Messages: []Message{{Role: "user", Content: "x"}}}); err != nil {
		t.Fatal(err)
	}
	want := []string{"Bearer s3cret", "Bearer s3cret", ""}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("request %d: Authorization %q, want %q", i, got[i], want[i])
		}
	}
}
