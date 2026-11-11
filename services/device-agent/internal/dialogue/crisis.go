package dialogue

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/textnorm"
)

// EmergencyNumber is India's single national emergency number (Emergency
// Response Support System, Ministry of Home Affairs: police, fire,
// ambulance). It is the ONLY phone number in the code base; every other
// helpline comes from a verified data file.
//
// DOCUMENTED EXCEPTION to "facts only from verified records" (decision 10,
// awaiting human sign-off, see docs/AI_GUARDRAILS.md): a crisis reply must
// work even when no helpline file is loaded or every entry has expired, so
// 112 is hard-coded as the emergency fallback.
const EmergencyNumber = "112"

// crisisType classifies an emergency.
type crisisType string

const (
	crisisSelfHarm    crisisType = "self_harm"
	crisisViolence    crisisType = "violence"
	crisisChild       crisisType = "child_abuse"
	crisisTrafficking crisisType = "trafficking"
	crisisMedical     crisisType = "medical"
	crisisDanger      crisisType = "danger"
)

// crisisOrder is the priority order when several types match.
var crisisOrder = []crisisType{crisisSelfHarm, crisisChild, crisisTrafficking, crisisViolence, crisisMedical, crisisDanger}

// =========================================================================
// CRISIS VOCABULARY — DATA, NOT LOGIC.
//
// !!! This table MUST be reviewed and signed off by a trained counsellor /
// !!! psychosocial reviewer before field deployment, and re-reviewed when
// !!! it changes. Engineers may add entries; only the reviewer may remove
// !!! them. Keep it broad: a false positive shows helplines once; a false
// !!! negative can cost a life.
//
// Everything is matched on textnorm.Fold output (nukta, vowel length,
// श/ष/स, ण/न … folded; punctuation removed; Latin lowercased), so one
// spelling per word is usually enough. Detection runs on the FULL raw
// input, before the length cap, the STT-confidence gate, sensitive-data
// redaction and every other step.
//
// Three mechanisms:
//  1. crisisPhrases: a phrase matches when it occurs in the folded text
//     starting at a word boundary; its LAST word may be a prefix
//     ("जान दे दू" matches "जान दे दूंगा/दूंगी").
//  2. crisisPairs: a word from A and a word from B within Window tokens of
//     each other, in either order (actor/victim + violence verb, substance
//     + "drink/eat", marriage + "minor" …). Words are exact folded tokens;
//     an entry ending in "*" is a prefix; an entry with spaces is a
//     consecutive token sequence.
//  3. child marriage by age: a marriage word near an age below 18
//     ("14 साल की बेटी की शादी"), see detectChildMarriageAge.
// =========================================================================

