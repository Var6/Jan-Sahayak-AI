// Package config loads the device agent configuration: a JSON file (unknown
// fields rejected) overlaid on safe defaults, then a small set of JS_*
// environment overrides, then validation. Every limit and timeout the agent
// uses lives here so that a technician can tune a kiosk without a rebuild,
// and Validate keeps each value inside a safe range.
//
// Profiles:
//
//	dev-mac  developer laptop: simulated button, local files, unverified
//	         knowledge may be shown (with a disclaimer) when explicitly allowed.
//	pi5      Raspberry Pi 5 kiosk: GPIO button, signed knowledge bundles only.
//
// Knowledge.AllowUnverifiedForDev, pprof_addr and the simulated button are
// refused (Validate error) unless the profile starts with "dev". A dev
// profile is itself refused on linux/arm64 (the kiosk hardware) unless the
// environment sets JS_ALLOW_DEV_PROFILE=1, so a mis-copied dev config cannot
// silently turn a field kiosk into a developer machine.
package config

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Duration is a time.Duration that reads and writes JSON strings ("5s").
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// MarshalJSON writes "1m30s" style strings.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// UnmarshalJSON accepts Go duration strings only (no bare numbers, which are
// ambiguous about units).
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("duration must be a string such as \"5s\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Profiles.
const (
	ProfileDevMac = "dev-mac"
	ProfilePi5    = "pi5"
)

// Config is the whole agent configuration.
type Config struct {
	Profile string `json:"profile"`
	// DataDir holds device.db, backups/ and identity.json.
	DataDir string `json:"data_dir"`
	// Listen is the loopback address of the local API + kiosk UI.
	Listen          string   `json:"listen"`
	LogLevel        string   `json:"log_level"` // debug | info | warn | error
	DefaultLanguage string   `json:"default_language"`
	MemoryLimitMB   int      `json:"memory_limit_mb"` // GOMEMLIMIT (soft); 0 = leave runtime default
	ShutdownTimeout Duration `json:"shutdown_timeout"`
	// PprofAddr enables net/http/pprof on a separate loopback port ("" = off).
	PprofAddr string `json:"pprof_addr"`

	Session   SessionConfig   `json:"session"`
	API       APIConfig       `json:"api"`
	Speech    SpeechConfig    `json:"speech"`
	LLM       LLMConfig       `json:"llm"`
	Dialogue  DialogueConfig  `json:"dialogue"`
	Knowledge KnowledgeConfig `json:"knowledge"`
	Sync      SyncConfig      `json:"sync"`
	Health    HealthConfig    `json:"health"`
	Backup    BackupConfig    `json:"backup"`
	HAL       HALConfig       `json:"hal"`
}

// SessionConfig holds the inactivity rules (2/9/10 minutes by default).
type SessionConfig struct {
	ReminderAfter     Duration `json:"reminder_after"`
	FinalWarningAfter Duration `json:"final_warning_after"`
	CloseAfter        Duration `json:"close_after"`
}

// APIConfig bounds the local HTTP API.
type APIConfig struct {
	MaxJSONBytes      int64    `json:"max_json_bytes"`
	MaxAudioBytes     int64    `json:"max_audio_bytes"`
	MaxTextRunes      int      `json:"max_text_runes"`
	TurnTimeout       Duration `json:"turn_timeout"`    // audio/text/option turn incl. STT+LLM+TTS
	ControlTimeout    Duration `json:"control_timeout"` // listen/touch/language/…
	StartTimeout      Duration `json:"start_timeout"`   // start/end incl. greeting/goodbye TTS
	ReadHeaderTimeout Duration `json:"read_header_timeout"`
	ReadTimeout       Duration `json:"read_timeout"`
	WriteTimeout      Duration `json:"write_timeout"`
	IdleTimeout       Duration `json:"idle_timeout"`
	SSEMaxClients     int      `json:"sse_max_clients"`
	SSEPingInterval   Duration `json:"sse_ping_interval"`
	SSEMaxLifetime    Duration `json:"sse_max_lifetime"`
	AudioCacheClips   int      `json:"audio_cache_clips"`
	AudioCacheBytes   int64    `json:"audio_cache_bytes"`
	GoodbyeGrace      Duration `json:"goodbye_grace"` // goodbye clip kept after session end
}

// SpeechConfig points at the speech worker (or whisper-server for STT).
type SpeechConfig struct {
	// STTEngine is "speech-worker" (IndicConformer, default) or "whisper"
	// (whisper-server, dev comparison only). "none" disables voice input.
	STTEngine    string   `json:"stt_engine"`
	TTSEnabled   bool     `json:"tts_enabled"`
	URL          string   `json:"url"`
	WhisperURL   string   `json:"whisper_url"`
	STTTimeout   Duration `json:"stt_timeout"`
	TTSTimeout   Duration `json:"tts_timeout"`
	MaxAudioSecs float64  `json:"max_audio_secs"`
	TTSSpeed     float64  `json:"tts_speed"` // 0 = worker default
	// TTSLangs are the languages the worker's voice speaks (its
	// -tts-langs). Sessions in other languages get no voice output.
	TTSLangs []string `json:"tts_langs"`
}

// LLMConfig points at llama-server.
type LLMConfig struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
	// APIKeyFile holds the bearer key llama-server was started with
	// (--api-key-file). On the Pi both units load it as a systemd
	// credential, so only each service user can read its copy.
	APIKeyFile      string   `json:"api_key_file"`
	Timeout         Duration `json:"timeout"`
	MaxTokens       int      `json:"max_tokens"`
	Rephrase        bool     `json:"rephrase"`
	ClassifyTimeout Duration `json:"classify_timeout"`
	RephraseTimeout Duration `json:"rephrase_timeout"`
}

