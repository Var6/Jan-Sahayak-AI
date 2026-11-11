package dialogue

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/textnorm"
)

// sanitize removes control and format characters (except ZWJ/ZWNJ, which
// Devanagari uses), collapses whitespace and caps the input at
// MaxInputRunes. It returns the cleaned text and whether it was truncated.
// Whitespace-like controls (newline, tab) become a space; every other
// control or format character (zero-width space, bidi override, soft
// hyphen …) is REMOVED, so it cannot split a word to dodge the guards.
func sanitize(s string) (string, bool) {
	return sanitizeN(s, MaxInputRunes)
}

// cleanText is sanitize without the length cap. Crisis detection runs on
// it so that a crisis phrase after the cap is still seen.
func cleanText(s string) string {
	out, _ := sanitizeN(s, maxCleanRunes)
	return out
}

// maxCleanRunes bounds cleanText (the API caps request bodies; this is a
// second, generous bound).
const maxCleanRunes = 16 << 10

func sanitizeN(s string, max int) (string, bool) {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, " ")
	}
	var b strings.Builder
	n := 0
	truncated := false
	lastSpace := true
	for _, r := range s {
		if n >= max {
			truncated = true
			break
		}
		switch {
		case r == '‌' || r == '‍':
			b.WriteRune(r)
		case unicode.IsSpace(r):
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
				n++
			}
			continue
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			continue // removed, not a word break
		default:
			b.WriteRune(r)
		}
		lastSpace = false
		n++
	}
	return strings.TrimSpace(b.String()), truncated
}

// --- Prompt-injection detection ---------------------------------------

// injectionRes match on lowercased raw text (English / Hinglish / markup).
var injectionRes = []*regexp.Regexp{
	regexp.MustCompile(`(ignore|disregard|forget|override|bypass)\b.{0,30}\b(previous|prior|above|earlier|all|your|the|any|these)\b.{0,20}\b(instructions?|rules?|prompts?|directions?|guidelines?|messages?)`),
	regexp.MustCompile(`\bsystem\s*(prompt|message|instructions?)\b`),
	regexp.MustCompile(`\byou\s+are\s+now\b`),
	regexp.MustCompile(`\b(act|behave|respond|talk)\s+as\b`),
	regexp.MustCompile(`\bpretend\s+(to\s+be|you|that)\b`),
	regexp.MustCompile(`\brole\s*-?\s*play`),
	regexp.MustCompile(`\b(jail\s*break|developer\s+mode|dan\s+mode|do\s+anything\s+now)\b`),
	regexp.MustCompile(`\b(reveal|show|print|repeat|tell\s+me|what\s+are|output)\b.{0,20}\b(your|the)\b.{0,15}\b(rules|instructions|prompt|system|guidelines|configuration|policy)\b`),
	regexp.MustCompile("```|<\\|im_start\\|>|<\\|im_end\\|>|<start_of_turn>|<end_of_turn>|\\[/?inst\\]|<</?sys>>|<\\|system\\|>|<\\|assistant\\|>"),
	regexp.MustCompile(`(^|\n|\s)(system|assistant|developer)\s*:`),
	regexp.MustCompile(`###\s*(instruction|system|response)`),
	regexp.MustCompile(`\bnew\s+(instructions?|rules?|persona)\b`),
	regexp.MustCompile(`\bfrom\s+now\s+on\b.{0,40}\b(you|act|answer|respond)\b`),
	// Hinglish
	regexp.MustCompile(`\b(pichhle|pichle|purane|sare|saare|apne)\s+(nirdesh|niyam|rules|instructions)\s+(bhool|bhul|chhod|chod|ignore)`),
	regexp.MustCompile(`\b(apne|tumhare|aapke)\s+(niyam|rules|nirdesh|instructions|prompt)\s+(batao|bataiye|dikhao)`),
	regexp.MustCompile(`\bab\s+(se\s+)?(tum|aap)\s+.{0,30}\b(ho|ban\s+jao|bano)\b`),
	regexp.MustCompile(`\b(ban\s*kar|banke)\s+(baat|jawab)\b`),
	regexp.MustCompile(`\b(kalpana|maan|man|socho|samjho)\s+(karo|kariye|kijiye|lo|lijiye|ki)?\s*(ki\s+)?(tum|aap|tu)\s+.{0,30}\b(ho|hain|hai)\b`),
	regexp.MustCompile(`\b(niyam|niyamon|rules|nirdesh)\s+(ko\s+)?(mat|na)\s+(mano|maniye|maano)\b`),
	regexp.MustCompile(`\b(nirdesh|niyam|instructions|rules)\s+(chhod|chod)\s+(do|dijiye)\b`),
	// English role-play / game framings
	regexp.MustCompile(`\blet'?s\s+play\b`),
	regexp.MustCompile(`\bplay\s+a\s+game\b`),
	regexp.MustCompile(`\byou\s+are\s+(the|a|an|my)\b.{0,40}\bnow\b`),
	regexp.MustCompile(`\b(imagine|suppose|assume)\s+(that\s+)?you\s+(are|were)\b`),
	regexp.MustCompile(`\bstay\s+in\s+character\b`),
}

