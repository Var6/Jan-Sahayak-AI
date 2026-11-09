package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/janmanindia/jan-sahayak/services/central-api/internal/core"
)

// maxAdminBody bounds admin JSON request bodies (scheme records included).
const maxAdminBody = 256 << 10

type apiError struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Details []string `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":{"code":"internal","message":"encode"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeError(w http.ResponseWriter, status int, code, msg string, details ...string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg, Details: details}})
}

// readBody reads at most limit bytes; ok=false means a response was written.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	if r.ContentLength > limit {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "request body too large")
		return nil, false
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "bad_request", "could not read body")
		}
		return nil, false
	}
	return b, true
}

// decodeAdmin decodes a JSON admin request strictly (unknown fields refused).
func decodeAdmin(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return false
	}
	b, ok := readBody(w, r, maxAdminBody)
	if !ok {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+trunc(err.Error(), 200))
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		writeError(w, http.StatusBadRequest, "bad_request", "trailing data after JSON body")
		return false
	}
	return true
}

// writeCoreError maps core errors to HTTP responses.
func (s *Server) writeCoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, core.ErrInvalid):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, core.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, core.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		s.log.Error("admin: database error", "err", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "temporarily unavailable")
	}
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
