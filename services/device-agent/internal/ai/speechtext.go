package ai

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxTTSRunes is the longest text PrepareForTTS returns and the speech
// worker accepts in one /v1/synthesize request.
const MaxTTSRunes = 600

// maxPrepInputRunes bounds the work PrepareForTTS does on its input.
const maxPrepInputRunes = 4 * MaxTTSRunes

// PrepareForTTS rewrites Hindi display text into text a Hindi TTS voice can
// read aloud. Supertonic (like most neural TTS) mispronounces or skips
// digits, symbols and Latin acronyms, so before synthesis:
//
//   - Devanagari digits become ASCII, nukta letters are decomposed
//   - e-mail addresses → "ईमेल पता", URLs and bare domains → "वेबसाइट"
//   - ₹/Rs/INR/रु amounts → "<words> रुपये" (2-digit decimals → पैसे)
//   - dates (15/12/2026, 15 दिसंबर 2026, 1 January 1998) → spoken Hindi
//   - times (10:30, 10 AM), percentages, ranges (18-60 → अठारह से साठ)
//   - integers incl. Indian (1,20,000) and Western (120,000) grouping →
//     Hindi number words with सौ/हज़ार/लाख/करोड़; decimals → "दशमलव"
//   - long digit strings (≥ 10 digits, leading zero, phone-style groups)
//     are read digit by digit
//   - common welfare acronyms/words (PM, CSC, RTPS, Aadhaar, OTP, KYC,
//     online, form, card, …) → Devanagari; other short all-caps
//     acronyms are spelled letter by letter
//   - markup symbols are dropped, whitespace collapsed, and the result is
//     cut to MaxTTSRunes at a sentence boundary.
//
// It never adds information: it only changes how existing text is spoken.
// The on-screen text is left untouched by the caller.
func PrepareForTTS(text string) string {
	s := text
	if utf8.RuneCountInString(s) > maxPrepInputRunes {
		s = string([]rune(s)[:maxPrepInputRunes])
	}
	s = strings.ToValidUTF8(s, " ")
	s = strings.Map(normaliseRune, s)
	s = decomposeNukta(s)

	s = reEmail.ReplaceAllString(s, " ईमेल पता ")
	s = reURL.ReplaceAllStringFunc(s, keepTrailingPunct("वेबसाइट"))
	s = reDomain.ReplaceAllStringFunc(s, keepTrailingPunct("वेबसाइट"))

	s = reCurrencyPrefix.ReplaceAllStringFunc(s, currencyPrefix)
	s = reCurrencySuffixDev.ReplaceAllStringFunc(s, currencySuffixDev)
	s = reCurrencySuffixLat.ReplaceAllStringFunc(s, currencySuffixLat)
	s = reSlashDash.ReplaceAllString(s, "$1")

	s = reNumericDate.ReplaceAllStringFunc(s, numericDate)
	s = reDayMonth.ReplaceAllStringFunc(s, dayMonth)
	s = reMonthDay.ReplaceAllStringFunc(s, monthDay)
	s = reMonthYear.ReplaceAllStringFunc(s, monthYear)
	s = reYearContext.ReplaceAllStringFunc(s, yearContext)

	s = reClock.ReplaceAllStringFunc(s, clock)
	s = reHourAMPM.ReplaceAllStringFunc(s, hourAMPM)
	s = rePercent.ReplaceAllStringFunc(s, percent)
	s = reHyphenNums.ReplaceAllStringFunc(s, hyphenNums)
	s = reNumber.ReplaceAllStringFunc(s, number)

	s = reEKYC.ReplaceAllString(s, "ई-केवाईसी")
	s = reLatinWord.ReplaceAllStringFunc(s, latinWord)
	s = reAcronym.ReplaceAllStringFunc(s, acronym)

	s = strings.ReplaceAll(s, "&", " और ")
	s = reDropSymbols.ReplaceAllString(s, " ")
	s = reNewline.ReplaceAllString(s, "$1। ")
	s = reSpaces.ReplaceAllString(s, " ")
	s = reSpaceBeforePunct.ReplaceAllString(s, "$1")
	s = strings.TrimSpace(s)
	return truncateSpeech(s, MaxTTSRunes)
}

