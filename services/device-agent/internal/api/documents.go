package api

// Document assistance (docs/API_DOCUMENTATION.md, "Document check"):
//
//	POST /api/v1/session/{id}/camera-consent    {consent: bool}
//	POST /api/v1/session/{id}/document          image/jpeg body (≤ 2 MiB, ≤ 1600 px)
//	POST /api/v1/session/{id}/document/confirm  {correct: bool}
//
// The camera is legitimate only after the citizen consented in this session
// (session.SetCameraConsent; reset when the session ends). A photo is read
// once in memory by the OCR subprocess and dropped (the request buffer is
// zeroed); nothing is written to disk, logged, recorded as analytics or
// sent to the central server. The UI receives the document type, a few
// confirmable fields and masked identifiers — never the OCR text. Results
// live in the session's in-RAM conversation and die with it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/docassist"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/health"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/knowledge"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/session"
)

// CompOCR is the health component of the OCR engine; capabilities.camera
// is true only while it is usable.
const CompOCR = "ocr"

// ReasonOCRFailed: the OCR process could not run or crashed.
const ReasonOCRFailed = "ocr_failed"

// maxConfirmedDocs bounds the confirmed document types of a session.
const maxConfirmedDocs = 16

// DocumentDeps wires document assistance.
type DocumentDeps struct {
	OCR docassist.OCR
	// Scheme returns a knowledge record and how it may be presented.
	Scheme func(ctx context.Context, id string) (scheme.Record, knowledge.Mode, error)
	// Image bounds the uploaded JPEG.
	Image docassist.ImageLimits
	// Timeout bounds one upload's OCR (default 20 s; the OCR runner has
	// its own, shorter, process timeout).
	Timeout time.Duration
}

// docState is a session's document check memory (guarded by Server.mu).
type docState struct {
	pending   *docassist.Result   // last photo, awaiting the citizen's yes/no
	confirmed []docassist.DocType // types the citizen confirmed
}

// DocumentJSON is what the UI may show about one photo.
type DocumentJSON struct {
	Type             docassist.DocType    `json:"doc_type"`
	Label            string               `json:"label"`
	Confidence       string               `json:"confidence"`
	Name             string               `json:"name,omitempty"`
	IssuingAuthority string               `json:"issuing_authority,omitempty"`
	IssueDate        string               `json:"issue_date,omitempty"`
	MaskedIDs        []docassist.MaskedID `json:"masked_ids,omitempty"`
}

// DocumentResult is the response of POST /document.
type DocumentResult struct {
	Session  session.Snapshot `json:"session"`
	Document DocumentJSON     `json:"document"`
	// Text asks "यह <type> लगता है — क्या यह सही है?" or, for an
	// unrecognised photo, how to take a better one.
	Text string `json:"text"`
	// CanConfirm: false for an unrecognised photo (only "retake").
	CanConfirm bool `json:"can_confirm"`
	// Notice: OCR does not verify authenticity; the photo was not kept.
	Notice string `json:"notice"`
}

// TypeLabel is a document type with its display name.
type TypeLabel struct {
	Type  docassist.DocType `json:"doc_type"`
	Label string            `json:"label"`
}

// ConfirmResult is the response of POST /document/confirm.
type ConfirmResult struct {
	Session   session.Snapshot    `json:"session"`
	Text      string              `json:"text"`
	Confirmed []TypeLabel         `json:"confirmed"`
	Coverage  *docassist.Coverage `json:"coverage,omitempty"`
	Caveat    string              `json:"caveat,omitempty"`
	Notice    string              `json:"notice"`
}

func (s *Server) registerDocumentRoutes(mux *http.ServeMux, p string) {
	mux.HandleFunc("POST "+p+"/session/{id}/camera-consent", s.handleCameraConsent)
	mux.HandleFunc("POST "+p+"/session/{id}/document", s.handleDocument)
	mux.HandleFunc("POST "+p+"/session/{id}/document/confirm", s.handleDocumentConfirm)
}

// camera: document assistance is wired and the OCR engine is healthy.
func (s *Server) camera() bool {
	return s.d.Documents != nil && s.d.Documents.OCR != nil && s.d.Health.IsUsable(CompOCR)
}

var errCameraOff = errf(http.StatusServiceUnavailable, "unavailable", "document check is unavailable on this kiosk right now")

