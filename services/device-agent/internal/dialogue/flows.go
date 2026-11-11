package dialogue

import (
	"strings"

	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/textnorm"
)

// Slot keys (values are short enum strings, never citizen free text).
const (
	slotPurpose     = "purpose"
	slotWho         = "who"
	slotCert        = "cert"
	slotKeywords    = "kw"    // concept keys from the opening utterance
	slotCands       = "cands" // comma-separated candidate record ids
	slotAadhaarOnly = "aadhaar_only"
	slotDistrict    = "district"
	slotIneligible  = "ineligible"
	slotUnknownName = "unknown_name" // citizen named a scheme we have no record for
	slotQPrefix     = "q:"
)

// Steps.
const (
	stepSub    = "sub"  // asking the flow's sub-question
	stepPick   = "pick" // choosing among candidate records
	stepQ      = "q"    // eligibility question; index in slot "qi"
	stepMore   = "more" // "do you need more help?"
	stepMenu   = "menu" // top-level / clarification menu
	slotQIndex = "qi"
)

// Limits.
const (
	MaxCandidates = 4
	MaxQuestions  = 3
	maxOptions    = 8
	maxSlotValue  = 128
)

// subOpt is one answer to a flow's sub-question.
type subOpt struct {
	id         string
	label      bi
	concepts   []string // lexicon concepts that select this option by voice
	categories []string // scheme categories to search
	query      string   // generic search words (concept keys)
}

type subQuestion struct {
	slot string
	ask  bi
	opts []subOpt
}

type flowDef struct {
	categories []string
	query      string
	// concept, when set with a sub-question, must appear in a record's
	// names/aliases/keywords (together with the chosen sub-option's concept)
	// for it to be offered: a "loan for farming" must be a loan record.
	concept string
	sub     *subQuestion
}

var flows = map[string]flowDef{
	IntentLoan: {categories: []string{"loan"}, query: "loan", concept: textnorm.CLoan, sub: &subQuestion{
		slot: slotPurpose,
		ask:  bi{"लोन किस काम के लिए चाहिए?", "What do you need the loan for?"},
		opts: []subOpt{
			{id: "agriculture", label: bi{"खेती / कृषि", "Agriculture"}, concepts: []string{textnorm.CFarmer}, categories: []string{"loan", "farmer"}, query: "loan farmer agriculture"},
			{id: "business", label: bi{"व्यवसाय / स्वरोजगार", "Business / self-employment"}, concepts: []string{textnorm.CBusiness, textnorm.CJob}, categories: []string{"loan", "self_employment"}, query: "loan business"},
			{id: "education", label: bi{"पढ़ाई", "Education"}, concepts: []string{textnorm.CEducation, textnorm.CScholarship}, categories: []string{"loan", "education", "scholarship"}, query: "loan education"},
			{id: "housing", label: bi{"घर / मकान", "Housing"}, concepts: []string{textnorm.CHousing}, categories: []string{"loan", "housing"}, query: "loan housing"},
			{id: "other", label: bi{"अन्य", "Other"}, concepts: []string{textnorm.CMore}, categories: []string{"loan"}, query: "loan"},
		},
	}},
	IntentPension: {categories: []string{"pension"}, query: "pension", concept: textnorm.CPension, sub: &subQuestion{
		slot: slotWho,
		ask:  bi{"पेंशन किसके लिए जानना है?", "Who is the pension for?"},
		opts: []subOpt{
			{id: "elderly", label: bi{"बुज़ुर्ग / वृद्धावस्था", "Elderly / old age"}, concepts: []string{textnorm.CElderly}, categories: []string{"pension"}, query: "pension elderly"},
			{id: "widow", label: bi{"विधवा", "Widow"}, concepts: []string{textnorm.CWidow}, categories: []string{"pension", "women_child"}, query: "pension widow"},
			{id: "disability", label: bi{"दिव्यांग जन", "Person with disability"}, concepts: []string{textnorm.CDisability}, categories: []string{"pension", "disability"}, query: "pension disability"},
		},
	}},
	IntentCertificate: {categories: []string{"certificate"}, query: "certificate", concept: textnorm.CCertificate, sub: &subQuestion{
		slot: slotCert,
		ask:  bi{"कौन सा प्रमाण पत्र बनवाना है?", "Which certificate do you need?"},
		opts: []subOpt{
			{id: "caste", label: bi{"जाति प्रमाण पत्र", "Caste certificate"}, concepts: []string{textnorm.CCaste}, categories: []string{"certificate"}, query: "certificate caste"},
			{id: "income", label: bi{"आय प्रमाण पत्र", "Income certificate"}, concepts: []string{textnorm.CIncome}, categories: []string{"certificate"}, query: "certificate income"},
			{id: "residence", label: bi{"निवास प्रमाण पत्र", "Residence certificate"}, concepts: []string{textnorm.CResidence}, categories: []string{"certificate"}, query: "certificate residence"},
			{id: "birth", label: bi{"जन्म प्रमाण पत्र", "Birth certificate"}, concepts: []string{textnorm.CBirth}, categories: []string{"certificate"}, query: "certificate birth"},
			{id: "death", label: bi{"मृत्यु प्रमाण पत्र", "Death certificate"}, concepts: []string{textnorm.CDeath}, categories: []string{"certificate"}, query: "certificate death"},
		},
	}},
	IntentFarmer:         {categories: []string{"farmer"}, query: "farmer"},
	IntentScholarship:    {categories: []string{"scholarship", "education"}, query: "scholarship education"},
	IntentRationCard:     {categories: []string{"food_security"}, query: "ration card"},
	IntentHousing:        {categories: []string{"housing"}, query: "housing"},
	IntentHealth:         {categories: []string{"health"}, query: "health insurance"},
	IntentSelfEmployment: {categories: []string{"self_employment", "loan"}, query: "business"},
	IntentWomenChild:     {categories: []string{"women_child"}, query: "women child"},
	IntentDisability:     {categories: []string{"disability", "pension"}, query: "disability"},
	IntentIdentity:       {categories: []string{"identity_document"}, query: "identity aadhaar card"},
}