// ---------------------------------------------------------------- numbers

var hindiOnes = [100]string{
	"शून्य", "एक", "दो", "तीन", "चार", "पाँच", "छह", "सात", "आठ", "नौ",
	"दस", "ग्यारह", "बारह", "तेरह", "चौदह", "पंद्रह", "सोलह", "सत्रह", "अठारह", "उन्नीस",
	"बीस", "इक्कीस", "बाईस", "तेईस", "चौबीस", "पच्चीस", "छब्बीस", "सत्ताईस", "अट्ठाईस", "उनतीस",
	"तीस", "इकतीस", "बत्तीस", "तैंतीस", "चौंतीस", "पैंतीस", "छत्तीस", "सैंतीस", "अड़तीस", "उनतालीस",
	"चालीस", "इकतालीस", "बयालीस", "तैंतालीस", "चवालीस", "पैंतालीस", "छियालीस", "सैंतालीस", "अड़तालीस", "उनचास",
	"पचास", "इक्यावन", "बावन", "तिरपन", "चौवन", "पचपन", "छप्पन", "सत्तावन", "अट्ठावन", "उनसठ",
	"साठ", "इकसठ", "बासठ", "तिरसठ", "चौंसठ", "पैंसठ", "छियासठ", "सड़सठ", "अड़सठ", "उनहत्तर",
	"सत्तर", "इकहत्तर", "बहत्तर", "तिहत्तर", "चौहत्तर", "पचहत्तर", "छिहत्तर", "सतहत्तर", "अठहत्तर", "उन्यासी",
	"अस्सी", "इक्यासी", "बयासी", "तिरासी", "चौरासी", "पचासी", "छियासी", "सत्तासी", "अट्ठासी", "नवासी",
	"नब्बे", "इक्यानवे", "बानवे", "तिरानवे", "चौरानवे", "पंचानवे", "छियानवे", "सत्तानवे", "अट्ठानवे", "निन्यानवे",
}

// Scale words, written with a decomposed nukta (U+093C) — the NFC form.
const (
	wSau    = "सौ"
	wHazaar = "हज़ार"
	wLakh   = "लाख"
	wCrore  = "करोड़"
)

func init() {
	for i, w := range hindiOnes {
		hindiOnes[i] = decomposeNukta(w)
	}
}

// HindiNumber spells n in Hindi using the Indian system
// (e.g. 120000 → "एक लाख बीस हज़ार"). Amounts of 100 crore and above
// recurse on the crore count ("एक सौ पचास करोड़").
func HindiNumber(n uint64) string {
	if n == 0 {
		return hindiOnes[0]
	}
	var parts []string
	if n >= 10000000 {
		parts = append(parts, HindiNumber(n/10000000), wCrore)
		n %= 10000000
	}
	if n >= 100000 {
		parts = append(parts, hindiOnes[n/100000], wLakh)
		n %= 100000
	}
	if n >= 1000 {
		parts = append(parts, hindiOnes[n/1000], wHazaar)
		n %= 1000
	}
	if n >= 100 {
		parts = append(parts, hindiOnes[n/100], wSau)
		n %= 100
	}
	if n > 0 {
		parts = append(parts, hindiOnes[n])
	}
	return strings.Join(parts, " ")
}

// hindiYear reads years 1100–1999 the spoken way ("उन्नीस सौ अट्ठानवे");
// others as ordinary numbers (2026 → "दो हज़ार छब्बीस").
func hindiYear(y int) string {
	if y >= 1100 && y <= 1999 {
		s := hindiOnes[y/100] + " " + wSau
		if y%100 != 0 {
			s += " " + hindiOnes[y%100]
		}
		return s
	}
	if y < 0 {
		return digitsOneByOne(strconv.Itoa(y))
	}
	return HindiNumber(uint64(y))
}