// DialogueConfig tunes the conversation engine.
type DialogueConfig struct {
	MinSTTConfidence float64 `json:"min_stt_confidence"`
	MaxMisses        int     `json:"max_misses"`
}

// KnowledgeConfig says where scheme records come from.
type KnowledgeConfig struct {
	// Path is a directory of record files (dev) or a bundle JSON file.
	// Empty = use what is already in the device database.
	Path          string `json:"path"`
	GazetteerPath string `json:"gazetteer_path"`
	HelplinesPath string `json:"helplines_path"`
	// RequireSignedBundle: Path must be a bundle signed by a Sync.ServerKeys
	// key. Forced on for non-dev profiles.
	RequireSignedBundle bool `json:"require_signed_bundle"`
	// AllowUnverifiedForDev shows UNVERIFIED content under a disclaimer.
	// Refused unless the profile starts with "dev".
	AllowUnverifiedForDev bool `json:"allow_unverified_for_dev"`
}

// SyncConfig configures uploads to the central API and the outbox bounds.
type SyncConfig struct {
	BaseURL           string            `json:"base_url"`    // "" = offline-only device
	ServerKeys        map[string]string `json:"server_keys"` // key_id → base64 Ed25519 public key
	BatchSize         int               `json:"batch_size"`
	Interval          Duration          `json:"interval"`
	HTTPTimeout       Duration          `json:"http_timeout"`
	HousekeepInterval Duration          `json:"housekeep_interval"`
	OutboxMaxRows     int               `json:"outbox_max_rows"`
	AckRetention      Duration          `json:"ack_retention"`
	MaxAttempts       int               `json:"max_attempts"`
}

