package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func devJSON(extra string) string {
	return `{"profile":"dev-mac","data_dir":"var/x","hal":{"button":"sim"},
	"knowledge":{"require_signed_bundle":false` + extra + `}}`
}

func TestDefaultsValid(t *testing.T) {
	c := Defaults()
	// Defaults are production-leaning: signed bundles required, which in
	// turn needs server keys only when a knowledge path is set.
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	if c.Session.CloseAfter.D() != 10*time.Minute || c.API.AudioCacheClips != 8 || c.API.MaxJSONBytes != 16<<10 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestLoadDevAndEnv(t *testing.T) {
	p := writeFile(t, devJSON(`,"allow_unverified_for_dev":true,"path":"packages/scheme-knowledge/records"`))
	c, err := Load(p, env(map[string]string{"JS_LLM_ENABLED": "false", "JS_LISTEN": "127.0.0.1:9999", "JS_MEMORY_LIMIT_MB": "128"}))
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsDev() || !c.Knowledge.AllowUnverifiedForDev || c.LLM.Enabled || c.Listen != "127.0.0.1:9999" || c.MemoryLimitMB != 128 {
		t.Fatalf("got %+v", c)
	}
	if c.DBPath() != filepath.Join("var/x", "device.db") || c.BackupDir() != filepath.Join("var/x", "backups") {
		t.Fatal("paths")
	}
}

func TestRefuseUnverifiedOnNonDev(t *testing.T) {
	p := writeFile(t, `{"profile":"pi5","knowledge":{"allow_unverified_for_dev":true}}`)
	_, err := Load(p, env(nil))
	if err == nil || !strings.Contains(err.Error(), "allow_unverified_for_dev is refused") {
		t.Fatalf("want refusal, got %v", err)
	}
	// Env cannot sneak a dev profile past it either: it changes the profile
	// for the whole device, which is visible and validated.
	if _, err := Load(p, env(map[string]string{"JS_PROFILE": "prod"})); err == nil {
		t.Fatal("unknown profile accepted")
	}
}

func TestNonDevForcesSignedBundle(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, `{"profile":"pi5","knowledge":{"require_signed_bundle":false,"path":"`+dir+`"}}`)
	_, err := Load(p, env(nil))
	if err == nil || !strings.Contains(err.Error(), "require_signed_bundle") {
		t.Fatalf("want signed-bundle error, got %v", err)
	}
}