func digitsOneByOne(s string) string {
	var parts []string
	for _, r := range s {
		if r >= '0' && r <= '9' {
			parts = append(parts, hindiOnes[r-'0'])
		}
	}
	return strings.Join(parts, " ")
}

var (
	reIndianGroup  = regexp.MustCompile(`^\d{1,2}(?:,\d{2})*,\d{3}$`)
	reWesternGroup = regexp.MustCompile(`^\d{1,3}(?:,\d{3})+$`)
)

// intWords converts a run of ASCII digits (no separators).
func intWords(d string) string {
	if d == "" {
		return ""
	}
	if len(d) > 1 && d[0] == '0' || len(d) >= 10 {
		return digitsOneByOne(d) // codes, phone and account numbers
	}
	n, err := strconv.ParseUint(d, 10, 64)
	if err != nil {
		return digitsOneByOne(d)
	}
	return HindiNumber(n)
}

// groupedWords converts "1,20,000"/"120,000"/"5" (with valid grouping) and
// reports false when the commas are not thousands separators.
func groupedWords(s string) (string, bool) {
	if !strings.Contains(s, ",") {
		return intWords(s), true
	}
	if !reIndianGroup.MatchString(s) && !reWesternGroup.MatchString(s) {
		return "", false
	}
	d := strings.ReplaceAll(s, ",", "")
	if len(d) > 15 {
		return digitsOneByOne(d), true
	}
	n, err := strconv.ParseUint(d, 10, 64)
	if err != nil {
		return digitsOneByOne(d), true
	}
	return HindiNumber(n), true
}

// numberWords converts "1,20,000.75" style tokens. Invalid comma groups are
// read as a list ("1,2,3" → "एक, दो, तीन").
func numberWords(tok string) string {
	ip, frac, _ := strings.Cut(tok, ".")
	w, ok := groupedWords(ip)
	if !ok {
		parts := strings.Split(ip, ",")
		ws := make([]string, len(parts))
		for i, p := range parts {
			ws[i] = intWords(p)
		}
		w = strings.Join(ws, ", ")
	}
	if frac != "" {
		w += " दशमलव " + digitsOneByOne(frac)
	}
	return w
}

const numPat = `\d+(?:,\d+)*(?:\.\d+)?`

var (
	reNumber     = regexp.MustCompile(numPat + `(?:(?:st|nd|rd|th)\b)?`)
	rePercent    = regexp.MustCompile(`(` + numPat + `)\s*%`)
	reHyphenNums = regexp.MustCompile(`\b\d+(?:-\d+)+\b`)
)

// number is not padded with spaces so Hindi suffixes stay attached
// ("10वीं" → "दसवीं").
func number(m string) string {
	m = strings.TrimRight(m, "stndrh") // ordinal suffix → cardinal
	return numberWords(m)
}

func percent(m string) string {
	n := rePercent.FindStringSubmatch(m)
	return " " + numberWords(n[1]) + " प्रतिशत "
}

// hyphenNums: "18-60" → "अठारह से साठ"; phone-like groups digit by digit.
func hyphenNums(m string) string {
	groups := strings.Split(m, "-")
	total := 0
	for _, g := range groups {
		total += len(g)
	}
	if len(groups) == 2 && len(groups[0]) <= 3 && len(groups[1]) <= 3 {
		return " " + intWords(groups[0]) + " से " + intWords(groups[1]) + " "
	}
	ws := make([]string, len(groups))
	for i, g := range groups {
		if total >= 7 || len(groups) > 2 {
			ws[i] = digitsOneByOne(g)
		} else {
			ws[i] = intWords(g)
		}
	}
	return " " + strings.Join(ws, ", ") + " "
}