// injectionPhrasesHi are matched on folded text (Devanagari).
var injectionPhrasesHi = foldAll(
	"पिछले निर्देश भूल जाओ", "पिछले निर्देश भूल", "पिछले सभी निर्देश", "निर्देश भूल जाओ", "निर्देशों को अनदेखा",
	"निर्देशों को भूल", "नियम भूल जाओ", "नियमों को भूल", "नियम तोड़", "नियमों को अनदेखा", "अपने नियम बताओ",
	"अपने नियम बताइए", "अपने निर्देश बताओ", "अपने निर्देश बताइए", "तुम्हारे नियम क्या", "आपके नियम क्या",
	"सिस्टम प्रॉम्प्ट", "प्रॉम्प्ट बताओ", "तुम अब", "अब से तुम", "अब से आप", "अब तुम एक", "का नाटक करो",
	"की तरह बात करो", "बनकर जवाब दो", "बनकर बात करो", "रोलप्ले",
	// role-play and rule-breaking phrasings (review P1)
	"की तरह जवाब दो", "की तरह जवाब दीजिए", "की तरह जवाब", "की तरह बोलो", "की तरह व्यवहार", "मान लो तुम", "मान लो कि तुम",
	"मान लीजिए आप", "मान लीजिए कि आप", "मान लीजिए तुम", "मानो तुम", "मानो कि तुम", "कल्पना करो",
	"कल्पना कीजिए", "निर्देश छोड़", "निर्देशों को छोड़", "नियम मत मान", "नियमों को मत मान",
	"नियमों का पालन मत", "नियम मत मानिए", "नियम छोड़ दो", "नियमों को छोड़", "खेल खेलते हैं", "एक खेल खेलो",
)

// detectInjection reports whether text looks like an attempt to change the
// assistant's instructions, extract its rules or smuggle in markup.
func detectInjection(raw string) bool {
	low := strings.ToLower(textnorm.Canon(raw))
	for _, re := range injectionRes {
		if re.MatchString(low) {
			return true
		}
	}
	f := " " + textnorm.Fold(raw) + " "
	for _, p := range injectionPhrasesHi {
		if strings.Contains(f, " "+p+" ") || strings.Contains(f, " "+p) {
			return true
		}
	}
	return false
}

func foldAll(xs ...string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, textnorm.Fold(x))
	}
	return out
}

// --- Sensitive data ------------------------------------------------------

var (
	// digit runs allowing single spaces / hyphens inside (Aadhaar "1234 5678 9012").
	digitRunRe = regexp.MustCompile(`\d(?:[\s\-]?\d)+`)
	ifscRe     = regexp.MustCompile(`(?i)\b[a-z]{4}0[a-z0-9]{6}\b`)
	// secret keywords followed (within a few words) by a value.
	secretKwRe = regexp.MustCompile(`(?i)(?:^|[\s,.;])(otp|pin|mpin|upi\s*pin|password|passcode|cvv|pass\s*word|पासवर्ड|ओटीपी|पिन|एमपिन|सीवीवी)(?:$|[\s,.;:=-])`)
	secretKws  = foldAll("ओटीपी", "पिन", "पासवर्ड", "एमपिन", "सीवीवी", "otp", "pin", "mpin", "password", "passcode", "cvv")
	acctKws    = foldAll("खाता संख्या", "खाता नंबर", "अकाउंट नंबर", "अकाउंट", "खाता", "account number", "account no", "account", "a/c", "ifsc", "आईएफएससी", "कार्ड नंबर", "card number", "आधार नंबर", "aadhaar number", "aadhar number", "आधार संख्या")
)

// sensitiveKind names what was detected (never the value).
type sensitiveKind string

const (
	sensNone    sensitiveKind = ""
	sensID      sensitiveKind = "id_number"   // Aadhaar-like / card / account-length digit runs
	sensSecret  sensitiveKind = "secret"      // OTP / PIN / password with a value
	sensAccount sensitiveKind = "bank_detail" // account / IFSC
	sensPhone   sensitiveKind = "phone"
)

