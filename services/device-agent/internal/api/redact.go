package api

import "github.com/janmanindia/jan-sahayak/services/device-agent/internal/dialogue"

// redactTranscript masks sensitive values (Aadhaar, phone, bank, OTP,
// passwords …) in what the recogniser heard before it is returned to the
// UI and shown on screen. The dialogue engine still receives the raw text
// (its own guard redacts before anything is stored or sent to the LLM).
func redactTranscript(text string) string { return dialogue.RedactSensitive(text) }