// --------------------------------------------------------------- currency

var (
	scaleAlt = `लाख|करोड\x{093C}|हज\x{093C}ार|हजार|(?i:lakhs?|crores?|thousand)`

	reCurrencyPrefix = regexp.MustCompile(
		`(?:₹|\b(?i:rs)\.?|\b(?i:inr)\.?|रु\.?|रू\.?)\s*(` + numPat + `)(?:\s*/-)?(?:\s*(` + scaleAlt + `))?`)
	reCurrencySuffixDev = regexp.MustCompile(`(` + numPat + `)\s*(?:/-\s*)?(?:रु|रू)\.?([^\p{L}\p{M}]|$)`)
	reCurrencySuffixLat = regexp.MustCompile(`(` + numPat + `)\s*(?:/-\s*)?(?i:rs)\b\.?`)
	reSlashDash         = regexp.MustCompile(`(\d)\s*/-`)
)

func scaleWord(s string) string {
	switch strings.ToLower(s) {
	case "lakh", "lakhs", wLakh:
		return wLakh
	case "crore", "crores", wCrore:
		return wCrore
	default: // thousand, हजार, हज़ार
		return wHazaar
	}
}

func rupees(amount, scale string) string {
	ip, frac, _ := strings.Cut(amount, ".")
	if scale == "" && len(frac) == 2 {
		w, ok := groupedWords(ip)
		if !ok {
			return numberWords(amount) + " रुपये"
		}
		out := w + " रुपये"
		if p, _ := strconv.Atoi(frac); p > 0 {
			out += " " + HindiNumber(uint64(p)) + " पैसे"
		}
		return out
	}
	w := numberWords(amount)
	if scale != "" {
		w += " " + scaleWord(scale)
	}
	return w + " रुपये"
}

func currencyPrefix(m string) string {
	g := reCurrencyPrefix.FindStringSubmatch(m)
	return " " + rupees(g[1], g[2]) + " "
}

func currencySuffixDev(m string) string {
	g := reCurrencySuffixDev.FindStringSubmatch(m)
	return " " + rupees(g[1], "") + " " + g[2]
}

func currencySuffixLat(m string) string {
	g := reCurrencySuffixLat.FindStringSubmatch(m)
	return " " + rupees(g[1], "") + " "
}

// ------------------------------------------------------------------ dates

var hindiMonths = [12]string{
	"जनवरी", "फ़रवरी", "मार्च", "अप्रैल", "मई", "जून",
	"जुलाई", "अगस्त", "सितंबर", "अक्टूबर", "नवंबर", "दिसंबर",
}

// monthVariants maps spellings (lower-case for Latin, decomposed nukta)
// to a month index.
var monthVariants = map[string]int{
	"जनवरी": 0, "फरवरी": 1, "फ़रवरी": 1, "मार्च": 2, "अप्रैल": 3, "अप्रेल": 3, "मई": 4, "जून": 5,
	"जुलाई": 6, "अगस्त": 7, "सितंबर": 8, "सितम्बर": 8, "अक्टूबर": 9, "अक्तूबर": 9,
	"नवंबर": 10, "नवम्बर": 10, "दिसंबर": 11, "दिसम्बर": 11,
	"january": 0, "jan": 0, "february": 1, "feb": 1, "march": 2, "mar": 2, "april": 3, "apr": 3,
	"may": 4, "june": 5, "jun": 5, "july": 6, "jul": 6, "august": 7, "aug": 7,
	"september": 8, "sept": 8, "sep": 8, "october": 9, "oct": 9, "november": 10, "nov": 10,
	"december": 11, "dec": 11,
}