// detectSensitive looks for personal identifiers and secrets. It returns
// the kind and a redacted copy of the text in which every PAN, IFSC code,
// digit run of 3+ digits (after spoken number words have been turned into
// digits) and value of a secret keyword is replaced by "[#]".
func detectSensitive(s string) (sensitiveKind, string) {
	kind := sensNone
	f := " " + textnorm.Fold(s) + " "
	hasSecretKw := false
	for _, k := range secretKws {
		if strings.Contains(f, " "+k+" ") {
			hasSecretKw = true
		}
	}
	hasAcctKw := false
	for _, k := range acctKws {
		if strings.Contains(f, " "+k+" ") || strings.Contains(f, " "+k) {
			hasAcctKw = true
		}
	}
	// Canonical digits/letters (fullwidth, Devanagari digits), then spoken
	// digit runs ("एक दो तीन चार …") → digits, so the digit rules see them.
	norm := numberWordsToDigits(mapDigits(textnorm.Canon(s)))
	// PAN (AAAAA9999A) and IFSC are masked whole, before the digit rule
	// would mask only their digits.
	redacted := norm
	if panRe.MatchString(redacted) {
		kind = maxKind(kind, sensID)
		redacted = panRe.ReplaceAllString(redacted, "[#]")
	}
	if ifscRe.MatchString(redacted) {
		kind = maxKind(kind, sensAccount)
		redacted = ifscRe.ReplaceAllString(redacted, "[#]")
	}
	redacted = digitRunRe.ReplaceAllStringFunc(redacted, func(run string) string {
		d := 0
		for _, r := range run {
			if r >= '0' && r <= '9' {
				d++
			}
		}
		switch {
		case d >= 12 && d <= 19:
			kind = maxKind(kind, sensID) // Aadhaar (12), card numbers (13–19)
		case d >= 9 && d <= 11:
			if d == 10 && (run[0] >= '6' && run[0] <= '9') && !hasAcctKw {
				kind = maxKind(kind, sensPhone)
			} else {
				kind = maxKind(kind, sensAccount)
			}
		case d >= 4 && (hasSecretKw || hasAcctKw):
			if hasSecretKw {
				kind = maxKind(kind, sensSecret)
			} else {
				kind = maxKind(kind, sensAccount)
			}
		case d >= 3 && hasSecretKw:
			kind = maxKind(kind, sensSecret)
		}
		if d >= 3 {
			return "[#]"
		}
		return run
	})
	// "my password is abc123" — a keyword followed by "is/है/:" and a value.
	if sm := secretKwRe.FindStringSubmatchIndex(redacted); sm != nil {
		loc := []int{sm[2], sm[3]}
		rest := strings.TrimSpace(redacted[loc[1]:])
		low := strings.ToLower(rest)
		for _, p := range []string{"is ", "है ", ": ", ":", "= ", "hai ", "- "} {
			if strings.HasPrefix(low, p) {
				val := strings.Fields(strings.TrimSpace(rest[len(p):]))
				if len(val) > 0 && val[0] != "[#]" && !isFunctionWord(val[0]) {
					kind = maxKind(kind, sensSecret)
					redacted = redacted[:loc[1]] + " [#]"
				}
				break
			}
		}
	}
	// Hindi/Hinglish word order: the value comes BEFORE है/hai
	// ("मेरा पासवर्ड गंगाजल है", "mera upi pin ganga hai").
	if r, ok := redactValueBeforeHai(redacted); ok {
		kind = maxKind(kind, sensSecret)
		redacted = r
	}
	return kind, redacted
}

// RedactSensitive returns text with personal identifiers and secrets
// (Aadhaar / card / account / phone numbers, PAN, IFSC, OTP / PIN /
// password values, digits spoken as words) replaced by "[#]". The API uses
// it on STT transcripts before they are shown or returned.
func RedactSensitive(text string) string {
	_, r := detectSensitive(cleanText(text))
	return r
}

var (
	panRe       = regexp.MustCompile(`(?i)\b[a-z]{5}[0-9]{4}[a-z]\b`)
	copulaWords = foldSet("है", "हैं", "hai", "hain", "h", "he")
	// tokens that show the words after a secret keyword are not its value
	// ("OTP नहीं आया है", "पिन कोड क्या है").
	notSecretValue = foldSet(
		"क्या", "नहीं", "नही", "ना", "भूल", "बदलना", "बदल", "गलत", "सही", "याद", "कौन", "कहाँ", "कहां", "कैसे", "कब", "नया", "पुराना",
		"आया", "आई", "आए", "आ", "मिला", "मिली", "गया", "गई", "चाहिए", "बंद", "खत्म", "कोड", "का", "की", "के", "में", "से", "पर", "को",
		"कितना", "कितने", "भेजा", "भेजो", "रहा", "रही", "नंबर",
		"what", "not", "kya", "nahi", "nahin", "na", "galat", "sahi", "yaad", "kahan", "kaha", "kaise", "kab", "naya", "purana",
		"aaya", "aya", "aayi", "ayi", "aa", "mila", "mili", "gaya", "gayi", "chahiye", "band", "khatam", "code", "ka", "ki", "ke",
		"mein", "me", "se", "par", "ko", "wrong", "forgot", "forgotten", "lost", "expired", "number", "kitna", "bheja", "raha", "rahi",
	)
	secretKwTokens = foldSet("ओटीपी", "पिन", "पासवर्ड", "एमपिन", "सीवीवी", "otp", "pin", "mpin", "password", "passcode", "passward", "cvv")
)

