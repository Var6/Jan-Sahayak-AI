package docassist

import (
	"regexp"
	"strings"
	"unicode"
)

// Keyword rules. Keywords are compared in "squashed" form: lower case, no
// nukta / zero-width joiners, Devanagari digits as ASCII, and every
// character that is not a letter, vowel sign or digit removed. That makes
// "प्रमाण-पत्र", "प्रमाण पत्र" and "प्रमाणपत्र" the same, and tolerates OCR
// putting spaces in odd places.
//
// Weights: 4–5 = a phrase that names the document itself ("आय प्रमाण
// पत्र", "Income Tax Department"); 2–3 = typical of it; 1 = weak hint.

type kw struct {
	k string
	w int
}

type rule struct {
	t   DocType
	kws []kw
	// pats are matched on the original (unsquashed) text.
	pats []pat
}

type pat struct {
	re *regexp.Regexp
	w  int
}

var (
	reAadhaarNum = regexp.MustCompile(`(?:^|[^0-9])[0-9]{4}[ \-]?[0-9]{4}[ \-]?[0-9]{4}(?:[^0-9]|$)`)
	rePAN        = regexp.MustCompile(`\b[A-Z]{5}[0-9]{4}[A-Z]\b`)
	reEPIC       = regexp.MustCompile(`\b[A-Z]{3}[0-9]{7}\b`)
	reIFSC       = regexp.MustCompile(`\b[A-Z]{4}0[A-Z0-9]{6}\b`)
)

var rules = []rule{
	{t: Aadhaar, kws: []kw{
		{"uidai", 4}, {"uniqueidentificationauthority", 4}, {"भारतीयविशिष्टपहचानप्राधिकरण", 4},
		{"मेराआधारमेरीपहचान", 4}, {"आधार", 2}, {"aadhaar", 2}, {"aadhar", 2},
		{"enrolmentno", 2}, {"नामांकनक्रम", 2},
	}, pats: []pat{{reAadhaarNum, 2}}},
	{t: PAN, kws: []kw{
		{"incometaxdepartment", 4}, {"आयकरविभाग", 4}, {"permanentaccountnumber", 4},
		{"स्थायीलेखासंख्या", 4},
	}, pats: []pat{{rePAN, 3}}},
	{t: VoterID, kws: []kw{
		{"electioncommissionofindia", 4}, {"भारतनिर्वाचनआयोग", 4}, {"electorsphotoidentitycard", 4},
		{"निर्वाचनआयोग", 3}, {"मतदाताफोटोपहचानपत्र", 4}, {"elector", 2}, {"निर्वाचक", 2},
		{"मतदाता", 2}, {"epic", 1},
	}, pats: []pat{{reEPIC, 2}}},
	{t: RationCard, kws: []kw{
		{"राशनकार्ड", 4}, {"rationcard", 4}, {"जनवितरण", 3}, {"publicdistribution", 3},
		{"राष्ट्रीयखाद्यसुरक्षा", 3}, {"nationalfoodsecurity", 3}, {"nfsa", 2},
		{"अंत्योदय", 2}, {"antyodaya", 2}, {"उचितमूल्य", 2}, {"fairprice", 2},
	}},
	{t: CasteCertificate, kws: []kw{
		{"जातिप्रमाणपत्र", 5}, {"castecertificate", 5}, {"जाति", 1}, {"caste", 1},
	}},
	{t: IncomeCertificate, kws: []kw{
		{"आयप्रमाणपत्र", 5}, {"incomecertificate", 5}, {"वार्षिकआय", 2}, {"annualincome", 2},
	}},
	{t: ResidenceCertificate, kws: []kw{
		{"निवासप्रमाणपत्र", 5}, {"आवासीयप्रमाणपत्र", 5}, {"residencecertificate", 5},
		{"residentialcertificate", 5}, {"domicile", 4}, {"स्थायीनिवास", 3},
	}},
	{t: BankPassbook, kws: []kw{
		{"passbook", 4}, {"पासबुक", 4}, {"ifsc", 3}, {"micr", 2}, {"cifno", 2},
		{"accountno", 2}, {"खातासंख्या", 2}, {"bank", 1}, {"बैंक", 1}, {"branch", 1}, {"शाखा", 1},
	}, pats: []pat{{reIFSC, 2}}},
	{t: LandRecord, kws: []kw{
		{"भूस्वामित्वप्रमाणपत्र", 5}, {"खतियान", 4}, {"जमाबंदी", 4}, {"खेसरा", 4},
		{"landrecord", 4}, {"भूअभिलेख", 4}, {"khatiyan", 4}, {"jamabandi", 4}, {"khesra", 4},
		{"रकबा", 3}, {"मौजा", 2}, {"lpc", 2}, {"भूमि", 1}, {"खाता", 1},
	}},
	{t: BirthCertificate, kws: []kw{
		{"जन्मप्रमाणपत्र", 5}, {"birthcertificate", 5}, {"certificateofbirth", 5},
		{"जन्मएवंमृत्यु", 3}, {"registrationofbirths", 3},
	}},
	{t: SchoolCertificate, kws: []kw{
		{"schoolleaving", 4}, {"transfercertificate", 4}, {"स्थानांतरणप्रमाणपत्र", 4},
		{"matriculation", 3}, {"मैट्रिक", 3}, {"secondaryschool", 3}, {"marksheet", 3},
		{"अंकपत्र", 3}, {"examinationboard", 3}, {"परीक्षासमिति", 3}, {"माध्यमिक", 2},
		{"intermediate", 2}, {"इंटरमीडिएट", 2}, {"विद्यालय", 1}, {"rollno", 1}, {"अनुक्रमांक", 1},
	}},
}

