package docassist

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Field extraction keeps only what a citizen can confirm at a glance and
// what is not an identifier: a clearly labelled name, the issuing office
// and the issue date. Every returned string passes scrub(), which masks
// any long number that slipped through.

const (
	maxNameRunes      = 60
	maxAuthorityRunes = 80
	maxMaskedIDs      = 4
)

var (
	// "नाम: …", "Name: …", "नाम / Name : …". Anchored at the line start,
	// so "पिता का नाम:" / "Father's Name:" never match.
	reName = regexp.MustCompile(`(?i)^\s*(?:नाम|name)(?:\s*/\s*(?:नाम|name))?\s*[:：\-–]\s*(.+?)\s*$`)
	// "जारी करने वाला कार्यालय: …", "Issuing Authority: …", "Issued by: …".
	reAuthorityLabel = regexp.MustCompile(`(?i)^\s*(?:जारी\s*करने\s*वाला\s*(?:कार्यालय|प्राधिकारी)|जारीकर्ता(?:\s*कार्यालय)?|निर्गत\s*करने\s*वाला\s*कार्यालय|issuing\s*(?:authority|office)|issued\s*by)\s*[:：\-–]\s*(.+?)\s*$`)
	// An issue-date label followed by a date.
	reIssueDate = regexp.MustCompile(`(?i)(?:जारी\s*करने\s*की\s*(?:तिथि|तारीख|तारीख़)|जारी\s*होने\s*की\s*(?:तिथि|तारीख|तारीख़)|जारी\s*तिथि|निर्गत\s*(?:तिथि|दिनांक)|issue\s*date|date\s*of\s*issue|issued\s*on|दिनांक|dated)\s*[:：\-–]?\s*([0-9०-९]{1,2})\s*[/\-.]\s*([0-9०-९]{1,2})\s*[/\-.]\s*([0-9०-९]{4})`)
	// Lines that look like the issuing office when nothing is labelled.
	authorityHints = []string{
		"कार्यालय", "विभाग", "प्राधिकरण", "आयोग", "अंचल", "प्रखंड", "नगर निगम", "नगर परिषद", "समिति",
		"department", "office", "authority", "commission", "municipal", "board",
	}
	// Words that mark a date as a birth date (never reported as issue date).
	birthWords = []string{"जन्म", "birth", "dob", "d.o.b"}
	// Lines that are (part of) a person's address or relation, never the
	// issuing office, even when they mention one ("प्रखंड कार्यालय के पास").
	addressWordsHi = []string{"पता", "ग्राम", "गाँव", "गांव", "पिन", "के पास", "निकट", "मोहल्ला", "पुत्र", "पुत्री", "पत्नी", "पति"}
	reAddressEn    = regexp.MustCompile(`(?i)\b(?:address|addr|s/o|w/o|d/o|c/o|near|village|vill|pin|pincode|house|h\.?\s?no|po|ps)\b`)
	// An address label on its own line ("पता:", "Address:"): the next line
	// continues the address.
	reAddressLabel = regexp.MustCompile(`(?i)^\s*(?:पता|address)\s*[:：\-–]?\s*$`)
)