func (s *Server) handleCameraConsent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, e := s.lookup(id)
	if e != nil {
		writeAPIErr(w, e)
		return
	}
	var body struct {
		Consent *bool `json:"consent"`
	}
	if e := s.decodeJSON(w, r, &body, false); e != nil {
		writeAPIErr(w, e)
		return
	}
	if body.Consent == nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "consent (true or false) is required")
		return
	}
	if *body.Consent && !s.camera() {
		writeAPIErr(w, errCameraOff)
		return
	}
	if !*body.Consent {
		// Withdrawing consent also forgets what was read.
		s.mu.Lock()
		c.doc = docState{}
		s.mu.Unlock()
	}
	if err := s.d.Sessions.SetCameraConsent(id, *body.Consent); err != nil {
		writeAPIErr(w, sessionErr(err))
		return
	}
	writeJSON(w, http.StatusOK, sessionResult{Session: s.d.Sessions.Current()})
}

func consentErr(err error) *apiError {
	if errors.Is(err, session.ErrNoCameraConsent) {
		return errf(http.StatusForbidden, "no_consent", "the citizen has not agreed to the document check in this session")
	}
	return sessionErr(err)
}

func (s *Server) handleDocument(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, e := s.lookup(id)
	if e != nil {
		writeAPIErr(w, e)
		return
	}
	if !s.camera() {
		writeAPIErr(w, errCameraOff)
		return
	}
	if err := s.d.Sessions.RequireCameraConsent(id); err != nil {
		writeAPIErr(w, consentErr(err))
		return
	}
	if ct := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])); ct != "image/jpeg" {
		writeErr(w, http.StatusUnsupportedMediaType, "bad_image", "Content-Type must be image/jpeg")
		return
	}
	dd := s.d.Documents
	maxBytes := dd.Image.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 2 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	img, err := io.ReadAll(r.Body) // RAM only; never written to disk
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("image exceeds %d bytes", maxBytes))
			return
		}
		writeErr(w, http.StatusBadRequest, "bad_image", "could not read the image")
		return
	}
	defer clear(img) // drop the photo's bytes as soon as we are done
	if err := docassist.CheckJPEG(img, dd.Image); err != nil {
		if errors.Is(err, docassist.ErrTooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "image too large (bytes or pixels)")
			return
		}
		writeErr(w, http.StatusBadRequest, "bad_image", "image must be a JPEG photo")
		return
	}

	// One thing at a time: no turn, greeting or other photo meanwhile.
	if !c.turnMu.TryLock() {
		writeErr(w, http.StatusConflict, "bad_state", "busy; try again after the reply")
		return
	}
	defer c.turnMu.Unlock()
	switch cur := s.d.Sessions.Current(); cur.Activity {
	case session.Ready:
	case session.Listening:
		// The UI stopped its recording to open the camera.
		if _, err := s.d.Sessions.SetActivity(id, session.Ready); err != nil {
			writeAPIErr(w, sessionErr(err))
			return
		}
	default:
		writeErr(w, http.StatusConflict, "bad_state", "cannot read a document while "+string(cur.Activity))
		return
	}
	if _, err := s.d.Sessions.SetActivity(id, session.Processing); err != nil {
		writeAPIErr(w, sessionErr(err))
		return
	}
	defer func() {
		if p := recover(); p != nil {
			s.forceReady(id)
			panic(p)
		}
	}()
	back := func() { _, _ = s.d.Sessions.SetActivity(id, session.Ready) }

	timeout := dd.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	extendDeadlines(w, timeout+5*time.Second)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	start := time.Now()
	text, err := dd.OCR.Recognize(ctx, img)
	clear(img)
	if err != nil {
		back()
		switch {
		case errors.Is(err, docassist.ErrBusy):
			writeErr(w, http.StatusServiceUnavailable, "unavailable", "the document reader is busy; please try again")
		case errors.Is(err, docassist.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
			// A hard photo, not an engine fault (the health check judges it).
			writeErr(w, http.StatusServiceUnavailable, "unavailable", "the document could not be read in time; please try again")
		case errors.Is(err, context.Canceled):
			writeErr(w, http.StatusServiceUnavailable, "unavailable", "request cancelled")
		default:
			s.d.Health.Report(CompOCR, health.Result{State: health.Unavailable, Detail: ReasonOCRFailed})
			s.log.Warn("api: OCR failed", "err", trimErr(err))
			writeAPIErr(w, errCameraOff)
		}
		return
	}
	res := docassist.Analyze(text) // the OCR text goes no further

	// Consent may have been withdrawn, or the session ended, while OCR ran:
	// then the result is discarded, not shown.
	if err := s.d.Sessions.RequireCameraConsent(id); err != nil {
		back()
		writeAPIErr(w, consentErr(err))
		return
	}
	s.mu.Lock()
	ended := c.ended
	if !ended {
		c.doc.pending = nil
		if res.Type != docassist.Unknown {
			p := res
			c.doc.pending = &p
		}
	}
	lang := c.lang
	s.mu.Unlock()
	if ended {
		writeAPIErr(w, sessionErr(session.ErrNoSession))
		return
	}
	snap, err := s.d.Sessions.SetActivity(id, session.Ready)
	if err != nil {
		writeAPIErr(w, sessionErr(err))
		return
	}
	s.log.Debug("api: document read", "type", string(res.Type), "confidence", res.Confidence,
		"ms", time.Since(start).Milliseconds())
	writeJSON(w, http.StatusOK, DocumentResult{
		Session: snap,
		Document: DocumentJSON{
			Type: res.Type, Label: docassist.Label(res.Type, lang), Confidence: res.Confidence,
			Name: res.Name, IssuingAuthority: res.Authority, IssueDate: res.IssueDate, MaskedIDs: res.MaskedIDs,
		},
		Text:       docassist.AskConfirm(res.Type, lang),
		CanConfirm: res.Type != docassist.Unknown,
		Notice:     docassist.Notice(lang),
	})
}

