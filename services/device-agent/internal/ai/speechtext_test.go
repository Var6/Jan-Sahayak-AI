package ai

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// norm makes expectations independent of how nukta letters were typed.
func norm(s string) string { return decomposeNukta(s) }

func TestHindiNumber(t *testing.T) {
	cases := map[uint64]string{
		0:          "शून्य",
		1:          "एक",
		9:          "नौ",
		15:         "पंद्रह",
		45:         "पैंतालीस",
		70:         "सत्तर",
		99:         "निन्यानवे",
		100:        "एक सौ",
		101:        "एक सौ एक",
		250:        "दो सौ पचास",
		1000:       "एक हज़ार",
		1500:       "एक हज़ार पाँच सौ",
		6000:       "छह हज़ार",
		10000:      "दस हज़ार",
		99999:      "निन्यानवे हज़ार नौ सौ निन्यानवे",
		100000:     "एक लाख",
		120000:     "एक लाख बीस हज़ार",
		250000:     "दो लाख पचास हज़ार",
		10000000:   "एक करोड़",
		12345678:   "एक करोड़ तेईस लाख पैंतालीस हज़ार छह सौ अठहत्तर",
		1500000000: "एक सौ पचास करोड़",
	}
	for n, want := range cases {
		if got := HindiNumber(n); got != norm(want) {
			t.Errorf("HindiNumber(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestHindiOnesTableShape(t *testing.T) {
	seen := map[string]int{}
	for i, w := range hindiOnes {
		if w == "" {
			t.Fatalf("empty word for %d", i)
		}
		if j, dup := seen[w]; dup {
			t.Fatalf("%d and %d share %q", i, j, w)
		}
		seen[w] = i
	}
}

func TestPrepareForTTS(t *testing.T) {
	cases := []struct{ in, want string }{
		// plain numbers & grouping
		{"0", "शून्य"},
		{"100", "एक सौ"},
		{"1000", "एक हज़ार"},
		{"1,00,000", "एक लाख"},
		{"1,00,00,000", "एक करोड़"},
		{"1,20,000", "एक लाख बीस हज़ार"},
		{"120,000", "एक लाख बीस हज़ार"},
		{"मेरी माँ की उम्र 70 साल है।", "मेरी माँ की उम्र सत्तर साल है।"},
		{"कक्षा 10वीं पास", "कक्षा दसवीं पास"},
		{"1, 2 और 3", "एक, दो और तीन"},
		{"1,2,3", "एक, दो, तीन"},
		// decimals
		{"2.5", "दो दशमलव पाँच"},
		{"0.75 एकड़", "शून्य दशमलव सात पाँच एकड़"},
		// digit-by-digit
		{"9876543210", "नौ आठ सात छह पाँच चार तीन दो एक शून्य"},
		{"कोड 007", "कोड शून्य शून्य सात"},
		{"1800-180-1551 पर फ़ोन करें", "एक आठ शून्य शून्य, एक आठ शून्य, एक पाँच पाँच एक पर फ़ोन करें"},
		// ranges
		{"18-60 वर्ष", "अठारह से साठ वर्ष"},
		// currency
		{"₹6,000 प्रति वर्ष", "छह हज़ार रुपये प्रति वर्ष"},
		{"₹ 500", "पाँच सौ रुपये"},
		{"Rs. 1,20,000", "एक लाख बीस हज़ार रुपये"},
		{"rs 250/-", "दो सौ पचास रुपये"},
		{"INR 1000", "एक हज़ार रुपये"},
		{"रु. 400", "चार सौ रुपये"},
		{"₹2 लाख तक", "दो लाख रुपये तक"},
		{"₹2.5 lakh", "दो दशमलव पाँच लाख रुपये"},
		{"₹12.50", "बारह रुपये पचास पैसे"},
		{"₹12.00", "बारह रुपये"},
		{"500 रु.", "पाँच सौ रुपये"},
		{"BDO", "बीडीओ"},
		{"NREGA", "NREGA"},
		{"500 रुपये", "पाँच सौ रुपये"},
		{"6000/-", "छह हज़ार"},
		// dates
		{"15/12/2026", "पंद्रह दिसंबर दो हज़ार छब्बीस"},
		{"01-04-2025 से", "एक अप्रैल दो हज़ार पच्चीस से"},
		{"15 दिसंबर 2026", "पंद्रह दिसंबर दो हज़ार छब्बीस"},
		{"15 दिसम्बर, 2026", "पंद्रह दिसंबर दो हज़ार छब्बीस"},
		{"1 January 1998", "एक जनवरी उन्नीस सौ अट्ठानवे"},
		{"December 5, 2026", "पाँच दिसंबर दो हज़ार छब्बीस"},
		{"31 मार्च तक", "इकतीस मार्च तक"},
		{"मार्च 2027", "मार्च दो हज़ार सत्ताईस"},
		{"सन 1998 में", "सन उन्नीस सौ अट्ठानवे में"},
		{"45/13/2026", "पैंतालीस तेरह दो हज़ार छब्बीस"},
		// times
		{"10:30 बजे", "दस बजकर तीस मिनट बजे"},
		{"10:00 AM", "सुबह दस बजे"},
		{"5 PM तक", "शाम पाँच बजे तक"},
		{"2:15 pm", "दोपहर दो बजकर पंद्रह मिनट"},
		// percent
		{"50%", "पचास प्रतिशत"},
		{"2.5 %", "दो दशमलव पाँच प्रतिशत"},
		// acronyms & words
		{"PM Kisan", "पीएम किसान"},
		{"PM-KISAN", "पीएम-किसान"},
		{"CSC केंद्र पर RTPS से", "सीएससी केंद्र पर आरटीपीएस से"},
		{"Aadhaar card और aadhar", "आधार कार्ड और आधार"},
		{"OTP, ATM, SMS", "ओटीपी, एटीएम, एसएमएस"},
		{"BPL, SC, ST, OBC, EWS", "बीपीएल, एससी, एसटी, ओबीसी, ईडब्ल्यूएस"},
		{"KYC और e-KYC और eKYC", "केवाईसी और ई-केवाईसी और ई-केवाईसी"},
		{"online form", "ऑनलाइन फ़ॉर्म"},
		{"UPI", "यूपीआई"},
		{"NFSA", "एनएफ़एसए"},
		{"MGNREGA", "MGNREGA"},
		{"pm st sc", "pm st sc"},
		// URLs & e-mail
		{"https://pmkisan.gov.in पर जाएँ", "वेबसाइट पर जाएँ"},
		{"देखें: https://example.gov.in/a?b=1।", "देखें: वेबसाइट।"},
		{"www.example.com देखें", "वेबसाइट देखें"},
		{"pmkisan.gov.in पर", "वेबसाइट पर"},
		{"help@example.gov.in पर लिखें", "ईमेल पता पर लिखें"},
		// symbols & whitespace
		{"  नमस्ते \t  जी  ", "नमस्ते जी"},
		{"पहली पंक्ति\nदूसरी पंक्ति", "पहली पंक्ति। दूसरी पंक्ति"},
		{"हाँ।\nठीक", "हाँ। ठीक"},
		{"**ज़रूरी** दस्तावेज़ (कॉपी)", "ज़रूरी दस्तावेज़ कॉपी"},
		{"राशन & पेंशन", "राशन और पेंशन"},
		{"१५ दिन", "पंद्रह दिन"},
		{"", ""},
	}
	for _, c := range cases {
		got := PrepareForTTS(c.in)
		if got != norm(c.want) {
			t.Errorf("PrepareForTTS(%q)\n got  %q\n want %q", c.in, got, norm(c.want))
		}
	}
}

func TestPrepareForTTSNoDigitsAndBounded(t *testing.T) {
	long := strings.Repeat("योजना में ₹6,000 मिलते हैं। ", 100)
	got := PrepareForTTS(long)
	if n := utf8.RuneCountInString(got); n > MaxTTSRunes || n < MaxTTSRunes/2 {
		t.Fatalf("length %d", n)
	}
	if !strings.HasSuffix(got, "।") {
		t.Fatalf("not cut at a sentence end: %q", got[len(got)-30:])
	}
	if strings.ContainsAny(got, "0123456789₹") {
		t.Fatalf("digits left: %q", got)
	}
	// no sentence boundary at all → cut at a space
	got = PrepareForTTS(strings.Repeat("शब्द ", 400))
	if n := utf8.RuneCountInString(got); n > MaxTTSRunes {
		t.Fatalf("length %d", n)
	}
}

func TestPrepareForTTSIdempotent(t *testing.T) {
	for _, in := range []string{"₹1,20,000 15/12/2026 को PM Kisan से 50% मिला।", "CSC पर e-KYC 10:30 AM"} {
		once := PrepareForTTS(in)
		if twice := PrepareForTTS(once); twice != once {
			t.Errorf("not idempotent:\n%q\n%q", once, twice)
		}
	}
}

func FuzzPrepareForTTS(f *testing.F) {
	for _, s := range []string{"₹1,20,000", "15/12/2026", "1800-180-1551", "https://a.gov.in।", "10:30 PM", "e-KYC", "१२३", "1,2,3.4.5", "99999999999999999999999"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := PrepareForTTS(s)
		if !utf8.ValidString(out) {
			t.Fatalf("invalid UTF-8 for %q", s)
		}
		if utf8.RuneCountInString(out) > MaxTTSRunes {
			t.Fatalf("too long for %q", s)
		}
		if strings.ContainsAny(out, "0123456789") {
			t.Fatalf("digits left for %q: %q", s, out)
		}
	})
}