func lines(text string) []string {
	out := strings.Split(strings.ReplaceAll(text, "\r", "\n"), "\n")
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

// extractName returns a clearly labelled name, or "".
func extractName(text string) string {
	for _, l := range lines(text) {
		m := reName.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		v := collapseSpaces(m[1])
		if !plausibleName(v) {
			continue
		}
		return scrub(v)
	}
	return ""
}

// plausibleName: 2..60 runes of letters, vowel signs, spaces and dots.
func plausibleName(v string) bool {
	n := utf8.RuneCountInString(v)
	if n < 2 || n > maxNameRunes {
		return false
	}
	letters := 0
	for _, r := range v {
		switch {
		case unicode.IsLetter(r):
			letters++
		case unicode.IsMark(r), r == ' ', r == '.':
		default:
			return false
		}
	}
	return letters >= 2
}

// extractAuthority returns the issuing office: a labelled value first,
// else the first line that names an office/department/authority and is not
// part of an address. A value with more than four digits is dropped
// altogether (an office name does not need them, and digits on a card are
// most often an identifier, a phone number or a PIN code).
func extractAuthority(text string) string {
	ls := lines(text)
	for _, l := range ls {
		if m := reAuthorityLabel.FindStringSubmatch(l); m != nil {
			if v := cleanAuthority(m[1]); v != "" {
				return v
			}
		}
	}
	afterAddressLabel := false
	for _, l := range ls {
		low := strings.ToLower(l)
		prevLabel := afterAddressLabel
		afterAddressLabel = reAddressLabel.MatchString(l)
		if prevLabel || afterAddressLabel || isAddressLine(low) {
			continue
		}
		if strings.Contains(low, ":") {
			continue // a labelled field of something else
		}
		for _, h := range authorityHints {
			if strings.Contains(low, h) {
				if v := cleanAuthority(l); v != "" {
					return v
				}
				break
			}
		}
	}
	return ""
}

// isAddressLine reports whether a (lower-cased) line looks like a postal
// address or a relation ("S/O", "पुत्र") rather than an office name.
func isAddressLine(low string) bool {
	return containsAny(low, addressWordsHi) || reAddressEn.MatchString(low)
}

// maxAuthorityDigits: an issuing office with more digits is not shown.
const maxAuthorityDigits = 4

func cleanAuthority(v string) string {
	v = collapseSpaces(strings.Trim(v, " \t-–:|"))
	n := utf8.RuneCountInString(v)
	if n < 3 || n > maxAuthorityRunes {
		return ""
	}
	if countDigits(normalizeOCRDigits(v)) > maxAuthorityDigits {
		return ""
	}
	letters := 0
	for _, r := range v {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	if letters < 3 {
		return ""
	}
	return scrub(v)
}

// countDigits counts ASCII and Devanagari digits.
func countDigits(s string) int {
	n := 0
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= '०' && r <= '९') {
			n++
		}
	}
	return n
}