// HealthConfig holds check intervals and thresholds.
type HealthConfig struct {
	DBInterval         Duration `json:"db_interval"`
	QuickCheckInterval Duration `json:"quick_check_interval"`
	AIInterval         Duration `json:"ai_interval"`
	ResourceInterval   Duration `json:"resource_interval"`
	// Process memory (RSS) thresholds.
	MemWarnMB int `json:"mem_warn_mb"`
	MemCritMB int `json:"mem_crit_mb"`
	// System available memory thresholds (percent of total; Linux only).
	SysMemWarnPct float64 `json:"sys_mem_warn_pct"`
	SysMemCritPct float64 `json:"sys_mem_crit_pct"`
	// Free space on the DataDir filesystem.
	DiskWarnMB int `json:"disk_warn_mb"`
	DiskCritMB int `json:"disk_crit_mb"`
	// SoC temperature (°C).
	TempWarnC float64 `json:"temp_warn_c"`
	TempCritC float64 `json:"temp_crit_c"`
	// Goroutine counts.
	GoroutineWarn int `json:"goroutine_warn"`
	GoroutineCrit int `json:"goroutine_crit"`
	// Sync: consecutive failures before DEGRADED.
	SyncFailWarn int `json:"sync_fail_warn"`
}

// BackupConfig controls the daily SQLite backup.
type BackupConfig struct {
	Enabled bool   `json:"enabled"`
	Hour    int    `json:"hour"` // local hour 0–23 (quiet time)
	Keep    int    `json:"keep"`
	Dir     string `json:"dir"` // default <data_dir>/backups
}

// HALConfig selects the button implementation.
type HALConfig struct {
	Button      string   `json:"button"`        // "sim" | "gpio"
	GPIOChip    string   `json:"gpio_chip"`     // e.g. /dev/gpiochip0 (Pi 5: gpiochip4 on older kernels)
	GPIOLine    int      `json:"gpio_line"`     // line offset on that chip
	ActiveLow   bool     `json:"active_low"`    // button pulls the line to ground
	PullUp      bool     `json:"pull_up"`       // enable internal pull-up bias
	Debounce    Duration `json:"debounce"`      // kernel debounce period
	MinPressGap Duration `json:"min_press_gap"` // ignore presses closer than this
}

