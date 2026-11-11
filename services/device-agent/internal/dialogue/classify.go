package dialogue

import (
	"sort"

	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/textnorm"
)

// Intents. Welfare intents start a flow; command intents act immediately;
// disallowed intents are redirected politely.
const (
	IntentLoan           = "loan"
	IntentPension        = "pension"
	IntentScholarship    = "scholarship"
	IntentFarmer         = "farmer"
	IntentCertificate    = "certificate"
	IntentRationCard     = "ration_card"
	IntentHousing        = "housing"
	IntentHealth         = "health_insurance"
	IntentSelfEmployment = "self_employment"
	IntentWomenChild     = "women_child"
	IntentDisability     = "disability"
	IntentIdentity       = "identity_document"
	IntentSchemeByName   = "scheme_by_name"

	IntentGreeting   = "greeting"
	IntentThanks     = "thanks"
	IntentRepeat     = "repeat"
	IntentHelp       = "help"
	IntentEnd        = "end"
	IntentChangeLang = "change_language"
	IntentHuman      = "human_help"
	IntentKeyboard   = "keyboard"

	IntentEntertainment = "entertainment"
	IntentChitChat      = "chit_chat"
	IntentPolitics      = "politics"
	IntentReligion      = "religion"
	IntentMedical       = "medical"
	IntentInvestment    = "investment"
	IntentRelationships = "relationships"
	IntentCoding        = "coding"
	IntentNews          = "news"
	IntentIllegal       = "illegal" // bribery, weapons

	IntentUnknown = "unknown"
)

// Analytics-only intents (never routed).
const (
	intentCrisis    = "crisis"
	intentInjection = "prompt_injection"
	intentOffTopic  = "off_topic"
	intentSensitive = "sensitive_data_warning"
	intentNoSpeech  = "not_understood"
)

// Resolution values for Analytics.
const (
	ResAnswered   = "answered"
	ResUnverified = "unverified_info"
	ResEscalated  = "escalated"
	ResUnresolved = "unresolved"
	ResRedirected = "redirected"
	ResCrisis     = "crisis"
	ResInProgress = "in_progress"
)

var welfareIntents = []string{
	IntentLoan, IntentPension, IntentScholarship, IntentFarmer, IntentCertificate, IntentRationCard,
	IntentHousing, IntentHealth, IntentSelfEmployment, IntentWomenChild, IntentDisability, IntentIdentity,
}

var commandIntents = []string{IntentGreeting, IntentThanks, IntentRepeat, IntentHelp, IntentEnd, IntentChangeLang, IntentHuman, IntentKeyboard}

var disallowedIntents = []string{
	IntentEntertainment, IntentChitChat, IntentPolitics, IntentReligion, IntentMedical,
	IntentInvestment, IntentRelationships, IntentCoding, IntentNews, IntentIllegal,
}