var crisisPhrases = map[crisisType][]string{
	crisisSelfHarm: {
		// Hindi
		"आत्महत्या", "आत्म हत्या", "खुदकुशी", "जान दे दू", "जान देना चाह", "जान दे दे", "अपनी जान दे", "अपनी जान ले", "अपनी जान खत्म", "खुद की जान", "जान ले लू",
		"मर जाना चाह", "मरना चाह", "मरना है", "मर जाना है", "मर जाऊ", "मर जाउ", "मौत चाहिए", "मौत आ जाए",
		"जीना नहीं", "जीने का मन नहीं", "जीने की इच्छा नहीं", "जिंदगी से तंग", "जिंदगी खत्म", "जीवन खत्म", "जिंदगी से परेशान",
		"सब खत्म कर दू", "सब कुछ खत्म कर दू", "खुद को खत्म", "अपने आप को खत्म", "खुद को नुकसान", "खुद को मार", "अपने आप को मार",
		"फांसी", "फंदा लगा", "फंदे से", "पंखे से लटक", "जहर", "सल्फास", "नींद की गोलिया खा", "ट्रेन के आगे", "नदी में कूद", "कुएं में कूद",
		// Hinglish
		"atmhatya", "aatmhatya", "khudkushi", "khudkhushi", "suicide", "marna hai", "marna chahta", "marna chahti", "mar jana hai",
		"mar jana chahta", "mar jana chahti", "mar jaunga", "mar jaungi", "jaan de dunga", "jaan de dungi", "jaan le lunga", "jaan le lungi",
		"apni jaan de", "apni jaan le", "jina nahi", "jeena nahi", "jine ka man nahi", "jeene ka man nahi", "zindagi se tang", "zindagi khatam", "jindagi khatam",
		"khud ko khatam", "khud ko khatm", "khud ko maar", "apne aap ko khatam", "apne aap ko maar", "sab khatam kar dunga", "sab khatam kar dungi",
		"zeher", "zehar", "zahar", "jahar", "fansi", "phansi", "faansi", "sulphas", "celphos",
		// English
		"kill myself", "killing myself", "end my life", "end it all", "take my own life", "take my life", "want to die", "wanna die",
		"self harm", "hurt myself", "harm myself", "cut myself", "no reason to live", "don t want to live", "dont want to live",
		"do not want to live", "no longer want to live", "tired of living", "tired of life", "better off dead", "hang myself",
		"hanging myself", "poison myself", "drink poison", "take poison", "overdose", "jump off", "jump in front",
	},
	crisisViolence: {
		"मार डाल", "जान से मार", "मारपीट", "घरेलू हिंसा", "बलात्कार", "रेप", "छेड़छाड़", "जबरदस्ती करता", "जबरदस्ती करते", "हमला",
		"तेजाब", "हाथ उठाता", "हाथ उठाते", "गला दबा", "दहेज की मांग", "दहेज मांग",
		"maar dal", "mar dal", "jaan se maar", "maarpeet", "marpeet", "marpit", "pitai karta", "dahej mang", "dahej maang",
		"beats me", "beating me", "beat me", "hits me", "hit me", "hitting me", "kicks me", "kicked me", "slapped me", "slaps me",
		"burn me", "will kill me", "going to kill", "domestic violence", "rape", "raped", "sexual assault", "acid attack", "dowry",
	},
	crisisChild: {
		"बच्चे के साथ गलत", "बच्ची के साथ गलत", "बच्चे का शोषण", "बच्ची का शोषण", "बाल शोषण", "बाल विवाह", "बच्चे को मारता", "बच्ची को मारता",
		"बाल मजदूरी", "बच्चे से मजदूरी", "नाबालिग की शादी", "नाबालिग लड़की की शादी", "कम उम्र में शादी", "कम उम्र की शादी",
		"child abuse", "abusing my child", "abusing a child", "child marriage", "child labour", "child labor", "underage marriage", "minor marriage",
		"bachche ke sath galat", "bacchi ke sath galat", "bal vivah", "baal vivah", "kam umar mein shadi", "kam umar me shaadi",
	},
	crisisTrafficking: {
		"तस्करी", "मुझे बेच दिया", "मुझे बेच दी", "बेटी को बेच", "बेटे को बेच", "बच्चे को बेच", "बच्ची को बेच", "लड़की को बेच", "पत्नी को बेच",
		"बंधुआ", "बंधक बना", "कैद करके", "trafficking", "trafficked", "sold me", "sold my daughter", "sold my son", "sold my child", "sold my wife",
		"bonded labour", "bonded labor", "held captive", "mujhe bech diya", "beti ko bech", "bandhua", "taskari",
	},
	crisisMedical: {
		"बेहोश", "सांस नहीं", "सांस नही", "सांस रुक", "सांस नहीं ले", "दिल का दौरा", "हार्ट अटैक", "दौरा पड़", "सांप ने काट", "सांप काट",
		"सांप ने डस", "बिच्छू ने काट", "खून बह रहा", "बहुत खून", "करंट लग", "पानी में डूब", "नदी में डूब",
		"behosh", "saans nahi", "sans nahi", "saans ruk", "dil ka daura", "heart attack", "saanp ne kaat", "saap ne kaat", "sanp ne kaat",
		"khoon beh", "current lag", "pani mein doob",
		"unconscious", "not breathing", "can t breathe", "cannot breathe", "cant breathe", "snake bit", "snakebite", "snake bite",
		"bleeding heavily", "electric shock", "drowning", "stroke",
	},
	crisisDanger: {
		"बचाओ", "बचाइए", "बचा लो", "खतरे में हू", "खतरे में है", "जान खतरे में", "आग लग गई", "आग लगी है", "कोई मुझे मार", "पीछा कर रहा",
		"धमकी दे रहा", "धमकी दे रहे", "जान से मारने की धमकी",
		"save me", "i am in danger", "life is in danger", "bachao", "bacha lo", "khatre mein hu", "khatre me hu", "jaan khatre",
	},
}