// Defaults returns safe defaults (production-leaning; dev settings come
// from the dev config file).
func Defaults() Config {
	return Config{
		Profile:         ProfilePi5,
		DataDir:         "/var/lib/jan-sahayak",
		Listen:          "127.0.0.1:8730",
		LogLevel:        "info",
		DefaultLanguage: "hi",
		MemoryLimitMB:   200,
		ShutdownTimeout: Duration(15 * time.Second),
		Session: SessionConfig{
			ReminderAfter:     Duration(2 * time.Minute),
			FinalWarningAfter: Duration(9 * time.Minute),
			CloseAfter:        Duration(10 * time.Minute),
		},
		API: APIConfig{
			MaxJSONBytes:      16 << 10,
			MaxAudioBytes:     1 << 20,
			MaxTextRunes:      500,
			TurnTimeout:       Duration(55 * time.Second),
			ControlTimeout:    Duration(5 * time.Second),
			StartTimeout:      Duration(15 * time.Second),
			ReadHeaderTimeout: Duration(5 * time.Second),
			ReadTimeout:       Duration(30 * time.Second),
			WriteTimeout:      Duration(70 * time.Second),
			IdleTimeout:       Duration(60 * time.Second),
			SSEMaxClients:     4,
			SSEPingInterval:   Duration(15 * time.Second),
			SSEMaxLifetime:    Duration(30 * time.Minute),
			AudioCacheClips:   8,
			AudioCacheBytes:   24 << 20,
			GoodbyeGrace:      Duration(20 * time.Second),
		},
		Speech: SpeechConfig{
			STTEngine:    "speech-worker",
			TTSEnabled:   true,
			URL:          "http://127.0.0.1:8743",
			WhisperURL:   "http://127.0.0.1:8741",
			STTTimeout:   Duration(20 * time.Second),
			TTSTimeout:   Duration(30 * time.Second),
			MaxAudioSecs: 20,
			TTSLangs:     []string{"hi"},
		},
		LLM: LLMConfig{
			Enabled:   true,
			URL:       "http://127.0.0.1:8742",
			Timeout:   Duration(20 * time.Second),
			MaxTokens: 256,
			// Rephrase stays off on production profiles until the
			// allowlist validator has been field-validated; classification
			// hints remain on.
			Rephrase:        false,
			ClassifyTimeout: Duration(4 * time.Second),
			RephraseTimeout: Duration(8 * time.Second),
		},
		Dialogue: DialogueConfig{MinSTTConfidence: 0.45, MaxMisses: 3},
		Knowledge: KnowledgeConfig{
			RequireSignedBundle: true,
		},
		Sync: SyncConfig{
			BatchSize:         100,
			Interval:          Duration(time.Minute),
			HTTPTimeout:       Duration(30 * time.Second),
			HousekeepInterval: Duration(time.Hour),
			OutboxMaxRows:     100_000,
			AckRetention:      Duration(7 * 24 * time.Hour),
			MaxAttempts:       50,
		},
		Health: HealthConfig{
			DBInterval:         Duration(15 * time.Second),
			QuickCheckInterval: Duration(6 * time.Hour),
			AIInterval:         Duration(15 * time.Second),
			ResourceInterval:   Duration(30 * time.Second),
			MemWarnMB:          150,
			MemCritMB:          220,
			SysMemWarnPct:      10,
			SysMemCritPct:      4,
			DiskWarnMB:         1024,
			DiskCritMB:         200,
			TempWarnC:          75,
			TempCritC:          82,
			GoroutineWarn:      300,
			GoroutineCrit:      1500,
			SyncFailWarn:       5,
		},
		Backup: BackupConfig{Enabled: true, Hour: 3, Keep: 7},
		HAL: HALConfig{
			Button:      "gpio",
			GPIOChip:    "/dev/gpiochip0",
			GPIOLine:    17,
			ActiveLow:   true,
			PullUp:      true,
			Debounce:    Duration(30 * time.Millisecond),
			MinPressGap: Duration(time.Second),
		},
	}
}

// MaxConfigBytes bounds the config file.
const MaxConfigBytes = 256 << 10

// Load reads path (may be empty: defaults only), applies env overrides and
// validates. Relative paths in the file are used as given (relative to the
// agent's working directory).
func Load(path string, getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	c := Defaults()
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return c, fmt.Errorf("config: %w", err)
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes+1))
		if err != nil {
			return c, fmt.Errorf("config: read: %w", err)
		}
		if len(b) > MaxConfigBytes {
			return c, errors.New("config: file too large")
		}
		if err := Decode(b, &c); err != nil {
			return c, fmt.Errorf("config: %s: %w", path, err)
		}
	}
	if err := applyEnv(&c, getenv); err != nil {
		return c, err
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	if c.IsDev() && platform == kioskPlatform && strings.TrimSpace(getenv("JS_ALLOW_DEV_PROFILE")) != "1" {
		return c, fmt.Errorf("config: profile %q is a development profile and is refused on %s (kiosk hardware); "+
			"use profile %s, or set JS_ALLOW_DEV_PROFILE=1 on a bench device", c.Profile, platform, ProfilePi5)
	}
	return c, nil
}

// Decode overlays JSON onto c, rejecting unknown fields and trailing data.
func Decode(b []byte, c *Config) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(c); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after JSON object")
	}
	return nil
}

// EnvVars lists the supported overrides (documented in the dev README).
var EnvVars = []string{
	"JS_PROFILE", "JS_DATA_DIR", "JS_LISTEN", "JS_LOG_LEVEL", "JS_SPEECH_URL",
	"JS_LLM_URL", "JS_LLM_ENABLED", "JS_SYNC_URL", "JS_KNOWLEDGE_PATH",
	"JS_PPROF_ADDR", "JS_MEMORY_LIMIT_MB", "JS_HAL_BUTTON", "JS_LLM_API_KEY_FILE",
	"JS_ALLOW_DEV_PROFILE",
}