func (s *Server) handleDocumentConfirm(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, e := s.lookup(id)
	if e != nil {
		writeAPIErr(w, e)
		return
	}
	if s.d.Documents == nil {
		writeAPIErr(w, errCameraOff)
		return
	}
	var body struct {
		Correct *bool `json:"correct"`
	}
	if e := s.decodeJSON(w, r, &body, false); e != nil {
		writeAPIErr(w, e)
		return
	}
	if body.Correct == nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "correct (true or false) is required")
		return
	}
	if err := s.d.Sessions.RequireCameraConsent(id); err != nil {
		writeAPIErr(w, consentErr(err))
		return
	}
	// The turn lock guards the dialogue state (the scheme being discussed).
	if !c.turnMu.TryLock() {
		writeErr(w, http.StatusConflict, "bad_state", "busy; try again after the reply")
		return
	}
	schemeID := c.st.Scheme
	c.turnMu.Unlock()

	s.mu.Lock()
	p := c.doc.pending
	c.doc.pending = nil
	if p != nil && *body.Correct && !containsType(c.doc.confirmed, p.Type) && len(c.doc.confirmed) < maxConfirmedDocs {
		c.doc.confirmed = append(c.doc.confirmed, p.Type)
	}
	have := append([]docassist.DocType(nil), c.doc.confirmed...)
	lang := c.lang
	s.mu.Unlock()
	if p == nil {
		writeErr(w, http.StatusConflict, "bad_state", "no document is waiting for confirmation")
		return
	}
	_ = s.d.Sessions.Touch(id)

	res := ConfirmResult{Session: s.d.Sessions.Current(), Notice: docassist.Notice(lang), Confirmed: []TypeLabel{}}
	for _, t := range have {
		res.Confirmed = append(res.Confirmed, TypeLabel{Type: t, Label: docassist.Label(t, lang)})
	}
	if !*body.Correct {
		res.Text = docassist.RetakeText(lang)
		writeJSON(w, http.StatusOK, res)
		return
	}
	res.Text = docassist.CoverageIntro(lang)
	if schemeID == "" || s.d.Documents.Scheme == nil {
		res.Text += " " + docassist.NoSchemeText(lang)
		writeJSON(w, http.StatusOK, res)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.d.Limits.ControlTimeout)
	defer cancel()
	rec, mode, err := s.d.Documents.Scheme(ctx, schemeID)
	if err != nil {
		s.log.Warn("api: scheme lookup for document check failed", "scheme", schemeID, "err", trimErr(err))
		res.Text += " " + docassist.NoSchemeText(lang)
		writeJSON(w, http.StatusOK, res)
		return
	}
	cov := docassist.Compare(rec, mode, have, lang)
	res.Coverage = &cov
	if len(cov.Items) > 0 {
		res.Caveat = docassist.CoverageCaveat(lang)
	}
	writeJSON(w, http.StatusOK, res)
}

func containsType(ts []docassist.DocType, t docassist.DocType) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}