func isIn(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// classification is the deterministic classifier's verdict.
type classification struct {
	Intent    string            // best intent or IntentUnknown
	Slots     map[string]string // sub-slots understood from the same utterance
	Ambiguous []string          // ≥2 welfare intents too close to call
	Score     float64
}

// disallowed concept → intent
var disallowedOf = map[string]string{
	textnorm.CEntertainment: IntentEntertainment, textnorm.CChitChat: IntentChitChat,
	textnorm.CPolitics: IntentPolitics, textnorm.CReligion: IntentReligion, textnorm.CMedical: IntentMedical,
	textnorm.CInvestment: IntentInvestment, textnorm.CRelationship: IntentRelationships,
	textnorm.CCoding: IntentCoding, textnorm.CNews: IntentNews, textnorm.CIllegal: IntentIllegal,
}

// classify scores intents from lexicon concepts. It is pure and fast.
func classify(a textnorm.Analysis) classification {
	s := map[string]float64{}
	slots := map[string]string{}
	has := a.Has

	// --- welfare
	if has(textnorm.CLoan) {
		s[IntentLoan] += 3
		switch {
		case has(textnorm.CFarmer):
			slots[slotPurpose] = "agriculture"
		case has(textnorm.CBusiness) || has(textnorm.CJob):
			slots[slotPurpose] = "business"
		case has(textnorm.CEducation) || has(textnorm.CScholarship):
			slots[slotPurpose] = "education"
		case has(textnorm.CHousing):
			slots[slotPurpose] = "housing"
		}
	}
	if has(textnorm.CPension) {
		s[IntentPension] += 3
	}
	if has(textnorm.CElderly) {
		s[IntentPension] += 1.5
		slots[slotWho] = "elderly"
	}
	if has(textnorm.CWidow) {
		s[IntentPension] += 1.5
		slots[slotWho] = "widow"
	}
	if has(textnorm.CDisability) {
		if has(textnorm.CPension) {
			slots[slotWho] = "disability"
			s[IntentPension] += 1
		} else {
			s[IntentDisability] += 2.5
		}
	}
	certKinds := map[string]string{textnorm.CCaste: "caste", textnorm.CIncome: "income", textnorm.CResidence: "residence", textnorm.CBirth: "birth", textnorm.CDeath: "death"}
	certHits := 0
	for c, k := range certKinds {
		if has(c) {
			certHits++
			slots[slotCert] = k
		}
	}
	if certHits > 1 {
		delete(slots, slotCert) // "income and caste" → ask which
	}
	if has(textnorm.CCertificate) {
		s[IntentCertificate] += 2.5 + float64(min(certHits, 1))
	} else if has(textnorm.CCaste) {
		s[IntentCertificate] += 2 // "जाति बनवाना है"
	} else if certHits > 0 && !has(textnorm.CPension) && !has(textnorm.CLoan) {
		s[IntentCertificate] += 1
	}
	if has(textnorm.CRation) {
		s[IntentRationCard] += 3
	}
	if has(textnorm.CIdentity) || (has(textnorm.CAadhaar) && has(textnorm.CCard)) {
		s[IntentIdentity] += 2.5
	} else if has(textnorm.CAadhaar) {
		s[IntentIdentity] += 1
	}
	if has(textnorm.CFarmer) {
		s[IntentFarmer] += 2.5
	}
	if has(textnorm.CScholarship) {
		s[IntentScholarship] += 3
	} else if has(textnorm.CEducation) {
		s[IntentScholarship] += 1.5
	}
	if has(textnorm.CHousing) {
		s[IntentHousing] += 2.5
	}
	if has(textnorm.CHealth) {
		s[IntentHealth] += 2
	}
	if has(textnorm.CInsurance) {
		s[IntentHealth] += 2
	}
	if has(textnorm.CBusiness) {
		s[IntentSelfEmployment] += 2.5
	} else if has(textnorm.CJob) {
		s[IntentSelfEmployment] += 1.5
	}
	if has(textnorm.CWomen) {
		s[IntentWomenChild] += 1.5
	}
	if has(textnorm.CChild) {
		s[IntentWomenChild] += 1.5
	}
	// A loan for X is a loan, not X.
	if has(textnorm.CLoan) {
		for _, i := range []string{IntentFarmer, IntentScholarship, IntentHousing, IntentSelfEmployment} {
			if s[i] > 0 {
				s[IntentLoan] += 0.5
				s[i] = min(s[i], 1)
			}
		}
	}
	// A widow/disability pension is a pension, not women_child/disability.
	if has(textnorm.CPension) {
		s[IntentWomenChild] = min(s[IntentWomenChild], 1)
		s[IntentDisability] = min(s[IntentDisability], 1)
	}
	// Ration card / identity card: the card word disambiguates.
	if has(textnorm.CRation) && has(textnorm.CCard) {
		s[IntentIdentity] = min(s[IntentIdentity], 1)
	}

	// --- disallowed
	for c, in := range disallowedOf {
		if has(c) {
			s[in] += 2.5
		}
	}
	// "इलाज/बीमा" + illness words is a health-insurance question, not diagnosis.
	if has(textnorm.CMedical) && (has(textnorm.CInsurance) || has(textnorm.CHealth) && has(textnorm.CMoney)) {
		s[IntentMedical] = 0
		s[IntentHealth] += 1
	}

	// --- commands
	short := len(a.Tokens) <= 4
	if has(textnorm.CGreeting) {
		s[IntentGreeting] += 1
	}
	if has(textnorm.CThanks) {
		s[IntentThanks] += 1.2
	}
	if has(textnorm.CHelp) {
		s[IntentHelp] += 0.8
	}
	// "again" / "फिर से" means REPEAT only when it is (nearly) the whole
	// utterance: in "my husband hit me again" it is not a command.
	if has(textnorm.CRepeat) && repeatIsWholeUtterance(a) {
		s[IntentRepeat] += 2
	}
	if has(textnorm.CEnd) && short {
		s[IntentEnd] += 2.2
	}
	if (has(textnorm.CEnglish) || has(textnorm.CHindi)) && len(a.Tokens) <= 6 {
		s[IntentChangeLang] += 2.5
	}
	if has(textnorm.CHuman) {
		s[IntentHuman] += 2.5
	}
	if has(textnorm.CKeyboard) && short {
		s[IntentKeyboard] += 2
	}

	best := func(group []string) (string, float64, string, float64) {
		type kv struct {
			k string
			v float64
		}
		var xs []kv
		for _, g := range group {
			if s[g] > 0 {
				xs = append(xs, kv{g, s[g]})
			}
		}
		sort.SliceStable(xs, func(i, j int) bool { return xs[i].v > xs[j].v })
		var a, b kv
		if len(xs) > 0 {
			a = xs[0]
		}
		if len(xs) > 1 {
			b = xs[1]
		}
		return a.k, a.v, b.k, b.v
	}
	w1, ws1, w2, ws2 := best(welfareIntents)
	c1, cs1, _, _ := best(commandIntents)
	d1, ds1, _, _ := best(disallowedIntents)

	out := classification{Intent: IntentUnknown, Slots: slots}
	// Bribery / weapons are always redirected, whatever else is mentioned;
	// so are voting questions ("पेंशन वाली पार्टी को वोट दूं?") unless an ID
	// card is meant ("वोटर कार्ड").
	if has(textnorm.CIllegal) {
		out.Intent, out.Score = IntentIllegal, 5
		return out
	}
	if has(textnorm.CVote) && !has(textnorm.CIdentity) && !has(textnorm.CCard) {
		out.Intent, out.Score = IntentPolitics, 5
		return out
	}
	switch {
	case cs1 >= 2.2 && (cs1 > ws1 || c1 == IntentEnd && ws1 < 3 || c1 == IntentHuman):
		// explicit commands ("बंद करो", "किसी व्यक्ति से बात") beat weak topics
		out.Intent, out.Score = c1, cs1
	case ws1 >= 2:
		if w2 != "" && ws1-ws2 < 0.5 {
			out.Ambiguous = []string{w1, w2}
		}
		out.Intent, out.Score = w1, ws1
	case cs1 >= 2:
		out.Intent, out.Score = c1, cs1
	case ds1 >= 2:
		out.Intent, out.Score = d1, ds1
	case ws1 >= 1.5 && (w2 == "" || ws1-ws2 >= 0.5):
		out.Intent, out.Score = w1, ws1
	case cs1 > 0:
		out.Intent, out.Score = c1, cs1
	case ws1 > 0:
		// weak signal only ("आय", "आधार"): treat as ambiguous
		out.Ambiguous = []string{w1}
		if w2 != "" {
			out.Ambiguous = append(out.Ambiguous, w2)
		}
		out.Score = ws1
	}
	return out
}

// repeatFiller are words that may accompany a repeat request without
// making it a different sentence ("फिर से बोलिए", "please say that again").
var repeatFiller = foldSet(
	"बोलिए", "बोलो", "बोलें", "बोलिये", "बताइए", "बताओ", "बताइये", "सुनाइए", "सुनाओ", "कहिए", "कहो", "जरा", "एक", "बार", "प्लीज",
	"please", "say", "tell", "it", "that", "can", "could", "you", "again", "once", "more", "sir", "ji", "boliye", "bolo",
	"bataiye", "sunaiye", "ek", "baar", "bar", "aap", "plz", "pls",
)

// repeatIsWholeUtterance reports whether, apart from the repeat phrase,
// stopwords and filler, at most one token remains.
func repeatIsWholeUtterance(a textnorm.Analysis) bool {
	covered := make([]bool, len(a.Tokens))
	for _, h := range a.Concepts {
		if h.Concept != textnorm.CRepeat {
			continue
		}
		for k := h.Start; k < h.Start+h.Words && k < len(covered); k++ {
			covered[k] = true
		}
	}
	rest := 0
	for i, t := range a.Tokens {
		if covered[i] || textnorm.IsStopword(t) || repeatFiller[t] {
			continue
		}
		rest++
	}
	return rest <= 1
}