// crisisPair is one co-occurrence rule (see the table comment above).
type crisisPair struct {
	Type   crisisType
	A, B   []string
	Window int // maximum token distance between the A and B words
}

// People (actors and victims) for the violence rules.
var crisisPeople = []string{
	// Hindi
	"पति", "पत्नी", "बीवी", "ससुराल", "ससुर", "सास", "देवर", "जेठ", "ननद", "पिता", "बाप", "भाई", "बेटा", "बेटे", "बेटी", "बहू", "माँ", "मां",
	"बहन", "बच्चे", "बच्ची", "बच्चा", "पड़ोसी", "मालिक", "ठेकेदार", "साहूकार", "दबंग", "गुंडे", "गुंडा", "पुलिस", "घरवाले", "घरवाला",
	"मर्द", "शराबी", "वो", "वह", "वे", "उसने", "उन्होंने", "मुझे", "मुझको", "मुझ", "हमें", "हमको",
	// Hinglish
	"pati", "patni", "biwi", "sasural", "saas", "sasur", "devar", "jeth", "papa", "baap", "bhai", "beta", "bete", "beti", "bahu",
	"maa", "behen", "bahan", "malik", "thekedar", "sahukar", "padosi", "gunde", "gharwale", "gharwala", "mujhe", "mujhko", "humein",
	"hamein", "hame", "usne", "unhone",
	// English
	"husband", "wife", "laws", "in law*", "father", "dad", "brother", "son", "daughter", "mother", "sister", "boss", "employer",
	"landlord", "neighbour", "neighbor", "he", "she", "they", "someone", "somebody", "me", "us", "him", "her",
}

// Violence verbs. Explicit forms rather than prefixes so that "मार्कशीट"
// (mark sheet) or "मर गए" (died) never match. "मारे" is left out on
// purpose: "भूख के मारे" means "because of hunger". "attack" is left out
// because "heart attack" is a medical emergency, not violence.
var crisisViolenceVerbs = []string{
	// मार (beat/kill)
	"मार", "मारा", "मारी", "मारता", "मारती", "मारते", "मारना", "मारने", "मारकर", "मारके", "मारेगा", "मारेगी", "मारेंगे", "मारूंगा",
	// पीट (beat)
	"पीट", "पीटा", "पीटी", "पीटे", "पीटता", "पीटती", "पीटते", "पीटना", "पीटने", "पीटकर", "पीटेगा", "पीटेगी", "पीटेंगे", "पिटाई", "पिटवाता",
	// जला (burn)
	"जला", "जलाया", "जलाई", "जलाए", "जलाता", "जलाती", "जलाते", "जलाना", "जलाने", "जलाकर", "जलाएगा", "जलाएगी", "जलाएंगे",
	// धमकी (threat), प्रताड़ना (abuse). Not "तंग": "बहुत तंग हूँ" means
	// "fed up", not harassment (it counts only next to दहेज, below).
	"धमकी", "धमकाता", "धमकाती", "धमकाते", "प्रताड़ित", "प्रताड़ना",
	// Hinglish
	"maar", "maarta", "maarti", "maarte", "maara", "maari", "maarna", "maarega", "maarenge", "marta", "marti", "marte", "peet",
	"peeta", "peetta", "pitta", "pitai", "pitaai", "peetna", "peetega", "jala", "jalaya", "jalaa", "jalana", "jalaenge", "dhamki",
	"dhamkata", "dhamkate",
	// English
	"hit", "hits", "hitting", "beat", "beats", "beating", "beaten", "burn", "burns", "burned", "burnt", "burning", "kick", "kicks",
	"kicked", "slap", "slaps", "slapped", "punch", "punches", "punched", "choke", "choked", "chokes", "strangle", "strangled",
	"torture", "tortures", "tortured", "threaten", "threatens", "threatened", "threatening", "kill", "kills", "abuse", "abuses",
	"abused", "abusing", "assault", "assaulted",
}