// platform is GOOS/GOARCH; a variable so tests can pretend to be a Pi.
var platform = runtime.GOOS + "/" + runtime.GOARCH

// kioskPlatform is the production hardware's platform.
const kioskPlatform = "linux/arm64"

func applyEnv(c *Config, getenv func(string) string) error {
	set := func(k string, dst *string) {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			*dst = v
		}
	}
	set("JS_PROFILE", &c.Profile)
	set("JS_DATA_DIR", &c.DataDir)
	set("JS_LISTEN", &c.Listen)
	set("JS_LOG_LEVEL", &c.LogLevel)
	set("JS_SPEECH_URL", &c.Speech.URL)
	set("JS_LLM_URL", &c.LLM.URL)
	set("JS_SYNC_URL", &c.Sync.BaseURL)
	set("JS_KNOWLEDGE_PATH", &c.Knowledge.Path)
	set("JS_PPROF_ADDR", &c.PprofAddr)
	set("JS_HAL_BUTTON", &c.HAL.Button)
	set("JS_LLM_API_KEY_FILE", &c.LLM.APIKeyFile)
	if v := strings.TrimSpace(getenv("JS_LLM_ENABLED")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config: JS_LLM_ENABLED: %w", err)
		}
		c.LLM.Enabled = b
	}
	if v := strings.TrimSpace(getenv("JS_MEMORY_LIMIT_MB")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: JS_MEMORY_LIMIT_MB: %w", err)
		}
		c.MemoryLimitMB = n
	}
	return nil
}

// IsDev reports whether this is a development profile.
func (c Config) IsDev() bool { return strings.HasPrefix(c.Profile, "dev") }

// DBPath is the device database file.
func (c Config) DBPath() string { return filepath.Join(c.DataDir, "device.db") }

// IdentityPath is the device key file.
func (c Config) IdentityPath() string { return filepath.Join(c.DataDir, "identity.json") }

// BackupDir is where backups go.
func (c Config) BackupDir() string {
	if c.Backup.Dir != "" {
		return c.Backup.Dir
	}
	return filepath.Join(c.DataDir, "backups")
}

// ServerPublicKeys decodes Sync.ServerKeys.
func (c Config) ServerPublicKeys() (map[string]ed25519.PublicKey, error) {
	out := make(map[string]ed25519.PublicKey, len(c.Sync.ServerKeys))
	for id, s := range c.Sync.ServerKeys {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(b) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("config: sync.server_keys[%q] is not a base64 Ed25519 public key", id)
		}
		out[id] = ed25519.PublicKey(b)
	}
	return out, nil
}

