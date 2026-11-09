package textnorm

import (
	"strings"
	"testing"
)

func TestFoldRules(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"nukta composed (precomposed U+095B)", "ज़रूरी", "जरुरि"},
		{"nukta decomposed", "ज़रूरी", "जरुरि"},
		{"nfc-composed nukta letter", "ऩ", "न"},
		{"ड़ → ड", "खगड़िया", "खगडिया"},
		{"ढ़ → ढ", "पढ़ाई", "पढाइ"},
		{"chandrabindu → anusvara", "माँ", "मां"},
		{"ी/ि folding", "जाती", "जाति"},
		{"ू/ु folding", "बूढ़ा", "बुढा"},
		{"ण → न", "प्रमाण", "प्रमान"},
		{"श/ष → स", "पेंशन विशेष", "पेंसन विसेस"},
		{"danda and punctuation removed", "मुझे लोन चाहिए।", "मुझे लोन चाहिए"},
		{"latin lowercased", "Caste CERTIFICATE!!", "caste certificate"},
		{"homorganic nasal → anusvara", "पेन्शन हिन्दी", "पेंसन हिंदि"},
		{"geminate aspirate", "वृद्धावस्था", "व्रिधावस्था"},
		{"vocalic r", "ऋण", "रिन"},
		{"candra o", "कॉलेज", "कालेज"},
		{"devanagari digits", "१२३४", "1234"},
		{"zwj stripped", "क्‍ष", "क्स"},
		{"whitespace collapsed", "  लोन \t\n  चाहिए  ", "लोन चाहिए"},
		{"mixed script", "मुझे loan चाहिए", "मुझे loan चाहिए"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Fold(c.in); got != c.want {
				t.Fatalf("Fold(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestFoldEquivalences(t *testing.T) {
	// Each pair must fold to the same string (STT/spelling variants).
	pairs := [][2]string{
		{"जाती प्रमान पत्र", "जाति प्रमाण पत्र"},
		{"व्रिधावस्था", "वृद्धावस्था"},
		{"पेन्शन", "पेंशन"},
		{"हिन्दी", "हिंदी"},
		{"ज़मीन", "जमीन"},
		{"सत्तर", "सतर"},
	}
	for _, p := range pairs {
		if Fold(p[0]) != Fold(p[1]) {
			t.Errorf("Fold(%q)=%q != Fold(%q)=%q", p[0], Fold(p[0]), p[1], Fold(p[1]))
		}
	}
}

func TestFoldBoundedAndValidUTF8(t *testing.T) {
	long := strings.Repeat("लोन ", 20000)
	f := Fold(long)
	if len(f) > MaxFoldInputBytes {
		t.Fatalf("output %d bytes exceeds bound", len(f))
	}
	if !strings.HasPrefix(f, "लोन लोन") {
		t.Fatalf("unexpected prefix %q", f[:30])
	}
}

func TestDistance(t *testing.T) {
	cases := []struct {
		a, b   string
		max, d int
	}{
		{"kitten", "sitting", -1, 3},
		{"पेसन", "पेंसन", -1, 1},
		{"", "abc", -1, 3},
		{"abc", "abc", 0, 0},
		{"abcdef", "uvwxyz", 2, 3}, // early exit returns max+1
		{"a", "abcdef", 2, 3},
	}
	for _, c := range cases {
		if got := Distance(c.a, c.b, c.max); got != c.d {
			t.Errorf("Distance(%q,%q,%d)=%d want %d", c.a, c.b, c.max, got, c.d)
		}
	}
}

func TestTokensAndBigrams(t *testing.T) {
	toks := Tokens("मुझे जाति प्रमाण पत्र बनवाना है।")
	want := []string{"मुझे", "जाति", "प्रमान", "पत्र", "बनवाना", "है"}
	if strings.Join(toks, "|") != strings.Join(want, "|") {
		t.Fatalf("tokens %v", toks)
	}
	bg := Bigrams(toks)
	if len(bg) != 5 || bg[1] != "जाति प्रमान" {
		t.Fatalf("bigrams %v", bg)
	}
	if n := NGrams([]string{"a", "b", "c"}, 2); len(n) != 5 {
		t.Fatalf("ngrams %v", n)
	}
	ct := ContentTokens("मुझे लोन चाहिए")
	if len(ct) != 1 || ct[0] != "लोन" {
		t.Fatalf("content tokens %v", ct)
	}
}

func TestConceptsFromSTTAndHinglish(t *testing.T) {
	cases := []struct {
		in   string
		want []string // all must be present
		not  []string // none may be present
	}{
		// Synthetic test clips (exact text)
		{"मुझे लोन चाहिए।", []string{CLoan}, nil},
		{"मुझे जाति प्रमाण पत्र बनवाना है।", []string{CCaste, CCertificate}, nil},
		{"मेरी माँ की उम्र सत्तर साल है, उन्हें वृद्धावस्था पेंशन कैसे मिलेगी?", []string{CElderly, CPension}, nil},
		{"मैं गया जिले का किसान हूँ, पीएम किसान का पैसा नहीं आया।", []string{CFarmer, CDistrict, CNotReceived}, nil},
		// STT errors observed in testing
		{"मुझी लोन", []string{CLoan}, nil},
		{"जाती प्रमान पत्र", []string{CCaste, CCertificate}, nil},
		{"व्रिधावस्था पेशन", []string{CElderly, CPension}, nil},
		{"पीम किसान", []string{CFarmer}, nil},
		{"राशन कोड बनवाना है", []string{CRation, CCard}, []string{CCoding}},
		{"राशन कोर्ट", []string{CRation, CCard}, nil},
		{"आधार कॉर्ड", []string{CAadhaar, CCard}, nil},
		{"मुझे कोड सीखना है", []string{CCoding}, []string{CCard}},
		// Hinglish romanised
		{"mujhe loan chahiye", []string{CLoan}, nil},
		{"lon chahiye", []string{CLoan}, nil},
		{"karza lena hai", []string{CLoan}, nil},
		{"karj chahiye", []string{CLoan}, nil},
		{"penshan kaise milegi", []string{CPension}, nil},
		{"pension for widow", []string{CPension, CWidow}, nil},
		{"jaati praman patra", []string{CCaste, CCertificate}, nil},
		{"caste certificate", []string{CCaste, CCertificate}, nil},
		{"scholarship chahiye", []string{CScholarship}, nil},
		{"kisan hu", []string{CFarmer}, nil},
		// negatives
		{"क्रिकेट मैच कब है", []string{CEntertainment}, []string{CLoan, CPension}},
		{"लाल", nil, []string{CLoan}}, // short words never fuzzy-match
	}
	for _, c := range cases {
		a := Analyze(c.in)
		for _, w := range c.want {
			if !a.Has(w) {
				t.Errorf("Analyze(%q): missing %q (got %v)", c.in, w, a.ConceptSet())
			}
		}
		for _, n := range c.not {
			if a.Has(n) {
				t.Errorf("Analyze(%q): unexpected %q (got %v)", c.in, n, a.ConceptSet())
			}
		}
	}
}

func TestVocabFuzzyPhrases(t *testing.T) {
	v := NewVocab([][2]string{
		{"पीएम किसान", "rec-a"},
		{"खगड़िया", "khagaria"},
		{"वृद्धावस्था पेंशन", "rec-b"},
		{"लोन", "loan"},
	})
	cases := []struct{ in, want string }{
		{"पीम किसान का पैसा", "rec-a"},
		{"खगरिया जिला", "khagaria"},
		{"व्रिधावस्था पेशन", "rec-b"},
		{"मुझी लोन", "loan"},
	}
	for _, c := range cases {
		ms := v.Find(Tokens(c.in))
		found := false
		for _, m := range ms {
			if m.Payload == c.want {
				found = true
			}
		}
		if !found {
			t.Errorf("Find(%q) = %+v, want payload %q", c.in, ms, c.want)
		}
	}
	if ms := v.Find(Tokens("लाल")); len(ms) != 0 {
		t.Errorf("short word fuzzy matched: %+v", ms)
	}
	if _, ok := v.Closest("खगडीया"); !ok {
		t.Error("Closest failed")
	}
}

func TestDevanagariRatio(t *testing.T) {
	if r := DevanagariRatio("यह योजना है"); r < 0.99 {
		t.Fatalf("ratio %f", r)
	}
	if r := DevanagariRatio("this is english"); r != 0 {
		t.Fatalf("ratio %f", r)
	}
	if r := DevanagariRatio("123"); r != 0 {
		t.Fatalf("ratio %f", r)
	}
}

func BenchmarkAnalyze(b *testing.B) {
	s := "मेरी माँ की उम्र सत्तर साल है, उन्हें वृद्धावस्था पेंशन कैसे मिलेगी?"
	for b.Loop() {
		Analyze(s)
	}
}