var crisisPairs = []crisisPair{
	// someone + violence verb ("मेरा पति मुझे रोज़ मारता है", "pati roz maarta hai",
	// "my husband hit me again", "ससुराल वाले … जला देंगे").
	{Type: crisisViolence, A: crisisPeople, B: crisisViolenceVerbs, Window: 6},
	// dowry + threat / harm
	{Type: crisisViolence, A: []string{"दहेज", "dahej", "dowry"}, B: append([]string{"निकाल", "निकाला", "निकाल दिया", "मांग", "मांगते", "मांगता",
		"मांग रहे", "तंग", "परेशान", "mang", "maang", "tang", "pareshan", "demand", "demanding", "harass*"}, crisisViolenceVerbs...), Window: 8},
	// poison / pesticide + drink / eat ("कीटनाशक पी लूंगा")
	{Type: crisisSelfHarm, A: []string{"कीटनाशक", "कीटनासक", "कीड़ा मार", "कीड़े मारने", "चूहे मार", "pesticide", "insecticide",
		"keetnashak", "kitnashak", "poison", "pills", "tablets"},
		B: []string{"पी", "पीना", "पीकर", "पिया", "पीया", "पीऊंगा", "पीऊंगी", "पी लू*", "खा", "खाकर", "खाया", "खाऊंगा", "खाऊंगी", "खा लू*", "निगल*",
			"pi", "pee", "peena", "piya", "pi lu*", "pee lu*", "kha", "khaunga", "khaungi", "kha lu*", "kha liya", "drink", "drank", "drinking",
			"swallow", "swallowed", "eat", "ate", "take all", "took all"}, Window: 3},
	// marriage + minor
	{Type: crisisChild, A: []string{"शादी", "विवाह", "ब्याह", "shaadi", "shadi", "vivah", "byah", "marriage", "married", "marry", "marrying", "wedding"},
		B: []string{"नाबालिग", "कम उम्र", "छोटी उम्र", "nabalig", "kam umar", "kam umra", "minor", "underage", "under age"}, Window: 6},
	// child + sexual abuse words
	{Type: crisisChild, A: []string{"बच्चे", "बच्ची", "बच्चा", "बेटी", "बेटे", "लड़की", "child", "children", "girl", "boy", "daughter", "son", "bachche", "bacchi", "beti", "ladki"},
		B: []string{"शोषण", "छेड़", "छेड़ता", "छेड़छाड़", "गलत काम", "गंदा काम", "गलत तरीके से छू*", "molest*", "abuse*", "touches", "touched"}, Window: 6},
}

