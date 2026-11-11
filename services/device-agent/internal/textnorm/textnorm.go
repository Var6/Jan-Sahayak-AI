// Package textnorm normalises Hindi (Devanagari), Hinglish (romanised Hindi)
// and English text for MATCHING. Its output is never shown to citizens: it
// deliberately throws information away (nukta, vowel length, sibilant
// distinctions …) so that speech-recognition spelling noise and the many
// ways people write the same Hindi word collapse onto one form.
//
// Scope of the Unicode handling: the kiosk only matches Devanagari and Latin
// text, so instead of pulling in golang.org/x/text for full NFC we apply the
// Devanagari canonical (de)compositions explicitly. NFC would recompose
// न+़ → ऩ, र+़ → ऱ, ळ+़ → ऴ and decompose the composition-excluded
// U+0958–U+095F (क़ ख़ ग़ ज़ ड़ ढ़ फ़ य़); since the nukta is stripped anyway,
// mapping every one of these to its base consonant is equivalent to
// "NFC, then strip nukta" and needs no tables beyond the few below.
package textnorm

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Devanagari code points used by the folding rules.
const (
	chandrabindu = 'ँ'
	anusvara     = 'ं'
	visarga      = 'ः'
	nukta        = '़'
	virama       = '्'
	signI        = 'ि' // ि
	signII       = 'ी' // ी
	signU        = 'ु' // ु
	signUU       = 'ू' // ू
	signVocR     = 'ृ' // ृ
	signVocRR    = 'ॄ' // ॄ
	signCandraE  = 'ॅ' // ॅ
	signE        = 'े' // े
	signCandraO  = 'ॉ' // ॉ
	signAA       = 'ा' // ा
	zwnj         = '‌'
	zwj          = '‍'
)

// single-rune substitutions applied after nukta removal.
var runeMap = map[rune][]rune{
	// composition-excluded nukta letters → base consonant
	'क़': {'क'}, 'ख़': {'ख'}, 'ग़': {'ग'}, 'ज़': {'ज'},
	'ड़': {'ड'}, 'ढ़': {'ढ'}, 'फ़': {'फ'}, 'य़': {'य'},
	// NFC-composed nukta letters → base consonant
	'ऩ': {'न'}, 'ऱ': {'र'}, 'ऴ': {'ळ'},
	// vowel length folding
	'ई': {'इ'}, 'ऊ': {'उ'}, signII: {signI}, signUU: {signU},
	// vocalic R: वृद्ध / व्रिद्ध / ऋण / रिण
	'ऋ': {'र', 'ि'}, 'ॠ': {'र', 'ि'}, signVocR: {virama, 'र', signI}, signVocRR: {virama, 'र', signI},
	// candra vowels (English loan words: कॉलेज/कालेज, बैंक)
	signCandraO: {signAA}, signCandraE: {signE}, 'ऑ': {'आ'}, 'ऍ': {'ए'},
	// nasalisation
	chandrabindu: {anusvara},
	// retroflex / sibilant mergers common in speech and STT output
	'ण': {'न'}, 'श': {'स'}, 'ष': {'स'},
	// Devanagari digits → ASCII
	'०': {'0'}, '१': {'1'}, '२': {'2'}, '३': {'3'}, '४': {'4'},
	'५': {'5'}, '६': {'6'}, '७': {'7'}, '८': {'8'}, '९': {'9'},
}

// unaspirated → aspirated pairs, used to simplify geminates such as द्ध → ध.
var aspirateOf = map[rune]rune{
	'क': 'ख', 'ग': 'घ', 'च': 'छ', 'ज': 'झ', 'ट': 'ठ', 'ड': 'ढ',
	'त': 'थ', 'द': 'ध', 'प': 'फ', 'ब': 'भ',
}

// nasal consonants that are folded to anusvara before another consonant
// (पेन्शन → पेंशन, हिन्दी → हिंदी).
var nasalConsonant = map[rune]bool{'ङ': true, 'ञ': true, 'न': true, 'म': true}

// isConsonant reports whether r is a Devanagari consonant letter.
func isConsonant(r rune) bool { return r >= 'क' && r <= 'ह' }

// isDevanagari reports whether r is in the Devanagari block.
func isDevanagari(r rune) bool { return r >= 0x0900 && r <= 0x097F }

// isMark reports whether r is a Devanagari dependent sign (part of a word).
func isMark(r rune) bool {
	return (r >= 0x0900 && r <= 0x0903) || (r >= 0x093A && r <= 0x094F) ||
		(r >= 0x0951 && r <= 0x0957) || r == 0x0962 || r == 0x0963
}

// MaxFoldInputBytes bounds the work Fold does; longer input is truncated
// (callers cap citizen input at a few hundred runes anyway).
const MaxFoldInputBytes = 16 << 10

