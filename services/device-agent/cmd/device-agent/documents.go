package main

import (
	"context"
	"errors"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/api"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/docassist"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/health"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/knowledge"
)

// buildDocuments wires document assistance (nil, nil when disabled). The
// knowledge store decides how a scheme record may be presented, so the
// document comparison follows the same display policy as answers.
func (a *agent) buildDocuments() (*api.DocumentDeps, *docassist.Tesseract) {
	dc := a.cfg.Documents
	if !dc.Enabled {
		return nil, nil
	}
	ocr, err := docassist.NewTesseract(docassist.TesseractConfig{
		Path: dc.TesseractPath, Languages: dc.Languages, TessdataDir: dc.TessdataDir,
		Timeout: dc.OCRTimeout.D(), MaxOutputBytes: dc.MaxOutputBytes, MaxMemoryMB: dc.MaxMemoryMB,
	})
	if err != nil {
		a.log.Error("documents: OCR not configured; document check disabled", "err", err)
		return nil, nil
	}
	ks := a.ks
	return &api.DocumentDeps{
		OCR: ocr,
		Scheme: func(ctx context.Context, id string) (scheme.Record, knowledge.Mode, error) {
			rec, err := ks.Get(ctx, id)
			if err != nil {
				return scheme.Record{}, knowledge.ModeNameOnly, err
			}
			return rec, ks.Mode(rec), nil
		},
		Image:   docassist.ImageLimits{MaxBytes: dc.MaxImageBytes, MaxSide: dc.MaxImageSide},
		Timeout: dc.OCRTimeout.D() + 5*time.Second,
	}, ocr
}

// registerOCRCheck adds the "ocr" health component: tesseract --version,
// the configured languages installed, and a tiny self-test image read.
// Optional: when it fails only capabilities.camera turns off.
func (a *agent) registerOCRCheck(ocr *docassist.Tesseract) {
	if ocr == nil {
		return
	}
	a.mon.Register(health.Component{Name: api.CompOCR, Interval: a.cfg.Documents.HealthInterval.D(),
		Timeout: a.cfg.Documents.OCRTimeout.D() + 10*time.Second,
		Check: func(ctx context.Context) health.Result {
			if err := ocr.Health(ctx); err != nil {
				a.log.Warn("health: OCR check failed", "err", err)
				detail := "ocr_unavailable"
				if errors.Is(err, docassist.ErrTimeout) {
					detail = "ocr_selftest_timeout"
				}
				return health.Result{State: health.Unavailable, Detail: detail}
			}
			return health.Result{State: health.Healthy, Data: map[string]any{"selftest_ms": ocr.SelfTestDuration().Milliseconds()}}
		}})
}