func (q *subQuestion) find(id string) (subOpt, bool) {
	for _, o := range q.opts {
		if o.id == id {
			return o, true
		}
	}
	return subOpt{}, false
}

// matchSub picks a sub-option from concepts in the analysed utterance.
func (q *subQuestion) matchSub(a textnorm.Analysis) (string, bool) {
	found := ""
	for _, o := range q.opts {
		for _, c := range o.concepts {
			if a.Has(c) && c != textnorm.CMore {
				if found != "" && found != o.id {
					return "", false // two options named → ask
				}
				found = o.id
			}
		}
	}
	return found, found != ""
}

// --- option ids --------------------------------------------------------

const (
	optTopicPrefix = "topic:"
	optSubPrefix   = "sub:"
	optPickPrefix  = "pick:"
	optAnsPrefix   = "ans:"
	optMoreYes     = "more:yes"
	optMoreNo      = "more:no"
	optRepeat      = "cmd:repeat"
	optEnd         = "cmd:end"
	optHuman       = "cmd:human"
	optKeyboard    = "cmd:keyboard"
	optMenu        = "cmd:menu"
	optOtherTopics = "cmd:other_topics"
)

// globalOptions are accepted at any time even if not on screen.
var globalOptions = map[string]bool{optRepeat: true, optEnd: true, optHuman: true, optKeyboard: true, optMenu: true, optOtherTopics: true}

var primaryTopics = []string{IntentLoan, IntentPension, IntentCertificate, IntentFarmer, IntentScholarship, IntentRationCard}
var otherTopics = []string{IntentHousing, IntentHealth, IntentSelfEmployment, IntentWomenChild, IntentDisability, IntentIdentity}

func topicOptions(l Lang, topics []string, withOther bool) []Option {
	var out []Option
	for _, t := range topics {
		out = append(out, Option{ID: optTopicPrefix + t, Label: topicLabel[t].in(l)})
	}
	if withOther {
		out = append(out, Option{ID: optOtherTopics, Label: m.optOther.in(l)})
	}
	return out
}

// withGlobals appends "repeat" and "end" (always offered) and caps the list.
func withGlobals(l Lang, opts []Option) []Option {
	out := append([]Option{}, opts...)
	if len(out) > maxOptions-2 {
		out = out[:maxOptions-2]
	}
	return append(out, Option{ID: optRepeat, Label: m.optRepeat.in(l)}, Option{ID: optEnd, Label: m.optEnd.in(l)})
}

// matchOffered maps a spoken/typed answer to one of the options on screen.
func matchOffered(a textnorm.Analysis, st *State) (string, bool) {
	if st.Last == nil || len(st.Last.Options) == 0 {
		return "", false
	}
	opts := st.Last.Options
	ids := map[string]bool{}
	for _, o := range opts {
		ids[o.ID] = true
	}
	// yes / no answers
	yes, no := a.Has(textnorm.CYes), a.Has(textnorm.CNo)
	if yes != no && len(a.Tokens) <= 6 {
		for _, cand := range []string{optAnsPrefix + "yes", optMoreYes} {
			if yes && ids[cand] {
				return cand, true
			}
		}
		for _, cand := range []string{optAnsPrefix + "no", optMoreNo} {
			if no && ids[cand] {
				return cand, true
			}
		}
	}
	// sub-question answers via concepts
	if st.Step == stepSub {
		if fd, ok := flows[st.Flow]; ok && fd.sub != nil {
			if id, ok := fd.sub.matchSub(a); ok && ids[optSubPrefix+id] {
				return optSubPrefix + id, true
			}
		}
	}
	// ordinal answers: "1", "पहला", "दूसरा", "first" …
	if idx, ok := ordinal(a); ok {
		n := 0
		for _, o := range opts {
			if globalOptions[o.ID] {
				continue
			}
			if n == idx {
				return o.ID, true
			}
			n++
		}
	}
	// label match (folded, fuzzy): the citizen repeats an option label
	var pairs [][2]string
	for _, o := range opts {
		if globalOptions[o.ID] {
			continue
		}
		for _, part := range strings.Split(o.Label, "/") {
			pairs = append(pairs, [2]string{part, o.ID})
		}
	}
	v := textnorm.NewVocab(pairs)
	best, bestWords := "", 0
	for _, mt := range v.Find(a.Tokens) {
		if mt.Words > bestWords {
			best, bestWords = mt.Payload, mt.Words
		}
	}
	if best != "" && bestWords >= 1 {
		// accept only if the label covers a meaningful part of the input
		content := 0
		for _, t := range a.Tokens {
			if !textnorm.IsStopword(t) {
				content++
			}
		}
		if bestWords*2 >= content || bestWords >= 2 {
			return best, true
		}
	}
	return "", false
}