const (
	hiMonthAlt = `जनवरी|फ\x{093C}?रवरी|मार्च|अप्रैल|अप्रेल|मई|जून|जुलाई|अगस्त|सितंबर|सितम्बर|अक्टूबर|अक्तूबर|नवंबर|नवम्बर|दिसंबर|दिसम्बर`
	enMonthAlt = `(?i:january|february|march|april|may|june|july|august|september|october|november|december|jan|feb|mar|apr|jun|jul|aug|sept|sep|oct|nov|dec)\b\.?`
	monthAlt   = `(?:` + hiMonthAlt + `|` + enMonthAlt + `)`
)

var (
	reNumericDate = regexp.MustCompile(`\b(\d{1,2})[/.-](\d{1,2})[/.-](\d{4})\b`)
	reDayMonth    = regexp.MustCompile(`\b(\d{1,2})(?:st|nd|rd|th)?[\s-]*(` + monthAlt + `)(?:,?\s*(\d{4})\b)?`)
	reMonthDay    = regexp.MustCompile(`(` + enMonthAlt + `)\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(\d{4})\b`)
	reMonthYear   = regexp.MustCompile(`(` + monthAlt + `)[\s,]+(\d{4})\b`)
	reYearContext = regexp.MustCompile(`(सन्|सन|वर्ष|साल)\s*(\d{4})\b`)
)

func monthIndex(s string) (int, bool) {
	s = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	i, ok := monthVariants[s]
	return i, ok
}

func dateWords(day, month int, year string) string {
	out := hindiOnes[day] + " " + hindiMonths[month]
	if year != "" {
		y, _ := strconv.Atoi(year)
		out += " " + hindiYear(y)
	}
	return out
}

func numericDate(m string) string {
	g := reNumericDate.FindStringSubmatch(m)
	d, _ := strconv.Atoi(g[1])
	mo, _ := strconv.Atoi(g[2])
	if d < 1 || d > 31 || mo < 1 || mo > 12 {
		return m
	}
	return " " + dateWords(d, mo-1, g[3]) + " "
}

func dayMonth(m string) string {
	g := reDayMonth.FindStringSubmatch(m)
	d, _ := strconv.Atoi(g[1])
	mi, ok := monthIndex(g[2])
	if !ok || d < 1 || d > 31 {
		return m
	}
	return " " + dateWords(d, mi, g[3]) + " "
}

func monthDay(m string) string {
	g := reMonthDay.FindStringSubmatch(m)
	d, _ := strconv.Atoi(g[2])
	mi, ok := monthIndex(g[1])
	if !ok || d < 1 || d > 31 {
		return m
	}
	return " " + dateWords(d, mi, g[3]) + " "
}

func monthYear(m string) string {
	g := reMonthYear.FindStringSubmatch(m)
	mi, ok := monthIndex(g[1])
	if !ok {
		return m
	}
	y, _ := strconv.Atoi(g[2])
	return " " + hindiMonths[mi] + " " + hindiYear(y) + " "
}

func yearContext(m string) string {
	g := reYearContext.FindStringSubmatch(m)
	y, _ := strconv.Atoi(g[2])
	return g[1] + " " + hindiYear(y) + " "
}

// ------------------------------------------------------------------ times

var (
	reClock    = regexp.MustCompile(`\b([01]?\d|2[0-3]):([0-5]\d)\b(?:\s*((?i:a\.?m\.?|p\.?m\.?)))?`)
	reHourAMPM = regexp.MustCompile(`\b(1[0-2]|0?[1-9])\s*((?i:a\.m\.|p\.m\.|am\b|pm\b))`)
)

func dayPeriod(h int, ampm string) (int, string) {
	pm := strings.HasPrefix(strings.ToLower(ampm), "p")
	if h == 0 {
		h = 12
	}
	switch {
	case !pm && h == 12, !pm && h <= 4:
		return h, "रात"
	case !pm:
		return h, "सुबह"
	case h == 12 || h <= 3:
		return h, "दोपहर"
	case h <= 7:
		return h, "शाम"
	default:
		return h, "रात"
	}
}

