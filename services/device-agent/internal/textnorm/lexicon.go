package textnorm

import (
	"sort"
	"sync"
)

// Concept keys produced by the default lexicon. They are language-neutral
// topic tokens ("loan", "caste" …) derived from Hindi, Hinglish and English
// surface forms. They carry no government facts: no scheme names, amounts
// or rules live here — only ordinary words people use.
const (
	CLoan        = "loan"
	CPension     = "pension"
	CElderly     = "elderly"
	CWidow       = "widow"
	CDisability  = "disability"
	CScholarship = "scholarship"
	CEducation   = "education"
	CFarmer      = "farmer"
	CCertificate = "certificate"
	CCaste       = "caste"
	CIncome      = "income"
	CResidence   = "residence"
	CBirth       = "birth"
	CDeath       = "death"
	CRation      = "ration"
	CCard        = "card"
	CHousing     = "housing"
	CHealth      = "health"
	CInsurance   = "insurance"
	CBusiness    = "business"
	CJob         = "job"
	CWomen       = "women"
	CChild       = "child"
	CAadhaar     = "aadhaar"
	CIdentity    = "identity"
	CDocument    = "document"
	COnly        = "only"
	CAmount      = "amount"
	CMoney       = "money"
	CNotReceived = "not_received"
	CDistrict    = "district_word"

	CGreeting = "greeting"
	CThanks   = "thanks"
	CRepeat   = "repeat"
	CHelp     = "help"
	CEnd      = "end"
	CYes      = "yes"
	CNo       = "no"
	CEnglish  = "lang_en"
	CHindi    = "lang_hi"
	CHuman    = "human"
	CKeyboard = "keyboard"
	CMore     = "more"

	CEntertainment = "entertainment"
	CChitChat      = "chit_chat"
	CPolitics      = "politics"
	CReligion      = "religion"
	CMedical       = "medical"
	CInvestment    = "investment"
	CRelationship  = "relationships"
	CCoding        = "coding"
	CNews          = "news"
)

