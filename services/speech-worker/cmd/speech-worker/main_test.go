package main

import (
	"path/filepath"
	"testing"
)

func TestParseFlags(t *testing.T) {
	c, err := parseFlags([]string{"-models-dir", "/m", "-threads", "2"})
	if err != nil {
		t.Fatal(err)
	}
	if c.addr != "127.0.0.1:8743" || c.threads != 2 || c.ttsSteps != 4 {
		t.Fatalf("%+v", c)
	}
	if c.asrTokens != filepath.Join("/m", "stt", "indicconformer-hi", "tokens.txt") {
		t.Fatalf("tokens=%s", c.asrTokens)
	}
	for _, bad := range [][]string{
		{"-addr", "0.0.0.0:8743"},
		{"-addr", "192.168.1.5:8743"},
		{"-addr", "nonsense"},
		{"-threads", "0"},
		{"-tts-steps", "100"},
	} {
		if _, err := parseFlags(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if _, err := parseFlags([]string{"-addr", "[::1]:9000"}); err != nil {
		t.Errorf("ipv6 loopback rejected: %v", err)
	}
}
