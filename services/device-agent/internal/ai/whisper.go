package ai

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// WhisperConfig configures the whisper-server client.
type WhisperConfig struct {
	BaseURL string        // e.g. http://127.0.0.1:8741
	Timeout time.Duration // per request
	// MaxAudioSecs rejects longer clips before sending (bounded work).
	MaxAudioSecs float64
	// NoSpeechThreshold: segments with no_speech_prob above this are ignored.
	NoSpeechThreshold float64
}

// Whisper talks to whisper.cpp's whisper-server.
type Whisper struct {
	cfg  WhisperConfig
	http *http.Client
	g    *gate
}

// NewWhisper creates a client.
func NewWhisper(cfg WhisperConfig) *Whisper {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxAudioSecs <= 0 {
		cfg.MaxAudioSecs = 30
	}
	if cfg.NoSpeechThreshold <= 0 {
		cfg.NoSpeechThreshold = 0.6
	}
	return &Whisper{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}, g: newGate()}
}

// MaxWAVBytes bounds uploaded audio: 30 s × 16 kHz × 2 bytes + header slack.
const MaxWAVBytes = 30*16000*2 + 4096

// ErrBadAudio is returned for audio that is not 16 kHz mono 16-bit PCM WAV.
var ErrBadAudio = errors.New("ai: audio must be 16 kHz mono 16-bit PCM WAV")

// WAVInfo describes a parsed WAV header.
type WAVInfo struct {
	SampleRate int
	Channels   int
	Bits       int
	DataBytes  int
	Seconds    float64
}

// ParseWAV validates a canonical RIFF/WAVE PCM file and returns its info.
func ParseWAV(b []byte) (WAVInfo, error) {
	var info WAVInfo
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return info, ErrBadAudio
	}
	off := 12
	var gotFmt bool
	for off+8 <= len(b) {
		id := string(b[off : off+4])
		size := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		body := off + 8
		if size < 0 || body+size > len(b) {
			if id == "data" { // some writers put a bogus size on streamed data
				size = len(b) - body
			} else {
				return info, ErrBadAudio
			}
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return info, ErrBadAudio
			}
			format := binary.LittleEndian.Uint16(b[body:])
			info.Channels = int(binary.LittleEndian.Uint16(b[body+2:]))
			info.SampleRate = int(binary.LittleEndian.Uint32(b[body+4:]))
			info.Bits = int(binary.LittleEndian.Uint16(b[body+14:]))
			if format != 1 && format != 0xFFFE {
				return info, ErrBadAudio
			}
			gotFmt = true
		case "data":
			if !gotFmt {
				return info, ErrBadAudio
			}
			info.DataBytes = size
			if info.SampleRate != 16000 || info.Channels != 1 || info.Bits != 16 {
				return info, fmt.Errorf("%w (got %d Hz, %d ch, %d bit)", ErrBadAudio, info.SampleRate, info.Channels, info.Bits)
			}
			info.Seconds = float64(size) / float64(info.SampleRate*2)
			return info, nil
		}
		off = body + size + size%2
	}
	return info, ErrBadAudio
}

type whisperSegment struct {
	Text         string  `json:"text"`
	Start        float64 `json:"start"`
	End          float64 `json:"end"`
	AvgLogprob   float64 `json:"avg_logprob"`
	NoSpeechProb float64 `json:"no_speech_prob"`
}

type whisperResponse struct {
	Text     string           `json:"text"`
	Language string           `json:"language"`
	Duration float64          `json:"duration"`
	Segments []whisperSegment `json:"segments"`
	Error    string           `json:"error"`
}