func clockWords(h, m int, ampm string) string {
	prefix := ""
	if ampm != "" && h <= 12 {
		var p string
		h, p = dayPeriod(h, ampm)
		prefix = p + " "
	}
	if m == 0 {
		return prefix + hindiOnes[h] + " बजे"
	}
	return prefix + hindiOnes[h] + " बजकर " + hindiOnes[m] + " मिनट"
}

func clock(m string) string {
	g := reClock.FindStringSubmatch(m)
	h, _ := strconv.Atoi(g[1])
	mi, _ := strconv.Atoi(g[2])
	return " " + clockWords(h, mi, g[3]) + " "
}

func hourAMPM(m string) string {
	g := reHourAMPM.FindStringSubmatch(m)
	h, _ := strconv.Atoi(g[1])
	return " " + clockWords(h, 0, g[2]) + " "
}

// ---------------------------------------------------------- URLs / e-mail

var (
	reEmail  = regexp.MustCompile(`(?i)\b[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}\b`)
	reURL    = regexp.MustCompile(`(?i)\b(?:https?://|www\.)\S+`)
	reDomain = regexp.MustCompile(`(?i)\b[a-z0-9][a-z0-9-]*(?:\.[a-z0-9-]+)*\.(?:gov\.in|nic\.in|co\.in|org\.in|ac\.in|in|com|org|net|info)\b(?:/\S*)?`)
)

// keepTrailingPunct replaces a match but keeps sentence punctuation that
// the greedy \S+ swallowed.
func keepTrailingPunct(repl string) func(string) string {
	return func(m string) string {
		trail := ""
		for {
			r, size := utf8.DecodeLastRuneInString(m)
			if size == 0 || !strings.ContainsRune(".,;:!?।)\"'", r) {
				break
			}
			trail = string(r) + trail
			m = m[:len(m)-size]
		}
		return " " + repl + trail + " "
	}
}

// ------------------------------------------------------- Latin words/caps

// latinWords are case-insensitive Latin words common in welfare speech.
var latinWords = map[string]string{
	"aadhaar": "आधार", "aadhar": "आधार", "adhaar": "आधार", "adhar": "आधार",
	"online": "ऑनलाइन", "offline": "ऑफ़लाइन",
	"form": "फ़ॉर्म", "forms": "फ़ॉर्म",
	"card": "कार्ड", "cards": "कार्ड",
	"otp": "ओटीपी", "atm": "एटीएम", "sms": "एसएमएस", "kyc": "केवाईसी",
	"csc": "सीएससी", "rtps": "आरटीपीएस", "bpl": "बीपीएल", "apl": "एपीएल",
	"obc": "ओबीसी", "ews": "ईडब्ल्यूएस",
	"mobile": "मोबाइल", "bank": "बैंक", "website": "वेबसाइट", "portal": "पोर्टल",
	"app": "ऐप", "email": "ईमेल", "e-mail": "ईमेल", "link": "लिंक",
	"helpline": "हेल्पलाइन", "office": "ऑफ़िस", "number": "नंबर",
	"upload": "अपलोड", "download": "डाउनलोड", "status": "स्टेटस",
	"certificate": "सर्टिफ़िकेट", "kisan": "किसान", "pension": "पेंशन",
}

// upperAcronyms match only in upper case ("PM", not "pm").
var upperAcronyms = map[string]string{
	"PM": "पीएम", "SC": "एससी", "ST": "एसटी", "ID": "आईडी", "PAN": "पैन",
	"UPI": "यूपीआई", "IFSC": "आईएफ़एससी", "DBT": "डीबीटी", "PDF": "पीडीएफ़",
}