// marriageWords and ageUnits drive detectChildMarriageAge.
var (
	marriageWords = []string{"शादी", "विवाह", "ब्याह", "shaadi", "shadi", "vivah", "byah", "marriage", "married", "marry", "marrying", "wedding"}
	ageUnits      = []string{"साल", "वर्ष", "बरस", "sal", "saal", "varsh", "year", "years", "yr", "yrs"}
	// age number words 1–17 (folded forms are computed at init).
	ageWords = map[string]int{
		"एक": 1, "दो": 2, "तीन": 3, "चार": 4, "पांच": 5, "पाँच": 5, "छह": 6, "सात": 7, "आठ": 8, "नौ": 9, "दस": 10, "ग्यारह": 11, "बारह": 12,
		"तेरह": 13, "चौदह": 14, "पंद्रह": 15, "पन्द्रह": 15, "सोलह": 16, "सत्रह": 17,
		"das": 10, "gyarah": 11, "barah": 12, "terah": 13, "chaudah": 14, "pandrah": 15, "solah": 16, "satrah": 17,
		"ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15, "sixteen": 16, "seventeen": 17,
	}
)

// crisisDistressWords are weak signals: on their own they do not trigger a
// crisis reply, but a low-confidence voice turn containing one makes the
// "please repeat" prompts also show 112 (see Conversation.notHeard).
var crisisDistressWords = []string{
	"मदद", "बचा*", "मार*", "पीट*", "खून", "डर", "रो रही", "रो रहा", "पुलिस", "हिंसा", "जान", "मर*", "दर्द", "जहर", "दहेज",
	"madad", "bacha*", "maar*", "peet*", "khoon", "dar", "darr", "police", "jaan", "mar*", "dard", "help", "danger", "hurt*", "kill*",
}

// --- compiled forms ---------------------------------------------------

type tokPattern struct {
	words  []string // folded words; the last may be a prefix
	prefix bool
}

type compiledPair struct {
	t      crisisType
	a, b   []tokPattern
	window int
}

var (
	crisisFolded   map[crisisType][]string
	crisisCompiled []compiledPair
	marriageC      []tokPattern
	ageUnitSet     = map[string]bool{}
	ageWordFolded  = map[string]int{}
	distressC      []tokPattern
)

func compilePatterns(xs []string) []tokPattern {
	out := make([]tokPattern, 0, len(xs))
	for _, x := range xs {
		p := tokPattern{}
		if strings.HasSuffix(x, "*") {
			p.prefix = true
			x = strings.TrimSuffix(x, "*")
		}
		p.words = strings.Fields(textnorm.Fold(x))
		if len(p.words) > 0 {
			out = append(out, p)
		}
	}
	return out
}

func init() {
	crisisFolded = map[crisisType][]string{}
	for t, ps := range crisisPhrases {
		for _, p := range ps {
			if f := textnorm.Fold(p); f != "" {
				crisisFolded[t] = append(crisisFolded[t], f)
			}
		}
	}
	for _, p := range crisisPairs {
		crisisCompiled = append(crisisCompiled, compiledPair{t: p.Type, a: compilePatterns(p.A), b: compilePatterns(p.B), window: p.Window})
	}
	marriageC = compilePatterns(marriageWords)
	for _, u := range ageUnits {
		ageUnitSet[textnorm.Fold(u)] = true
	}
	for w, n := range ageWords {
		ageWordFolded[textnorm.Fold(w)] = n
	}
	distressC = compilePatterns(crisisDistressWords)
}

// matchAt reports whether pattern p matches toks starting at i.
func (p tokPattern) matchAt(toks []string, i int) bool {
	if i+len(p.words) > len(toks) {
		return false
	}
	for k, w := range p.words {
		t := toks[i+k]
		if k == len(p.words)-1 && p.prefix {
			if !strings.HasPrefix(t, w) {
				return false
			}
		} else if t != w {
			return false
		}
	}
	return true
}

// positions returns the token indexes where any of ps matches.
func positions(toks []string, ps []tokPattern) []int {
	var out []int
	for i := range toks {
		for _, p := range ps {
			if p.matchAt(toks, i) {
				out = append(out, i)
				break
			}
		}
	}
	return out
}