// Transcribe sends audio to whisper-server.
func (w *Whisper) Transcribe(ctx context.Context, wav []byte, lang string) (Transcript, error) {
	if len(wav) > MaxWAVBytes {
		return Transcript{}, fmt.Errorf("%w: too large", ErrBadAudio)
	}
	info, err := ParseWAV(wav)
	if err != nil {
		return Transcript{}, err
	}
	if info.Seconds > w.cfg.MaxAudioSecs {
		return Transcript{}, fmt.Errorf("%w: %.1fs exceeds %.0fs", ErrBadAudio, info.Seconds, w.cfg.MaxAudioSecs)
	}
	if info.Seconds < 0.3 {
		return Transcript{NoSpeech: true, AudioSecs: info.Seconds}, nil
	}
	release, err := w.g.acquire(ctx)
	if err != nil {
		return Transcript{}, err
	}
	start := time.Now()
	tr, err := w.do(ctx, wav, lang)
	release(err)
	if err != nil {
		return Transcript{}, err
	}
	tr.AudioSecs = info.Seconds
	tr.LatencyMS = time.Since(start).Milliseconds()
	return tr, nil
}

func (w *Whisper) do(ctx context.Context, wav []byte, lang string) (Transcript, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "audio.wav")
	if err != nil {
		return Transcript{}, err
	}
	if _, err := fw.Write(wav); err != nil {
		return Transcript{}, err
	}
	if lang == "" {
		lang = "hi"
	}
	_ = mw.WriteField("language", lang)
	_ = mw.WriteField("response_format", "verbose_json")
	_ = mw.WriteField("temperature", "0.0")
	_ = mw.WriteField("temperature_inc", "0.2")
	if err := mw.Close(); err != nil {
		return Transcript{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.BaseURL+"/inference", &body)
	if err != nil {
		return Transcript{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := w.http.Do(req)
	if err != nil {
		return Transcript{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return Transcript{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Transcript{}, fmt.Errorf("ai: whisper HTTP %d: %.200s", resp.StatusCode, rb)
	}
	var wr whisperResponse
	if err := json.Unmarshal(rb, &wr); err != nil {
		return Transcript{}, fmt.Errorf("ai: whisper decode: %w", err)
	}
	if wr.Error != "" {
		return Transcript{}, fmt.Errorf("ai: whisper: %s", wr.Error)
	}
	return summarise(wr, w.cfg.NoSpeechThreshold), nil
}

// summarise turns segments into a transcript with a single confidence score.
func summarise(wr whisperResponse, noSpeech float64) Transcript {
	var parts []string
	var wsum, lsum float64
	for _, s := range wr.Segments {
		if s.NoSpeechProb > noSpeech {
			continue
		}
		t := strings.TrimSpace(s.Text)
		if t == "" {
			continue
		}
		parts = append(parts, t)
		d := s.End - s.Start
		if d <= 0 {
			d = 0.1
		}
		wsum += d
		lsum += d * s.AvgLogprob
	}
	tr := Transcript{Language: wr.Language}
	if len(wr.Segments) == 0 { // non-verbose fallback
		tr.Text = strings.Join(strings.Fields(wr.Text), " ")
		tr.Confidence = 0.5
		tr.NoSpeech = tr.Text == ""
		return tr
	}
	tr.Text = strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
	if wsum > 0 {
		tr.Confidence = math.Exp(lsum / wsum)
	}
	tr.NoSpeech = tr.Text == "" || isHallucination(tr.Text)
	return tr
}

// isHallucination catches whisper's well-known outputs on silence/noise.
func isHallucination(t string) bool {
	l := strings.ToLower(strings.Trim(t, " .।!?\"'"))
	for _, h := range []string{
		"", "thank you", "thanks for watching", "subscribe", "music", "[music]", "(music)",
		"धन्यवाद", "शुक्रिया", "सब्सक्राइब", "[संगीत]", "संगीत",
	} {
		if l == h {
			return true
		}
	}
	return false
}

// Health checks whisper-server's /health endpoint.
func (w *Whisper) Health(ctx context.Context) error {
	if w.g.open() {
		return ErrUnavailable
	}
	return getOK(ctx, w.http, w.cfg.BaseURL+"/health")
}

func getOK(ctx context.Context, c *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTTP %d", ErrUnavailable, resp.StatusCode)
	}
	return nil
}