// Fold returns the matching form of s: lowercase Latin, Devanagari folded as
// described in the package comment, punctuation (including danda) removed,
// whitespace collapsed to single spaces.
func Fold(s string) string {
	if len(s) > MaxFoldInputBytes {
		s = s[:MaxFoldInputBytes]
		for !utf8.ValidString(s) && len(s) > 0 {
			s = s[:len(s)-1]
		}
	}
	// Pass 1: rune substitutions and character classes.
	out := make([]rune, 0, len(s))
	space := func() {
		if len(out) > 0 && out[len(out)-1] != ' ' {
			out = append(out, ' ')
		}
	}
	for _, r := range s {
		r = canonRune(r)
		switch {
		case r < 0:
			continue // format / zero-width character: removed, never a word break
		case r == nukta || r == zwj || r == zwnj || r == '॑' || r == '॒':
			continue // stripped
		case r == '।' || r == '॥':
			space()
		case isDevanagari(r):
			if m, ok := runeMap[r]; ok {
				out = append(out, m...)
			} else if isConsonant(r) || isMark(r) || (r >= 'ऄ' && r <= 'औ') || r == 'ॐ' {
				out = append(out, r)
			} else {
				space()
			}
		case r < utf8.RuneSelf:
			switch {
			case r >= 'A' && r <= 'Z':
				out = append(out, r+('a'-'A'))
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				out = append(out, r)
			default:
				space()
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			out = append(out, unicode.ToLower(r))
		case unicode.Is(unicode.Mn, r):
			continue // stray combining marks (e.g. Latin diacritics)
		default:
			space()
		}
	}
	// Pass 2: context rules on the folded runes.
	res := make([]rune, 0, len(out))
	for i := 0; i < len(out); i++ {
		r := out[i]
		// consonant + virama + consonant
		if isConsonant(r) && i+2 < len(out) && out[i+1] == virama && isConsonant(out[i+2]) {
			next := out[i+2]
			switch {
			case nasalConsonant[r]:
				// न्श → ंश (homorganic nasal before consonant)
				res = append(res, anusvara)
				i++ // skip virama; next consonant handled on next loop
				continue
			case next == r || aspirateOf[r] == next:
				// geminate: त्त → त, द्ध → ध
				i++ // drop r and virama; keep next
				continue
			}
		}
		// anusvara + nasal-consonant sequences already folded; drop doubled anusvara
		if r == anusvara && len(res) > 0 && res[len(res)-1] == anusvara {
			continue
		}
		res = append(res, r)
	}
	return strings.TrimSpace(string(res))
}

// Tokens returns the whitespace tokens of Fold(s).
func Tokens(s string) []string {
	f := Fold(s)
	if f == "" {
		return nil
	}
	return strings.Fields(f)
}

// Bigrams returns adjacent token pairs joined by a space.
func Bigrams(toks []string) []string {
	if len(toks) < 2 {
		return nil
	}
	out := make([]string, 0, len(toks)-1)
	for i := 0; i+1 < len(toks); i++ {
		out = append(out, toks[i]+" "+toks[i+1])
	}
	return out
}

// NGrams returns all n-grams of length 1..n joined by spaces.
func NGrams(toks []string, n int) []string {
	var out []string
	for size := 1; size <= n; size++ {
		for i := 0; i+size <= len(toks); i++ {
			out = append(out, strings.Join(toks[i:i+size], " "))
		}
	}
	return out
}

// DevanagariRatio returns the fraction of letters in s that are Devanagari
// (0 when s has no letters). Used by the response validator.
func DevanagariRatio(s string) float64 {
	var dev, letters int
	for _, r := range s {
		if isDevanagari(r) && (isConsonant(r) || (r >= 'ऄ' && r <= 'औ') || isMark(r)) {
			dev++
			letters++
		} else if unicode.IsLetter(r) {
			letters++
		}
	}
	if letters == 0 {
		return 0
	}
	return float64(dev) / float64(letters)
}

// stopwords are dropped before search (folded forms). They are function
// words that carry no topic.
var stopwords = map[string]bool{}

func init() {
	for _, w := range []string{
		"मुझे", "मुझी", "मुजे", "मेरा", "मेरी", "मेरे", "हम", "हमें", "मैं", "मै", "है", "हैं", "हूं", "हु", "था", "थी",
		"का", "की", "के", "को", "में", "मे", "से", "पर", "और", "या", "भी", "तो", "ही", "कि", "क्या", "कैसे",
		"चाहिए", "चाहिये", "चाहता", "चाहती", "करना", "करनी", "करें", "कर", "दो", "दीजिए", "बताइए", "बताओ",
		"बनवाना", "बनाना", "लेना", "मिलेगा", "मिलेगी", "मिलता", "मिलती", "एक", "यह", "वह", "उन्हें", "उनको",
		"इस", "उस", "जी", "कृपया", "लिए", "लिये", "बारे", "जानकारी", "कोई", "कुछ", "अपना", "अपनी", "आप",
		"i", "me", "my", "we", "the", "a", "an", "to", "for", "of", "is", "am", "are", "want", "need", "please",
		"how", "what", "can", "get", "do", "about", "and", "or", "in", "on", "with", "mujhe", "mujhko", "mera",
		"meri", "hai", "hain", "hoon", "hu", "ka", "ki", "ke", "ko", "se", "aur", "chahiye", "karna", "kaise",
		"kya", "batao", "bataiye", "banwana", "liye", "bare", "jankari",
	} {
		stopwords[Fold(w)] = true
	}
}

// IsStopword reports whether a folded token is a function word.
func IsStopword(tok string) bool { return stopwords[tok] }

// ContentTokens returns folded tokens without stopwords.
func ContentTokens(s string) []string {
	toks := Tokens(s)
	out := toks[:0]
	for _, t := range toks {
		if !stopwords[t] {
			out = append(out, t)
		}
	}
	return out
}