func near(as, bs []int, window int) bool {
	for _, a := range as {
		for _, b := range bs {
			if a != b && abs(a-b) <= window {
				return true
			}
		}
	}
	return false
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// maxCrisisTokens bounds the pairwise work on very long input (the whole
// raw transcript is scanned, before the length cap).
const maxCrisisTokens = 2000

// detectCrisis returns the most serious crisis type found in text. It must
// be called on the full raw input (see the table comment).
func detectCrisis(text string) (crisisType, bool) {
	folded := textnorm.Fold(text)
	if folded == "" {
		return "", false
	}
	f := " " + folded + " "
	toks := strings.Fields(folded)
	if len(toks) > maxCrisisTokens {
		toks = toks[:maxCrisisTokens]
	}
	found := map[crisisType]bool{}
	for t, ps := range crisisFolded {
		for _, p := range ps {
			if strings.Contains(f, " "+p) {
				found[t] = true
				break
			}
		}
	}
	for _, p := range crisisCompiled {
		if found[p.t] {
			continue
		}
		as := positions(toks, p.a)
		if len(as) == 0 {
			continue
		}
		if near(as, positions(toks, p.b), p.window) {
			found[p.t] = true
		}
	}
	if !found[crisisChild] && detectChildMarriageAge(toks) {
		found[crisisChild] = true
	}
	for _, t := range crisisOrder {
		if found[t] {
			return t, true
		}
	}
	return "", false
}

// detectChildMarriageAge finds a marriage word within 8 tokens of an age
// below 18 ("14 साल की बेटी की शादी", "chaudah saal ki ladki ki shaadi").
func detectChildMarriageAge(toks []string) bool {
	ms := positions(toks, marriageC)
	if len(ms) == 0 {
		return false
	}
	var ages []int
	for i, t := range toks {
		n, ok := ageWordFolded[t]
		if !ok {
			if v, err := atoiSmall(t); err == nil {
				n, ok = v, true
			}
		}
		if !ok || n <= 0 || n >= 18 {
			continue
		}
		for k := i + 1; k <= i+2 && k < len(toks); k++ {
			if ageUnitSet[toks[k]] {
				ages = append(ages, i)
				break
			}
		}
	}
	return near(ms, ages, 8)
}

func atoiSmall(s string) (int, error) {
	if s == "" || len(s) > 3 {
		return 0, errors.New("not a small number")
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}

// hasDistress reports a weak distress signal (crisisDistressWords).
func hasDistress(text string) bool {
	toks := strings.Fields(textnorm.Fold(text))
	if len(toks) > maxCrisisTokens {
		toks = toks[:maxCrisisTokens]
	}
	return len(positions(toks, distressC)) > 0
}

// --- Helplines -----------------------------------------------------------

// Helpline is one verified helpline from the data file.
type Helpline struct {
	ID        string
	Name      bi
	Number    string
	Tags      []string  // use_for tags, as in the file
	ReviewDue time.Time // end of this day (IST-agnostic, compared with +24h slack)
}

// Helplines is the verified helpline directory.
type Helplines struct {
	list []Helpline
}

// helplineEntry is one entry of packages/scheme-knowledge/helplines.json
// (schema: cmd/kb, "helplines"). The verification fields may be flat
// (status / verified_by / verified_at / review_due, as the reviewer
// procedure writes them) or nested under "verification". Unknown fields
// are ignored.
type helplineEntry struct {
	ID     string `json:"id"`
	Number string `json:"number"`
	Name   struct {
		Hi string `json:"hi"`
		En string `json:"en"`
	} `json:"name"`
	UseFor       []string `json:"use_for"`
	Status       string   `json:"status"`
	VerifiedBy   string   `json:"verified_by"`
	VerifiedAt   string   `json:"verified_at"`
	ReviewDue    string   `json:"review_due"`
	Verification *struct {
		Status     string `json:"status"`
		VerifiedBy string `json:"verified_by"`
		VerifiedAt string `json:"verified_at"`
		ReviewDue  string `json:"review_due"`
	} `json:"verification"`
}

var helplineNumberRe = regexp.MustCompile(`^[0-9][0-9 \-]{1,18}[0-9]$`)

// MaxHelplines bounds the directory.
const MaxHelplines = 200

// helplineTagCrisis is the EXPLICIT map from a use_for tag to the crisis
// types it may be shown for. Tags not listed here never put a helpline in
// a crisis reply:
//   - "crisis" is too generic (112, 181, 1098 and 14416 all carry it, so
//     it would show the child helpline for self-harm);
//   - "emergency" is carried by the LPG gas-leak line 1906, which must
//     never be offered for a personal crisis;
//   - "legal_aid", "general_help", "scheme" are not crisis services.
//
// Like the vocabulary above, changes need counsellor review.
var helplineTagCrisis = map[string][]crisisType{
	"suicide":           {crisisSelfHarm},
	"mental_health":     {crisisSelfHarm},
	"domestic_violence": {crisisViolence},
	"women":             {crisisViolence, crisisTrafficking},
	"child":             {crisisChild, crisisTrafficking},
	"police":            {crisisViolence, crisisTrafficking, crisisDanger},
	"ambulance":         {crisisMedical},
	"fire":              {crisisDanger},
}

// LoadHelplines reads the helpline data file. Only entries that are
// VERIFIED by a human reviewer (verified_by + verified_at + review_due all
// present and well-formed) and not past review_due are kept: a wrong number
// in a crisis is worse than no number, and 112 is always shown regardless.
// Expiry is checked again at every use (Helplines.ForAt).
func LoadHelplines(path string, now time.Time) (*Helplines, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 256<<10+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 256<<10 {
		return nil, errors.New("helplines: file too large")
	}
	return ParseHelplines(b, now)
}

// ParseHelplines parses helpline JSON (see LoadHelplines). It accepts the
// shipped {"schema_version":1,"helplines":[…]} document or a bare array.
func ParseHelplines(b []byte, now time.Time) (*Helplines, error) {
	var entries []helplineEntry
	trim := strings.TrimSpace(string(b))
	if strings.HasPrefix(trim, "[") {
		if err := json.Unmarshal(b, &entries); err != nil {
			return nil, fmt.Errorf("helplines: %w", err)
		}
	} else {
		var wrap struct {
			Helplines []helplineEntry `json:"helplines"`
		}
		if err := json.Unmarshal(b, &wrap); err != nil {
			return nil, fmt.Errorf("helplines: %w", err)
		}
		entries = wrap.Helplines
	}
	h := &Helplines{}
	for _, e := range entries {
		if len(h.list) >= MaxHelplines {
			break
		}
		status, by, at, due := e.Status, e.VerifiedBy, e.VerifiedAt, e.ReviewDue
		if v := e.Verification; v != nil {
			status, by, at, due = v.Status, v.VerifiedBy, v.VerifiedAt, v.ReviewDue
		}
		if status != string(scheme.Verified) || scheme.ValidReviewer(by) != nil {
			continue
		}
		if _, err := scheme.ParseVerifiedAt(at); err != nil {
			continue
		}
		dueDay, err := time.Parse("2006-01-02", due)
		if err != nil {
			continue // review_due is mandatory for a helpline
		}
		hl := Helpline{ID: e.ID, Number: strings.TrimSpace(e.Number), ReviewDue: dueDay.Add(24 * time.Hour)}
		if hl.expired(now) {
			continue
		}
		if !helplineNumberRe.MatchString(hl.Number) || e.Name.Hi == "" && e.Name.En == "" {
			continue
		}
		for _, t := range e.UseFor {
			hl.Tags = append(hl.Tags, strings.ToLower(strings.TrimSpace(t)))
		}
		hl.Name = bi{e.Name.Hi, e.Name.En}
		if hl.Name.hi == "" {
			hl.Name.hi = hl.Name.en
		}
		if hl.Name.en == "" {
			hl.Name.en = hl.Name.hi
		}
		h.list = append(h.list, hl)
	}
	return h, nil
}

func (hl Helpline) expired(now time.Time) bool { return now.After(hl.ReviewDue) }

// Len returns the number of verified helplines loaded (expiry at load).
func (h *Helplines) Len() int {
	if h == nil {
		return 0
	}
	return len(h.list)
}

// For is ForAt with the wall clock.
func (h *Helplines) For(t crisisType, max int) []Helpline {
	return h.ForAt(t, max, time.Now())
}

// ForAt returns up to max verified, not-yet-expired helplines whose use_for
// tags map to crisis type t (helplineTagCrisis), excluding the emergency
// number (always shown separately). Expiry is evaluated at every call so a
// long-running kiosk stops showing a helpline the day its review lapses.
func (h *Helplines) ForAt(t crisisType, max int, now time.Time) []Helpline {
	if h == nil {
		return nil
	}
	var out []Helpline
	for _, hl := range h.list {
		if len(out) >= max {
			break
		}
		if strings.ReplaceAll(hl.Number, " ", "") == EmergencyNumber || hl.expired(now) {
			continue
		}
	tags:
		for _, tag := range hl.Tags {
			for _, ct := range helplineTagCrisis[tag] {
				if ct == t {
					out = append(out, hl)
					break tags
				}
			}
		}
	}
	return out
}

var crisisIntro = map[crisisType]bi{
	crisisSelfHarm: {"आप जो महसूस कर रहे हैं, वह बहुत भारी है — और आप अकेले नहीं हैं। आपकी जान बहुत कीमती है। कृपया अभी किसी से बात कीजिए।",
		"What you are feeling is very heavy — and you are not alone. Your life matters. Please talk to someone right now."},
	crisisViolence: {"आपकी सुरक्षा सबसे ज़रूरी है। हिंसा सहना आपकी मजबूरी नहीं है — मदद उपलब्ध है।",
		"Your safety comes first. You do not have to endure violence — help is available."},
	crisisChild: {"किसी बच्चे की सुरक्षा सबसे ज़रूरी है। कृपया तुरंत मदद लें।",
		"A child's safety comes first. Please get help immediately."},
	crisisTrafficking: {"यह गंभीर अपराध है और मदद उपलब्ध है। कृपया तुरंत संपर्क करें।",
		"This is a serious crime and help is available. Please make contact immediately."},
	crisisMedical: {"यह आपात स्थिति लगती है। देर मत कीजिए — तुरंत एम्बुलेंस बुलाइए।",
		"This sounds like a medical emergency. Do not wait — call an ambulance now."},
	crisisDanger: {"आपकी सुरक्षा सबसे ज़रूरी है।", "Your safety comes first."},
}

var crisisCall112 = bi{
	"अगर आप या कोई और अभी खतरे में है, तो तुरंत " + EmergencyNumber + " पर कॉल करें (राष्ट्रीय आपातकालीन नंबर — पुलिस, एम्बुलेंस, दमकल)।",
	"If you or someone else is in danger right now, call " + EmergencyNumber + " immediately (national emergency number — police, ambulance, fire).",
}

// distress112 is appended to "please repeat" prompts after a low-confidence
// voice turn that contained distress words.
var distress112 = bi{
	"अगर आप खतरे में हैं, तो तुरंत " + EmergencyNumber + " पर कॉल करें।",
	"If you are in danger, call " + EmergencyNumber + " immediately.",
}

var crisisOutro = bi{
	"पास में मौजूद किसी भरोसेमंद व्यक्ति या यहाँ के सहायक को भी बताइए।",
	"Please also tell someone you trust nearby, or the helper here.",
}

var crisisOtherNumbers = bi{"अन्य सत्यापित हेल्पलाइन:", "Other verified helplines:"}
