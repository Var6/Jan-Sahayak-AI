package config

import (
	"testing"
	"time"
)

func TestDocumentsDefaultsAndRejects(t *testing.T) {
	d := Defaults().Documents
	if !d.Enabled || d.Languages != "eng+hin" || d.OCRTimeout.D() != 15*time.Second ||
		d.MaxImageBytes != 2<<20 || d.MaxImageSide != 1600 || d.MaxOutputBytes != 64<<10 || d.MaxMemoryMB != 512 {
		t.Fatalf("documents defaults: %+v", d)
	}
	cases := map[string]string{
		"shell in languages": `{"profile":"dev-mac","documents":{"languages":"eng;rm"}}`,
		"empty languages":    `{"profile":"dev-mac","documents":{"languages":""}}`,
		"empty path":         `{"profile":"dev-mac","documents":{"tesseract_path":" "}}`,
		"timeout too long":   `{"profile":"dev-mac","documents":{"ocr_timeout":"2m"}}`,
		"image too big":      `{"profile":"dev-mac","documents":{"max_image_bytes":8388608}}`,
		"side too big":       `{"profile":"dev-mac","documents":{"max_image_side":4000}}`,
		"output too big":     `{"profile":"dev-mac","documents":{"max_output_bytes":1048576}}`,
		"memory too small":   `{"profile":"dev-mac","documents":{"max_memory_mb":16}}`,
		"write < ocr":        `{"profile":"dev-mac","documents":{"ocr_timeout":"60s"},"api":{"turn_timeout":"20s","write_timeout":"65s"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeFile(t, body), env(nil)); err == nil {
				t.Fatalf("accepted: %s", body)
			}
		})
	}
	// Disabled: the other fields are not checked.
	if _, err := Load(writeFile(t, `{"profile":"dev-mac","hal":{"button":"sim"},"knowledge":{"require_signed_bundle":false},"documents":{"enabled":false,"languages":""}}`), env(nil)); err != nil {
		t.Fatalf("disabled documents: %v", err)
	}
}