var ordinals = map[string]int{
	"1": 0, "2": 1, "3": 2, "4": 3, "5": 4, "6": 5,
	"पहला": 0, "पहली": 0, "पहले": 0, "दूसरा": 1, "दूसरी": 1, "दूसरे": 1, "तीसरा": 2, "तीसरी": 2, "चौथा": 3, "चौथी": 3,
	"first": 0, "second": 1, "third": 2, "fourth": 3, "pehla": 0, "pahla": 0, "doosra": 1, "dusra": 1, "teesra": 2, "tisra": 2,
}
var foldedOrdinals = func() map[string]int {
	out := map[string]int{}
	for k, v := range ordinals {
		out[textnorm.Fold(k)] = v
	}
	return out
}()

func ordinal(a textnorm.Analysis) (int, bool) {
	if len(a.Tokens) > 4 {
		return 0, false
	}
	for _, t := range a.Tokens {
		if i, ok := foldedOrdinals[t]; ok {
			return i, true
		}
	}
	return 0, false
}

// questionsFor returns the eligibility questions the kiosk may ask.
func questionsFor(r scheme.Record) []scheme.Question {
	var out []scheme.Question
	for _, q := range r.Questions {
		if q.ID == "" || (q.Ask.Hi == "" && q.Ask.En == "") {
			continue
		}
		if q.Kind != "yes_no" && !(q.Kind == "choice" && len(q.Choices) > 0 && len(q.Choices) <= 5) {
			continue
		}
		out = append(out, q)
		if len(out) >= MaxQuestions {
			break
		}
	}
	return out
}

// --- Named schemes we have no record of (review F1) ----------------------

var (
	// schemeNamePrefixes start a scheme's proper name ("पीएम किसान",
	// "मुख्यमंत्री … योजना"). Folded forms.
	schemeNamePrefixes = foldSet("पीएम", "पीम", "प्रधानमंत्री", "मुख्यमंत्री", "सीएम", "pm", "cm", "pradhanmantri", "mukhyamantri")
	schemeNamePrefix2  = foldSet("प्रधान मंत्री", "मुख्य मंत्री", "pradhan mantri", "mukhya mantri")
	schemeMarkerWords  = foldSet("योजना", "योजनाएं", "स्कीम", "yojana", "yojna", "scheme", "मिशन", "mission", "अभियान", "abhiyan")
	// genericNameWords describe schemes in general, not one scheme.
	genericNameWords = foldSet("सरकारी", "sarkari", "government", "govt", "कोई", "कौन", "कौनसी", "कोनसी", "कौन सी", "which", "any", "नई",
		"new", "सभी", "all", "अच्छी", "बिहार", "bihar", "राज्य", "केंद्र", "केंद्रीय", "central", "state", "सी", "इस", "उस", "यह", "वह",
		"किस", "किसी", "इन", "उन", "ये", "वो", "दूसरी", "other", "अन्य", "बड़ी", "हमारी", "आपकी", "इसी", "कल्याण", "welfare")
)

// namesUnknownScheme reports whether the citizen appears to name a specific
// scheme ("पीएम किसान", "मुख्यमंत्री वृद्धजन पेंशन योजना", "<X> स्कीम").
// Callers use it only when knowledge.MatchNames found no record name.
func namesUnknownScheme(a textnorm.Analysis) bool {
	toks := a.Tokens
	lex := textnorm.DefaultLexicon()
	generic := func(w string) bool {
		if textnorm.IsStopword(w) || genericNameWords[w] {
			return true
		}
		ms := lex.Find([]string{w})
		return len(ms) > 0 && ms[0].Distance == 0
	}
	for i, t := range toks {
		if schemeNamePrefixes[t] && i+1 < len(toks) && !textnorm.IsStopword(toks[i+1]) {
			return true
		}
		if i+2 < len(toks) && schemeNamePrefix2[t+" "+toks[i+1]] {
			return true
		}
		if schemeMarkerWords[t] {
			for k := i - 1; k >= 0 && k >= i-3; k-- {
				w := toks[k]
				if textnorm.IsStopword(w) {
					break
				}
				if !generic(w) {
					return true
				}
			}
		}
	}
	return false
}