// Validate checks every value against a safe range.
func (c *Config) Validate() error {
	var errs []string
	bad := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }
	durIn := func(name string, d Duration, lo, hi time.Duration) {
		if d.D() < lo || d.D() > hi {
			bad("%s=%s must be within %s..%s", name, d.D(), lo, hi)
		}
	}
	intIn := func(name string, v, lo, hi int) {
		if v < lo || v > hi {
			bad("%s=%d must be within %d..%d", name, v, lo, hi)
		}
	}

	switch {
	case c.Profile == ProfilePi5, c.Profile == ProfileDevMac, strings.HasPrefix(c.Profile, "dev-"):
	default:
		bad("profile %q unknown (want %s, %s or dev-*)", c.Profile, ProfileDevMac, ProfilePi5)
	}
	if c.Knowledge.AllowUnverifiedForDev && !c.IsDev() {
		bad("knowledge.allow_unverified_for_dev is refused for profile %q (only dev* profiles)", c.Profile)
	}
	if !c.IsDev() {
		// Production devices only accept signed bundles.
		c.Knowledge.RequireSignedBundle = true
	}
	if c.DataDir == "" {
		bad("data_dir is required")
	}
	if err := loopbackHostPort(c.Listen); err != nil {
		bad("listen: %v", err)
	}
	if c.PprofAddr != "" {
		if !c.IsDev() {
			bad("pprof_addr is refused for profile %q (only dev* profiles)", c.Profile)
		}
		if err := loopbackHostPort(c.PprofAddr); err != nil {
			bad("pprof_addr: %v", err)
		} else if c.PprofAddr == c.Listen {
			bad("pprof_addr must differ from listen")
		}
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		bad("log_level %q (want debug|info|warn|error)", c.LogLevel)
	}
	if c.DefaultLanguage != "hi" && c.DefaultLanguage != "en" {
		bad("default_language must be hi or en")
	}
	if c.MemoryLimitMB != 0 {
		intIn("memory_limit_mb", c.MemoryLimitMB, 32, 4096)
	}
	durIn("shutdown_timeout", c.ShutdownTimeout, time.Second, 2*time.Minute)

	s := c.Session
	durIn("session.reminder_after", s.ReminderAfter, 10*time.Second, time.Hour)
	durIn("session.final_warning_after", s.FinalWarningAfter, 10*time.Second, time.Hour)
	durIn("session.close_after", s.CloseAfter, 20*time.Second, time.Hour)
	if !(s.ReminderAfter < s.FinalWarningAfter && s.FinalWarningAfter < s.CloseAfter) {
		bad("session: need reminder_after < final_warning_after < close_after")
	}

	a := c.API
	if a.MaxJSONBytes < 1<<10 || a.MaxJSONBytes > 64<<10 {
		bad("api.max_json_bytes must be within 1 KiB..64 KiB")
	}
	if a.MaxAudioBytes < 64<<10 || a.MaxAudioBytes > 1<<20 {
		bad("api.max_audio_bytes must be within 64 KiB..1 MiB (speech worker limit)")
	}
	intIn("api.max_text_runes", a.MaxTextRunes, 50, 500)
	durIn("api.turn_timeout", a.TurnTimeout, 5*time.Second, 2*time.Minute)
	durIn("api.control_timeout", a.ControlTimeout, time.Second, 30*time.Second)
	durIn("api.start_timeout", a.StartTimeout, 2*time.Second, time.Minute)
	durIn("api.read_header_timeout", a.ReadHeaderTimeout, time.Second, 30*time.Second)
	durIn("api.read_timeout", a.ReadTimeout, 2*time.Second, 2*time.Minute)
	durIn("api.write_timeout", a.WriteTimeout, 5*time.Second, 3*time.Minute)
	durIn("api.idle_timeout", a.IdleTimeout, time.Second, 10*time.Minute)
	if a.WriteTimeout.D() < a.TurnTimeout.D()+5*time.Second {
		bad("api.write_timeout must exceed api.turn_timeout by at least 5s")
	}
	intIn("api.sse_max_clients", a.SSEMaxClients, 1, 8)
	durIn("api.sse_ping_interval", a.SSEPingInterval, time.Second, time.Minute)
	durIn("api.sse_max_lifetime", a.SSEMaxLifetime, time.Minute, 24*time.Hour)
	intIn("api.audio_cache_clips", a.AudioCacheClips, 1, 8)
	if a.AudioCacheBytes < 1<<20 || a.AudioCacheBytes > 64<<20 {
		bad("api.audio_cache_bytes must be within 1 MiB..64 MiB")
	}
	durIn("api.goodbye_grace", a.GoodbyeGrace, 0, 2*time.Minute)

	sp := c.Speech
	switch sp.STTEngine {
	case "speech-worker", "whisper", "none":
	default:
		bad("speech.stt_engine %q (want speech-worker|whisper|none)", sp.STTEngine)
	}
	if sp.STTEngine == "speech-worker" || sp.TTSEnabled {
		if err := loopbackURL(sp.URL); err != nil {
			bad("speech.url: %v", err)
		}
	}
	if sp.STTEngine == "whisper" {
		if err := loopbackURL(sp.WhisperURL); err != nil {
			bad("speech.whisper_url: %v", err)
		}
	}
	durIn("speech.stt_timeout", sp.STTTimeout, time.Second, time.Minute)
	durIn("speech.tts_timeout", sp.TTSTimeout, time.Second, 2*time.Minute)
	if sp.MaxAudioSecs < 1 || sp.MaxAudioSecs > 30 {
		bad("speech.max_audio_secs must be within 1..30")
	}
	for _, l := range sp.TTSLangs {
		if l != "hi" && l != "en" {
			bad("speech.tts_langs: %q (want hi and/or en)", l)
		}
	}
	if sp.TTSSpeed != 0 && (sp.TTSSpeed < 0.5 || sp.TTSSpeed > 2) {
		bad("speech.tts_speed must be 0 (default) or within 0.5..2")
	}

	l := c.LLM
	if l.Enabled {
		if err := loopbackURL(l.URL); err != nil {
			bad("llm.url: %v", err)
		}
	}
	if l.APIKeyFile != "" && !filepath.IsAbs(l.APIKeyFile) && !c.IsDev() {
		bad("llm.api_key_file must be an absolute path")
	}
	durIn("llm.timeout", l.Timeout, time.Second, 2*time.Minute)
	intIn("llm.max_tokens", l.MaxTokens, 16, 1024)
	durIn("llm.classify_timeout", l.ClassifyTimeout, 500*time.Millisecond, 30*time.Second)
	durIn("llm.rephrase_timeout", l.RephraseTimeout, 500*time.Millisecond, time.Minute)

	if c.Dialogue.MinSTTConfidence <= 0 || c.Dialogue.MinSTTConfidence >= 1 {
		bad("dialogue.min_stt_confidence must be within (0,1)")
	}
	intIn("dialogue.max_misses", c.Dialogue.MaxMisses, 1, 10)

	sy := c.Sync
	if sy.BaseURL != "" {
		u, err := url.Parse(sy.BaseURL)
		switch {
		case err != nil || u.Host == "":
			bad("sync.base_url is not a URL")
		case u.Scheme == "https":
		case u.Scheme == "http" && c.IsDev() && loopbackURL(sy.BaseURL) == nil:
		default:
			bad("sync.base_url must be https (plain http only to loopback on dev profiles)")
		}
		if len(sy.ServerKeys) == 0 {
			bad("sync.server_keys is required when sync.base_url is set")
		}
	}
	if _, err := c.ServerPublicKeys(); err != nil {
		bad("%v", err)
	}
	intIn("sync.batch_size", sy.BatchSize, 1, 200)
	durIn("sync.interval", sy.Interval, 5*time.Second, 24*time.Hour)
	durIn("sync.http_timeout", sy.HTTPTimeout, 2*time.Second, 5*time.Minute)
	durIn("sync.housekeep_interval", sy.HousekeepInterval, time.Minute, 24*time.Hour)
	intIn("sync.outbox_max_rows", sy.OutboxMaxRows, 1000, 1_000_000)
	durIn("sync.ack_retention", sy.AckRetention, time.Hour, 90*24*time.Hour)
	intIn("sync.max_attempts", sy.MaxAttempts, 1, 1000)

	k := c.Knowledge
	if k.RequireSignedBundle && k.Path != "" {
		if fi, err := os.Stat(k.Path); err == nil && fi.IsDir() {
			bad("knowledge.path is a directory but require_signed_bundle is on (use a signed bundle file)")
		}
		if len(sy.ServerKeys) == 0 {
			bad("knowledge.require_signed_bundle needs sync.server_keys to verify the bundle")
		}
	}

	h := c.Health
	durIn("health.db_interval", h.DBInterval, time.Second, 10*time.Minute)
	durIn("health.quick_check_interval", h.QuickCheckInterval, time.Minute, 7*24*time.Hour)
	durIn("health.ai_interval", h.AIInterval, time.Second, 10*time.Minute)
	durIn("health.resource_interval", h.ResourceInterval, time.Second, 10*time.Minute)
	if !(h.MemWarnMB > 0 && h.MemWarnMB < h.MemCritMB) {
		bad("health: need 0 < mem_warn_mb < mem_crit_mb")
	}
	if !(h.SysMemCritPct >= 0 && h.SysMemCritPct < h.SysMemWarnPct && h.SysMemWarnPct < 100) {
		bad("health: need 0 <= sys_mem_crit_pct < sys_mem_warn_pct < 100")
	}
	if !(h.DiskCritMB > 0 && h.DiskCritMB < h.DiskWarnMB) {
		bad("health: need 0 < disk_crit_mb < disk_warn_mb")
	}
	if !(h.TempWarnC > 30 && h.TempWarnC < h.TempCritC && h.TempCritC <= 105) {
		bad("health: need 30 < temp_warn_c < temp_crit_c <= 105")
	}
	if !(h.GoroutineWarn > 10 && h.GoroutineWarn < h.GoroutineCrit) {
		bad("health: need 10 < goroutine_warn < goroutine_crit")
	}
	intIn("health.sync_fail_warn", h.SyncFailWarn, 1, 1000)

	b := c.Backup
	intIn("backup.hour", b.Hour, 0, 23)
	intIn("backup.keep", b.Keep, 1, 60)

	hl := c.HAL
	switch hl.Button {
	case "sim":
		if !c.IsDev() {
			// The simulated button enables POST /session/start, i.e.
			// sessions (and the microphone flow) without the physical button.
			bad("hal.button \"sim\" is refused for profile %q (only dev* profiles)", c.Profile)
		}
	case "gpio":
		if !strings.HasPrefix(hl.GPIOChip, "/dev/gpiochip") {
			bad("hal.gpio_chip must be a /dev/gpiochipN path")
		}
		intIn("hal.gpio_line", hl.GPIOLine, 0, 511)
	default:
		bad("hal.button %q (want sim|gpio)", hl.Button)
	}
	durIn("hal.debounce", hl.Debounce, 0, time.Second)
	durIn("hal.min_press_gap", hl.MinPressGap, 0, 10*time.Second)

	if len(errs) > 0 {
		return errors.New("config: invalid:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}

func loopbackHostPort(hp string) error {
	host, port, err := net.SplitHostPort(hp)
	if err != nil {
		return err
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("bad port %q", port)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%q is not a loopback address (the agent never listens on the network)", host)
	}
	return nil
}

func loopbackURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return fmt.Errorf("%q must be an http://127.0.0.1:PORT URL", s)
	}
	if u.Port() == "" {
		return fmt.Errorf("%q must include a port", s)
	}
	return loopbackHostPort(u.Host)
}

// LLMAPIKey reads the first key from LLM.APIKeyFile ("" when unset). The
// file uses llama-server's format: one key per line, '#' starts a comment.
// On non-dev profiles a key file readable by group or others is refused.
func (c Config) LLMAPIKey() (string, error) {
	p := c.LLM.APIKeyFile
	if p == "" {
		return "", nil
	}
	f, err := os.Open(p)
	if err != nil {
		return "", fmt.Errorf("config: llm.api_key_file: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("config: llm.api_key_file: %w", err)
	}
	if !c.IsDev() && fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("config: llm.api_key_file %s is readable by other users (mode %v); want 0400 or 0600", p, fi.Mode().Perm())
	}
	b, err := io.ReadAll(io.LimitReader(f, 4<<10))
	if err != nil {
		return "", fmt.Errorf("config: llm.api_key_file: %w", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) < 16 {
			return "", errors.New("config: llm.api_key_file: key shorter than 16 characters")
		}
		return line, nil
	}
	return "", errors.New("config: llm.api_key_file contains no key")
}
