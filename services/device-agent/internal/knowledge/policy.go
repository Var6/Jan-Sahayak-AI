package knowledge

import "github.com/janmanindia/jan-sahayak/internal/scheme"

// Mode says how much of a record may be presented.
type Mode int

const (
	// ModeNameOnly: show the scheme name and the generic "verified details
	// are not available yet" message. No office names, documents, amounts,
	// URLs or helplines from the record.
	ModeNameOnly Mode = iota
	// ModeUnverifiedDev: dev-* profile with AllowUnverifiedForDev — show
	// the content under a prominent "not yet verified" disclaimer.
	ModeUnverifiedDev
	// ModeFactual: VERIFIED by a human reviewer and not overdue for review.
	ModeFactual
)

func (m Mode) String() string {
	switch m {
	case ModeFactual:
		return "factual"
	case ModeUnverifiedDev:
		return "unverified_dev"
	default:
		return "name_only"
	}
}

// Mode decides how a record may be presented right now.
func (s *Store) Mode(r scheme.Record) Mode {
	return ModeFor(s.cfg, r)
}

// ModeFor is Mode without a Store.
func ModeFor(cfg Config, r scheme.Record) Mode {
	now := cfg.Now
	if r.IsFactual(now()) {
		return ModeFactual
	}
	if cfg.UnverifiedAllowed() {
		return ModeUnverifiedDev
	}
	return ModeNameOnly
}

// Policy messages. These name only generic kinds of public offices, never
// an office taken from a record, because they are used exactly when the
// record cannot be trusted.
var (
	notVerifiedHi = "इस योजना/सेवा की सत्यापित जानकारी अभी उपलब्ध नहीं है। कृपया अपने नज़दीकी RTPS काउंटर, CSC (जन सेवा केंद्र) या प्रखंड कार्यालय पर पुष्टि करें।"
	notVerifiedEn = "Verified details are not available yet. Please confirm at your nearest RTPS counter, CSC (Common Service Centre) or block office."

	devDisclaimerHi = "ध्यान दें: यह जानकारी अभी सत्यापित नहीं है (परीक्षण मोड)। इस पर भरोसा न करें — कार्यालय में पुष्टि करें।"
	devDisclaimerEn = "WARNING: this information is NOT yet verified (test mode). Do not rely on it — confirm at the office."
)

// NotVerifiedMessage is the generic message for ModeNameOnly.
func NotVerifiedMessage(lang string) string {
	if lang == "en" {
		return notVerifiedEn
	}
	return notVerifiedHi
}

// UnverifiedDisclaimer is the disclaimer for ModeUnverifiedDev.
func UnverifiedDisclaimer(lang string) string {
	if lang == "en" {
		return devDisclaimerEn
	}
	return devDisclaimerHi
}
