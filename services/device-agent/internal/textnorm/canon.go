package textnorm

import (
	"strings"
	"unicode"
)

// Canonicalisation for MATCHING ("NFKC-ish", without golang.org/x/text).
//
// Attackers (and some keyboards) write look-alike characters so that a
// blocklist regex does not fire: "ｉｇｎｏｒｅ" (fullwidth), "іgnore" (Cyrillic
// і), "ig​nore" (zero-width space). canonRune undoes the common
// cases before any matching:
//   - fullwidth ASCII (U+FF01–U+FF5E) → ASCII, ideographic space → space;
//   - format characters (Unicode category Cf: zero-width space/joiners,
//     bidi controls, soft hyphen, BOM …) are REMOVED, not turned into a
//     space, so they cannot split a word;
//   - Cyrillic and Greek letters that look like Latin letters → the Latin
//     letter (the kiosk serves Hindi/English only, so no real text is lost).
//
// The output is only ever used for matching, never shown to citizens.

// confusables maps look-alike letters to lowercase Latin.
var confusables = map[rune]rune{
	// Cyrillic
	'а': 'a', 'А': 'a', 'в': 'b', 'В': 'b', 'с': 'c', 'С': 'c', 'ԁ': 'd', 'е': 'e', 'Е': 'e', 'ё': 'e', 'Ё': 'e',
	'һ': 'h', 'Н': 'h', 'н': 'h', 'і': 'i', 'І': 'i', 'ї': 'i', 'Ї': 'i', 'ј': 'j', 'Ј': 'j', 'к': 'k', 'К': 'k',
	'ӏ': 'l', 'Ӏ': 'l', 'м': 'm', 'М': 'm', 'о': 'o', 'О': 'o', 'р': 'p', 'Р': 'p', 'ԛ': 'q', 'г': 'r',
	'ѕ': 's', 'Ѕ': 's', 'т': 't', 'Т': 't', 'у': 'y', 'У': 'y', 'х': 'x', 'Х': 'x', 'ԝ': 'w', 'Ԝ': 'w',
	'ь': 'b', 'п': 'n',
	// Greek
	'α': 'a', 'Α': 'a', 'β': 'b', 'Β': 'b', 'ε': 'e', 'Ε': 'e', 'η': 'n', 'Η': 'h', 'ι': 'i', 'Ι': 'i',
	'κ': 'k', 'Κ': 'k', 'Μ': 'm', 'ν': 'v', 'Ν': 'n', 'ο': 'o', 'Ο': 'o', 'ρ': 'p', 'Ρ': 'p', 'τ': 't',
	'Τ': 't', 'υ': 'u', 'χ': 'x', 'Χ': 'x', 'Υ': 'y', 'Ζ': 'z', 'ς': 's',
	// Latin look-alikes outside ASCII
	'ı': 'i', 'ℓ': 'l', 'ſ': 's',
}

// canonRune returns the canonical form of r, or -1 if r must be dropped.
func canonRune(r rune) rune {
	switch {
	case r >= 0xFF01 && r <= 0xFF5E:
		return r - 0xFEE0
	case r == 0x3000:
		return ' '
	case r == zwj || r == zwnj:
		return r // meaningful in Devanagari; Fold strips them itself
	case unicode.Is(unicode.Cf, r):
		return -1
	}
	if c, ok := confusables[r]; ok {
		return c
	}
	return r
}

// Canon applies canonRune to s (fullwidth → ASCII, format characters
// removed, Cyrillic/Greek look-alikes → Latin). Case is preserved for
// ASCII; use it before regex matching on raw text.
func Canon(s string) string {
	if len(s) > MaxFoldInputBytes {
		s = s[:MaxFoldInputBytes]
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		c := canonRune(r)
		if c < 0 || c == zwj || c == zwnj {
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}