// defaultLexicon maps concept → surface forms (any script; folded at build).
var defaultLexicon = map[string][]string{
	CLoan: {"लोन", "कर्ज", "कर्जा", "क़र्ज़", "ऋण", "उधार", "लोन्स", "loan", "loans", "lon", "laon", "lone", "karz", "karza", "karja", "karj", "karjaa", "rin", "rinn", "udhar", "udhaar", "credit", "क्रेडिट"},
	CPension: {"पेंशन", "पेन्शन", "पेंसन", "पैंशन", "pension", "penshan", "pension", "pensan", "penson", "pention", "penshion"},
	CElderly: {"वृद्धावस्था", "वृद्ध", "वृद्धजन", "बुजुर्ग", "बुज़ुर्ग", "बूढ़ा", "बूढ़ी", "बूढ़े", "बुढ़ापा", "बुढ़ापे", "old age", "elderly", "old", "senior citizen", "budhapa", "budhape", "vridha", "vriddha", "vridhavastha", "vriddhavastha", "buzurg", "bujurg", "budha", "budhi"},
	CWidow: {"विधवा", "vidhwa", "vidhva", "widow", "widowed", "पति की मृत्यु", "पति नहीं रहे", "पति का देहांत"},
	CDisability: {"दिव्यांग", "विकलांग", "दिव्यांगता", "विकलांगता", "divyang", "viklang", "disabled", "disability", "handicapped", "अंधा", "नेत्रहीन", "बहरा", "blind", "deaf"},
	CScholarship: {"छात्रवृत्ति", "स्कॉलरशिप", "स्कालरशिप", "वजीफा", "वज़ीफ़ा", "scholarship", "chhatravritti", "chatravritti", "vajifa", "wazifa", "scolarship"},
	CEducation: {"पढ़ाई", "पढ़ाइ", "शिक्षा", "स्कूल", "कॉलेज", "विद्यालय", "छात्र", "छात्रा", "education", "padhai", "padhaai", "school", "college", "student", "study", "studies", "shiksha"},
	CFarmer: {"किसान", "कृषि", "खेती", "खेतीबाड़ी", "फसल", "kisan", "kishan", "kissan", "farmer", "farmers", "kheti", "krishi", "fasal", "farming", "agriculture", "agricultural", "खेत"},
	CCertificate: {"प्रमाण पत्र", "प्रमाणपत्र", "सर्टिफिकेट", "सर्टिफिकेट", "certificate", "praman patra", "pramanpatra", "praman pater", "sartifiket", "certficate", "पत्र"},
	CCaste: {"जाति", "जात", "caste", "jati", "jaati", "jaat", "cast"},
	CIncome: {"आय", "आमदनी", "इनकम", "income", "aay", "aamdani", "amdani"},
	CResidence: {"निवास", "डोमिसाइल", "domicile", "residence", "residential", "niwas", "nivas", "आवासीय"},
	CBirth: {"जन्म", "birth", "janm", "janam"},
	CDeath: {"मृत्यु", "मौत", "death", "mrityu", "maut"},
	CRation: {"राशन", "ration", "rashan", "राशनकार्ड"},
	CCard: {"कार्ड", "card", "kard", "कारड"},
	CHousing: {"घर", "मकान", "आवास", "पक्का मकान", "house", "housing", "home", "ghar", "makan", "makaan", "awas", "aawas", "awaas"},
	CHealth: {"इलाज", "अस्पताल", "स्वास्थ्य", "हेल्थ", "health", "hospital", "ilaj", "ilaaj", "aspatal", "treatment"},
	CInsurance: {"बीमा", "इंश्योरेंस", "insurance", "bima", "beema"},
	CBusiness: {"स्वरोजगार", "व्यवसाय", "व्यापार", "धंधा", "दुकान", "बिजनेस", "बिज़नेस", "उद्यम", "उद्योग", "business", "self employment", "rozgar", "rojgar", "swarojgar", "vyapar", "vyavsay", "dhandha", "dukan", "udyam", "startup", "shop", "रोजगार", "रोज़गार"},
	CJob: {"नौकरी", "job", "naukri", "employment"},
	CWomen: {"महिला", "महिलाओं", "औरत", "स्त्री", "women", "woman", "mahila", "बेटी", "लड़की", "girl", "beti", "ladki"},
	CChild: {"बच्चा", "बच्चे", "बच्ची", "शिशु", "child", "children", "bachcha", "bacche", "baby", "गर्भवती", "pregnant", "pregnancy", "garbhvati"},
	CAadhaar: {"आधार", "aadhaar", "aadhar", "adhar", "adhaar", "uid"},
	CIdentity: {"पैन", "वोटर", "पहचान पत्र", "पहचान", "pan", "voter", "identity", "id card", "पासपोर्ट", "passport"},
	CDocument: {"दस्तावेज", "दस्तावेज़", "कागज", "कागजात", "कागज़ात", "document", "documents", "kagaz", "kagzat", "papers"},
	COnly: {"सिर्फ", "सिर्फ़", "केवल", "only", "sirf", "keval", "kewal", "बस आधार"},
	CAmount: {"कितना", "कितनी", "कितने", "राशि", "how much", "kitna", "kitni", "amount"},
	CMoney: {"पैसा", "पैसे", "रुपया", "रुपये", "money", "paisa", "paise", "rupaye"},
	CNotReceived: {"नहीं आया", "नहीं आई", "नहीं मिला", "नहीं मिली", "रुक गया", "not received", "nahi aaya", "nahi mila"},
	CDistrict: {"जिला", "जिले", "ज़िला", "ज़िले", "जनपद", "district", "zila", "jila", "zile", "jile"},

	CGreeting: {"नमस्ते", "नमस्कार", "प्रणाम", "हेलो", "हैलो", "हलो", "राम राम", "hello", "hi", "hii", "namaste", "namaskar", "pranam", "good morning"},
	CThanks: {"धन्यवाद", "शुक्रिया", "थैंक्यू", "थैंक यू", "thanks", "thank you", "thankyou", "dhanyavad", "dhanyawad", "shukriya"},
	CRepeat: {"दोबारा", "दुबारा", "फिर से", "फिरसे", "रिपीट", "repeat", "again", "dobara", "dubara", "phir se", "fir se", "समझ नहीं आया"},
	CHelp: {"मदद", "सहायता", "हेल्प", "help", "madad", "sahayata"},
	CEnd: {"बंद करो", "बंद कीजिए", "खत्म", "ख़त्म", "अलविदा", "बाय", "bye", "goodbye", "end", "stop", "exit", "alvida", "khatam", "band karo", "बस इतना", "बस"},
	CYes: {"हाँ", "हां", "हा", "जी हाँ", "हाँ जी", "ठीक है", "बिल्कुल", "yes", "haan", "han", "haa", "ha", "ok", "okay", "sure", "yeah", "हम्म"},
	CNo: {"नहीं", "नही", "ना", "जी नहीं", "no", "nahi", "nahin", "na", "nope"},
	CEnglish: {"english", "इंग्लिश", "अंग्रेजी", "अंग्रेज़ी", "angrezi", "angreji"},
	CHindi: {"hindi", "हिंदी", "हिन्दी"},
	CHuman: {"इंसान", "आदमी से", "कर्मचारी", "स्टाफ", "ऑपरेटर", "human", "person", "staff", "operator", "insaan", "insan", "karmchari", "किसी से बात", "असली व्यक्ति"},
	CKeyboard: {"कीबोर्ड", "टाइप", "keyboard", "type"},
	CMore: {"और", "more", "दूसरा", "अन्य", "other"},

	CEntertainment: {"फिल्म", "फ़िल्म", "मूवी", "गाना", "गाने", "सिनेमा", "क्रिकेट", "मैच", "गेम", "चुटकुला", "जोक", "movie", "film", "song", "songs", "gaana", "gana", "cricket", "match", "game", "joke", "chutkula", "ipl", "हीरो", "हीरोइन", "bhojpuri song"},
	CChitChat: {"कैसे हो", "क्या हाल", "कैसी हो", "मौसम", "how are you", "kaise ho", "kya haal", "weather", "बोर", "bored", "tum kaun ho"},
	CPolitics: {"चुनाव", "वोट", "पार्टी", "नेता", "मुख्यमंत्री कौन", "election", "vote", "party", "neta", "chunav", "bjp", "congress", "rjd", "jdu", "बीजेपी", "कांग्रेस", "राजद", "जदयू", "campaign", "प्रचार"},
	CReligion: {"भगवान", "मंदिर", "मस्जिद", "पूजा", "धर्म", "नमाज", "god", "temple", "mosque", "religion", "dharm", "dharma", "puja", "pooja", "allah", "church"},
	CMedical: {"दवा", "दवाई", "बीमारी", "बुखार", "खांसी", "दर्द", "लक्षण", "medicine", "disease", "fever", "symptom", "symptoms", "dawai", "dawa", "bukhar", "bimari", "pain", "दर्द है"},
	CInvestment: {"शेयर", "म्यूचुअल फंड", "निवेश", "क्रिप्टो", "बिटकॉइन", "share market", "stock", "stocks", "invest", "investment", "crypto", "bitcoin", "trading", "mutual fund", "nivesh"},
	CRelationship: {"प्यार", "गर्लफ्रेंड", "बॉयफ्रेंड", "ब्रेकअप", "love", "girlfriend", "boyfriend", "breakup", "pyaar", "pyar", "dating"},
	CCoding: {"कोडिंग", "कोड", "प्रोग्रामिंग", "प्रोग्राम", "सॉफ्टवेयर", "coding", "programming", "python", "javascript", "java", "html", "sql", "software", "code"},
	CNews: {"खबर", "ख़बर", "समाचार", "न्यूज़", "न्यूज", "गपशप", "news", "khabar", "gossip", "samachar"},
}