func TestRejects(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.StdEncoding.EncodeToString(pub)
	cases := map[string]string{
		"unknown field":       `{"profile":"dev-mac","nope":1}`,
		"non-loopback listen": `{"profile":"dev-mac","listen":"0.0.0.0:8730"}`,
		"lan listen":          `{"profile":"dev-mac","listen":"192.168.1.5:8730"}`,
		"remote speech":       `{"profile":"dev-mac","speech":{"url":"http://10.0.0.2:8743"}}`,
		"remote llm":          `{"profile":"dev-mac","llm":{"enabled":true,"url":"http://example.com:8742"}}`,
		"too many clips":      `{"profile":"dev-mac","api":{"audio_cache_clips":9}}`,
		"too many sse":        `{"profile":"dev-mac","api":{"sse_max_clients":9}}`,
		"audio > 1MiB":        `{"profile":"dev-mac","api":{"max_audio_bytes":2097152}}`,
		"bad duration":        `{"profile":"dev-mac","api":{"turn_timeout":5}}`,
		"inverted session":    `{"profile":"dev-mac","session":{"reminder_after":"9m","final_warning_after":"2m"}}`,
		"http sync prod":      `{"profile":"pi5","sync":{"base_url":"http://central.example","server_keys":{"k":"` + key + `"}}}`,
		"sync without keys":   `{"profile":"dev-mac","sync":{"base_url":"https://central.example"}}`,
		"bad server key":      `{"profile":"dev-mac","sync":{"server_keys":{"k":"AAAA"}}}`,
		"bad button":          `{"profile":"dev-mac","hal":{"button":"usb"}}`,
		"bad gpio chip":       `{"profile":"pi5","hal":{"button":"gpio","gpio_chip":"/tmp/x"}}`,
		"trailing data":       `{"profile":"dev-mac"} {}`,
		"write<turn":          `{"profile":"dev-mac","api":{"turn_timeout":"60s","write_timeout":"61s"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeFile(t, body), env(nil)); err == nil {
				t.Fatalf("accepted: %s", body)
			}
		})
	}
}

func TestDevSyncLoopbackHTTPAllowed(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.StdEncoding.EncodeToString(pub)
	p := writeFile(t, devJSON("")[:len(devJSON(""))-1]+`,"sync":{"base_url":"http://127.0.0.1:8750","server_keys":{"srv-1":"`+key+`"}}}`)
	c, err := Load(p, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := c.ServerPublicKeys()
	if err != nil || len(keys["srv-1"]) != ed25519.PublicKeySize {
		t.Fatalf("keys: %v %v", keys, err)
	}
}

func TestRepoConfigExamplesValid(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..")
	for _, name := range []string{"device.dev.json", "device.pi5.json"} {
		p := filepath.Join(root, "config", name)
		if _, err := os.Stat(p); err != nil {
			t.Skipf("%s not present", p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		c := Defaults()
		if err := Decode(b, &c); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// Knowledge paths are relative to the repo root in the dev file and
		// absolute device paths in the pi5 file; validate structure only.
		c.Knowledge.Path = ""
		if err := c.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestProductionRefusesDevOnlyFeatures(t *testing.T) {
	cases := map[string]string{
		"pprof on pi5": `{"profile":"pi5","pprof_addr":"127.0.0.1:6060"}`,
		"sim on pi5":   `{"profile":"pi5","hal":{"button":"sim"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeFile(t, body), env(nil))
			if err == nil || !strings.Contains(err.Error(), "refused") {
				t.Fatalf("want refusal, got %v", err)
			}
		})
	}
	// Env cannot switch a pi5 to the simulated button either.
	if _, err := Load(writeFile(t, `{"profile":"pi5"}`), env(map[string]string{"JS_HAL_BUTTON": "sim"})); err == nil {
		t.Fatal("JS_HAL_BUTTON=sim accepted on pi5")
	}
	// Dev profiles may still use both.
	if _, err := Load(writeFile(t, devJSON("")[:len(devJSON(""))-1]+`,"pprof_addr":"127.0.0.1:6060"}`), env(nil)); err != nil {
		t.Fatalf("dev pprof refused: %v", err)
	}
}

func TestDevProfileRefusedOnKioskPlatform(t *testing.T) {
	old := platform
	platform = kioskPlatform
	t.Cleanup(func() { platform = old })
	p := writeFile(t, devJSON(""))
	if _, err := Load(p, env(nil)); err == nil || !strings.Contains(err.Error(), "JS_ALLOW_DEV_PROFILE") {
		t.Fatalf("want dev-profile refusal on %s, got %v", kioskPlatform, err)
	}
	if _, err := Load(p, env(map[string]string{"JS_ALLOW_DEV_PROFILE": "1"})); err != nil {
		t.Fatalf("explicit override refused: %v", err)
	}
	if _, err := Load(writeFile(t, `{"profile":"pi5"}`), env(nil)); err != nil {
		t.Fatalf("pi5 refused on kiosk platform: %v", err)
	}
}

func TestDefaultsKeepRephraseOff(t *testing.T) {
	if Defaults().LLM.Rephrase {
		t.Fatal("rephrase must be off by default (pi5) until field-validated")
	}
}

func TestLLMAPIKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "key")
	if err := os.WriteFile(p, []byte("# llama key\n0123456789abcdef0123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Defaults()
	c.LLM.APIKeyFile = p
	k, err := c.LLMAPIKey()
	if err != nil || k != "0123456789abcdef0123" {
		t.Fatalf("key %q err %v", k, err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.LLMAPIKey(); err == nil {
		t.Fatal("world-readable key file accepted on pi5")
	}
	c.Profile = ProfileDevMac
	if _, err := c.LLMAPIKey(); err != nil {
		t.Fatalf("dev: %v", err)
	}
}