// squashedRules are the rules with their keywords squashed once.
var squashedRules = func() []rule {
	out := make([]rule, len(rules))
	for i, r := range rules {
		nr := rule{t: r.t, pats: r.pats}
		for _, k := range r.kws {
			nr.kws = append(nr.kws, kw{squash(k.k), k.w})
		}
		out[i] = nr
	}
	return out
}()

// Classification thresholds.
const (
	minScore   = 3 // below this nothing is recognised
	highScore  = 4 // a strong keyword at least
	highMargin = 2 // and this far ahead of the runner-up
)

// Classify returns the most likely document type and a confidence.
func Classify(text string) (DocType, string) {
	scores := Scores(text)
	best, top, runner := Unknown, 0, 0
	for _, r := range squashedRules { // fixed order: ties resolve the same way every time
		switch s := scores[r.t]; {
		case s > top:
			best, top, runner = r.t, s, top
		case s > runner:
			runner = s
		}
	}
	if top < minScore || top == runner {
		// Too little evidence, or a tie between two types: ask, do not guess.
		return Unknown, ConfNone
	}
	if top >= highScore && top-runner >= highMargin {
		return best, ConfHigh
	}
	return best, ConfLow
}

// Scores returns the rule score of every type (for tests and tuning).
func Scores(text string) map[DocType]int {
	sq := squash(text)
	out := make(map[DocType]int, len(squashedRules))
	for _, r := range squashedRules {
		s := 0
		for _, k := range r.kws {
			if k.k != "" && strings.Contains(sq, k.k) {
				s += k.w
			}
		}
		for _, p := range r.pats {
			if p.re.MatchString(text) {
				s += p.w // once, however often it matches
			}
		}
		out[r.t] = s
	}
	return out
}

// squash lower-cases s, drops nukta and zero-width (non-)joiners, maps
// Devanagari digits to ASCII and keeps only letters, marks and digits.
func squash(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '़' || r == '‌' || r == '‍':
			continue
		case r >= '०' && r <= '९':
			b.WriteRune('0' + (r - '०'))
		case unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}