// Card-word confusions seen in STT output. These only become CCard when the
// previous token is a word that commonly precedes "card" (राशन कार्ड → राशन
// कोड), because on their own कोड/कोर्ट mean "code"/"court".
var cardConfusables = map[string]bool{}

// cardPrefixConcepts: concepts after which a confusable means "card".
var cardPrefixConcepts = map[string]bool{CRation: true, CAadhaar: true, CIdentity: true, CHealth: true, CFarmer: true, CJob: true, CLoan: true}

func init() {
	for _, w := range []string{"कोड", "कोर्ट", "कॉर्ड", "कोर्ड", "कार्ट", "काड", "कड", "code", "cord", "court", "kod", "cart", "kaad"} {
		cardConfusables[Fold(w)] = true
	}
}

var (
	lexOnce sync.Once
	lexV    *Vocab
)

// DefaultLexicon returns the built-in concept vocabulary.
func DefaultLexicon() *Vocab {
	lexOnce.Do(func() {
		keys := make([]string, 0, len(defaultLexicon))
		for k := range defaultLexicon {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var pairs [][2]string
		for _, k := range keys {
			for _, s := range defaultLexicon[k] {
				pairs = append(pairs, [2]string{s, k})
			}
		}
		lexV = NewVocab(pairs)
	})
	return lexV
}

// ConceptHit is a concept found in text.
type ConceptHit struct {
	Concept string
	Start   int // token index
	Words   int
	Fuzzy   bool
}

// Analysis is the result of Analyze.
type Analysis struct {
	Folded   string
	Tokens   []string
	Concepts []ConceptHit
	set      map[string]bool
}

// Has reports whether a concept was found.
func (a Analysis) Has(c string) bool { return a.set[c] }

// Count returns how many distinct concepts from cs were found.
func (a Analysis) Count(cs ...string) int {
	n := 0
	for _, c := range cs {
		if a.set[c] {
			n++
		}
	}
	return n
}

// ConceptSet returns the distinct concepts found.
func (a Analysis) ConceptSet() []string {
	out := make([]string, 0, len(a.set))
	for c := range a.set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Analyze folds s, tokenises it, finds lexicon concepts (exact, phrase and
// fuzzy) and applies the STT card-confusion rule.
func Analyze(s string) Analysis {
	toks := Tokens(s)
	if len(toks) > MaxInputTokens {
		toks = toks[:MaxInputTokens]
	}
	a := Analysis{Folded: Fold(s), Tokens: toks, set: map[string]bool{}}
	ms := DefaultLexicon().Find(toks)
	byStart := map[int][]string{}
	for _, m := range ms {
		a.Concepts = append(a.Concepts, ConceptHit{Concept: m.Payload, Start: m.Start, Words: m.Words, Fuzzy: m.Distance > 0})
		byStart[m.Start+m.Words-1] = append(byStart[m.Start+m.Words-1], m.Payload)
	}
	// STT confusion: "<card-prefix> कोड/कोर्ट/कॉर्ड" → card.
	for i := 1; i < len(toks); i++ {
		if !cardConfusables[toks[i]] {
			continue
		}
		prefix := false
		for _, c := range byStart[i-1] {
			if cardPrefixConcepts[c] {
				prefix = true
			}
		}
		if !prefix {
			continue
		}
		// replace any concept the confusable produced (coding) with card
		kept := a.Concepts[:0]
		for _, h := range a.Concepts {
			if h.Start == i && h.Words == 1 {
				continue
			}
			kept = append(kept, h)
		}
		a.Concepts = append(kept, ConceptHit{Concept: CCard, Start: i, Words: 1, Fuzzy: true})
	}
	for _, h := range a.Concepts {
		a.set[h.Concept] = true
	}
	return a
}

// HinglishKeywords returns the concept keys found in s — the language-
// neutral keyword tokens used for intent scoring and knowledge search.
func HinglishKeywords(s string) []string { return Analyze(s).ConceptSet() }