// extractIssueDate returns a labelled issue date as DD/MM/YYYY, or "".
func extractIssueDate(text string) string {
	for _, l := range lines(text) {
		low := strings.ToLower(l)
		birth := false
		for _, w := range birthWords {
			if strings.Contains(low, w) {
				birth = true
				break
			}
		}
		if birth {
			continue
		}
		m := reIssueDate.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		d, _ := strconv.Atoi(asciiDigits(m[1]))
		mo, _ := strconv.Atoi(asciiDigits(m[2]))
		y, _ := strconv.Atoi(asciiDigits(m[3]))
		if d < 1 || d > 31 || mo < 1 || mo > 12 || y < 1950 || y > 2100 {
			continue
		}
		return twoDigits(d) + "/" + twoDigits(mo) + "/" + strconv.Itoa(y)
	}
	return ""
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func asciiDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '०' && r <= '९' {
			r = '0' + (r - '०')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func collapseSpaces(s string) string { return strings.Join(strings.Fields(s), " ") }

// ----------------------------------------------------------------- masking

var (
	// Runs of digits, optionally grouped by single spaces or hyphens
	// ("1234 5678 9012", "1234-5678-9012", "123456789012").
	reDigitRun = regexp.MustCompile(`[0-9०-९](?:[ \-]?[0-9०-९]){5,}`)
	reDateLike = regexp.MustCompile(`^[0-9]{1,2}-[0-9]{1,2}-[0-9]{2,4}$`)
	accountCue = []string{"account", "a/c", "खाता", "acct"}
	aadhaarCue = []string{"आधार", "aadhaar", "aadhar"}
)

// maskedIDs finds identifiers in the text and returns them masked.
func maskedIDs(text string, t DocType) []MaskedID {
	var out []MaskedID
	seen := map[string]bool{}
	add := func(m MaskedID) {
		if len(out) < maxMaskedIDs && !seen[m.Value] {
			seen[m.Value] = true
			out = append(out, m)
		}
	}
	for _, l := range lines(text) {
		low := strings.ToLower(l)
		for _, m := range rePAN.FindAllString(l, -1) {
			add(MaskedID{Kind: "pan", Value: maskTail(m)})
		}
		for _, m := range reEPIC.FindAllString(l, -1) {
			add(MaskedID{Kind: "epic", Value: maskTail(m)})
		}
		for _, m := range reDigitRun.FindAllString(l, -1) {
			raw := strings.TrimSpace(asciiDigits(m))
			if reDateLike.MatchString(raw) {
				continue
			}
			digits := strings.NewReplacer(" ", "", "-", "").Replace(raw)
			n := len(digits)
			if n < 8 || n > 18 {
				continue
			}
			kind := "number"
			switch {
			case n == 12 && (t == Aadhaar || containsAny(low, aadhaarCue)):
				kind = "aadhaar"
			case n == 16 && t == Aadhaar:
				kind = "vid"
			case containsAny(low, accountCue) || t == BankPassbook:
				kind = "account"
			}
			add(MaskedID{Kind: kind, Value: maskDigits(digits, kind)})
		}
	}
	return out
}

// maskDigits keeps the last four digits. Aadhaar-style numbers keep their
// familiar grouping ("XXXX XXXX 1234").
func maskDigits(d, kind string) string {
	last := d[len(d)-4:]
	switch kind {
	case "aadhaar":
		return "XXXX XXXX " + last
	case "vid":
		return "XXXX XXXX XXXX " + last
	}
	return strings.Repeat("X", len(d)-4) + last
}

// maskTail keeps the last four characters of an alphanumeric identifier.
func maskTail(s string) string {
	if len(s) <= 4 {
		return strings.Repeat("X", len(s))
	}
	return strings.Repeat("X", len(s)-4) + s[len(s)-4:]
}

// Free-text masking (scrub) is looser than maskedIDs: any separator OCR
// may put between digit groups (spaces, dots, slashes, dashes, commas,
// colons, underscores; up to two of them) and OCR letter/digit mix-ups.
var (
	reLooseDigitRun = regexp.MustCompile(`[0-9०-९](?:[\s./\-–—_,:]{0,2}[0-9०-९]){5,}`)
	// PAN / EPIC shapes in any case, with O/I/l/| read for digits.
	reLoosePAN  = regexp.MustCompile(`(?i)\b[a-z]{5}[0-9oil|]{4}[a-z]\b`)
	reLooseEPIC = regexp.MustCompile(`(?i)\b[a-z]{3}[0-9oil|]{7}\b`)
)

// normalizeOCRDigits maps the usual OCR mix-ups (O/o → 0, I/l/| → 1)
// inside whitespace-separated tokens that already contain a digit, so
// "1234 5678 9O12" is recognised as a number. Other tokens are untouched.
func normalizeOCRDigits(s string) string {
	fs := strings.Fields(s)
	changed := false
	for i, f := range fs {
		if countDigits(f) == 0 {
			continue
		}
		g := strings.Map(func(r rune) rune {
			switch r {
			case 'O', 'o':
				return '0'
			case 'I', 'l', '|':
				return '1'
			}
			return r
		}, f)
		if g != f {
			fs[i], changed = g, true
		}
	}
	if !changed {
		return s
	}
	return strings.Join(fs, " ")
}

// maskLooseID masks a PAN/EPIC-shaped token that has at least one real
// digit in its number part (so plain words are left alone).
func maskLooseID(m string) string {
	if countDigits(m) == 0 {
		return m
	}
	return maskTail(strings.ToUpper(m))
}

// scrub masks identifiers inside a free-text field (name, office): long
// digit runs (whatever the separators) and PAN/EPIC-shaped tokens (any
// case, OCR mix-ups tolerated). Defence in depth: the extractors already
// reject names with digits and offices with more than four digits.
func scrub(s string) string {
	s = reLoosePAN.ReplaceAllStringFunc(s, maskLooseID)
	s = reLooseEPIC.ReplaceAllStringFunc(s, maskLooseID)
	s = normalizeOCRDigits(collapseSpaces(s))
	s = reLooseDigitRun.ReplaceAllStringFunc(s, func(m string) string {
		d := asciiDigits(m)
		d = strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, d)
		if len(d) < 6 {
			return m
		}
		return maskDigits(d, "number")
	})
	return s
}

func containsAny(s string, subs []string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}