var letterNames = map[rune]string{
	'A': "ए", 'B': "बी", 'C': "सी", 'D': "डी", 'E': "ई", 'F': "एफ़", 'G': "जी",
	'H': "एच", 'I': "आई", 'J': "जे", 'K': "के", 'L': "एल", 'M': "एम", 'N': "एन",
	'O': "ओ", 'P': "पी", 'Q': "क्यू", 'R': "आर", 'S': "एस", 'T': "टी", 'U': "यू",
	'V': "वी", 'W': "डब्ल्यू", 'X': "एक्स", 'Y': "वाई", 'Z': "ज़ेड",
}

var (
	reEKYC      = regexp.MustCompile(`(?i)\be-?kyc\b`)
	reLatinWord = regexp.MustCompile(`(?i)\b(?:aadhaar|aadhar|adhaar|adhar|online|offline|forms?|cards?|otp|atm|sms|kyc|csc|rtps|bpl|apl|obc|ews|mobile|bank|website|portal|app|e-?mail|link|helpline|office|number|upload|download|status|certificate|kisan|pension)\b`)
	reAcronym   = regexp.MustCompile(`\b[A-Z]{2,5}\b`)
)

func latinWord(m string) string {
	if w, ok := latinWords[strings.ToLower(m)]; ok {
		return w
	}
	return m
}

// acronym maps known upper-case acronyms, and spells other short ones
// letter by letter when they look like initialisms (≤ 4 letters, or no
// vowel). Longer pronounceable caps words (e.g. "KISAN") are left alone.
func acronym(m string) string {
	if w, ok := upperAcronyms[m]; ok {
		return w
	}
	if len(m) > 4 && strings.ContainsAny(m, "AEIOU") {
		return m
	}
	var b strings.Builder
	for _, r := range m {
		b.WriteString(letterNames[r])
	}
	return b.String()
}

// ------------------------------------------------------------ clean-up

var (
	reDropSymbols      = regexp.MustCompile("[*#_~^|<>\\[\\]{}()\"`•=+\\\\/]+")
	reNewline          = regexp.MustCompile(`([^\s।.?!,:;])[ \t]*\n\s*`)
	reSpaces           = regexp.MustCompile(`\s+`)
	reSpaceBeforePunct = regexp.MustCompile(`\s+([,।.?!;:])`)
)

func normaliseRune(r rune) rune {
	switch {
	case r >= '०' && r <= '९':
		return '0' + (r - '०')
	case r == '\u200b' || r == '\ufeff': // zero-width space, BOM
		return -1
	case r == '\n':
		return r
	case r == '\u00a0' || r == '\u2009' || r == '\u202f' || r == '\t' || r == '\r':
		return ' '
	case r == '\u2013' || r == '\u2014' || r == '\u2212': // en/em dash, minus
		return '-'
	case r == '\u2018' || r == '\u2019':
		return '\''
	case r == '\u201c' || r == '\u201d':
		return '"'
	case unicode.IsControl(r):
		return ' '
	}
	return r
}

// decomposeNukta rewrites precomposed nukta consonants (U+0958–U+095F,
// composition exclusions in Unicode) as base + U+093C, i.e. their NFC form.
func decomposeNukta(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r >= 0x0958 && r <= 0x095F }) {
		return s
	}
	base := [8]rune{'क', 'ख', 'ग', 'ज', 'ड', 'ढ', 'फ', 'य'}
	var b strings.Builder
	for _, r := range s {
		if r >= 0x0958 && r <= 0x095F {
			b.WriteRune(base[r-0x0958])
			b.WriteRune('़')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// truncateSpeech cuts s to at most max runes, preferring the last sentence
// end in the second half of the window, then the last space.
func truncateSpeech(s string, max int) string {
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	cut := -1
	for i := max - 1; i >= max/2; i-- {
		if strings.ContainsRune("।.?!", rs[i]) {
			cut = i + 1
			break
		}
	}
	if cut < 0 {
		for i := max; i >= max/2; i-- {
			if unicode.IsSpace(rs[i]) {
				cut = i
				break
			}
		}
	}
	if cut < 0 {
		cut = max
	}
	return strings.TrimSpace(string(rs[:cut]))
}