// redactValueBeforeHai replaces the 1–3 tokens between a secret keyword
// and a following है/hai with "[#]" when none of them is a function word.
func redactValueBeforeHai(s string) (string, bool) {
	toks := strings.Fields(s)
	changed := false
	for i := 0; i < len(toks); i++ {
		if !secretKwTokens[textnorm.Fold(toks[i])] {
			continue
		}
		for j := i + 2; j <= i+4 && j < len(toks); j++ {
			if !copulaWords[textnorm.Fold(toks[j])] {
				continue
			}
			val := toks[i+1 : j]
			ok := true
			for _, v := range val {
				fv := textnorm.Fold(v)
				if v == "[#]" || fv == "" || notSecretValue[fv] {
					ok = false
					break
				}
			}
			if ok {
				out := append(append(append([]string{}, toks[:i+1]...), "[#]"), toks[j:]...)
				toks = out
				changed = true
			}
			break
		}
	}
	if !changed {
		return s, false
	}
	return strings.Join(toks, " "), true
}

// spokenDigit maps folded digit words (Hindi, Hinglish, English) to digits.
var spokenDigit = func() map[string]byte {
	m := map[string]byte{}
	add := func(d byte, ws ...string) {
		for _, w := range ws {
			m[textnorm.Fold(w)] = d
		}
	}
	add('0', "शून्य", "जीरो", "ज़ीरो", "zero", "shunya", "sunya", "shoonya")
	add('1', "एक", "ek", "one")
	add('2', "दो", "do", "two")
	add('3', "तीन", "teen", "tin", "three")
	add('4', "चार", "char", "chaar", "four")
	add('5', "पाँच", "पांच", "panch", "paanch", "five")
	add('6', "छह", "छः", "छे", "छै", "chhe", "chhah", "chah", "che", "six")
	add('7', "सात", "saat", "sat", "seven")
	add('8', "आठ", "aath", "ath", "eight")
	add('9', "नौ", "nau", "nine")
	return m
}()

// minSpokenDigitRun: runs of at least this many digit words are converted
// ("चार पाँच दिन" stays as it is).
const minSpokenDigitRun = 4

// numberWordsToDigits replaces every run of ≥ minSpokenDigitRun spoken
// digits (single ASCII digits may be mixed in) with the digit string.
func numberWordsToDigits(s string) string {
	toks := strings.Fields(s)
	if len(toks) < minSpokenDigitRun {
		return s
	}
	digitOf := func(t string) (byte, bool) {
		if len(t) == 1 && t[0] >= '0' && t[0] <= '9' {
			return t[0], true
		}
		d, ok := spokenDigit[textnorm.Fold(t)]
		return d, ok
	}
	var out []string
	changed := false
	for i := 0; i < len(toks); {
		j := i
		var ds []byte
		for j < len(toks) {
			d, ok := digitOf(toks[j])
			if !ok {
				break
			}
			ds = append(ds, d)
			j++
		}
		if len(ds) >= minSpokenDigitRun {
			out = append(out, string(ds))
			changed = true
			i = j
			continue
		}
		if j == i {
			j = i + 1
		}
		out = append(out, toks[i:j]...)
		i = j
	}
	if !changed {
		return s
	}
	return strings.Join(out, " ")
}

func isFunctionWord(w string) bool {
	switch strings.ToLower(strings.Trim(w, ".,!?।")) {
	case "क्या", "नहीं", "भूल", "बदलना", "what", "not", "forgotten", "lost", "kya", "nahi":
		return true
	}
	return false
}

func maxKind(a, b sensitiveKind) sensitiveKind {
	rank := map[sensitiveKind]int{sensNone: 0, sensPhone: 1, sensAccount: 2, sensID: 3, sensSecret: 4}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func mapDigits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '०' && r <= '९' {
			return '0' + (r - '०')
		}
		return r
	}, s)
}
