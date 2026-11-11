package main

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/janmanindia/jan-sahayak/internal/scheme"
)

// Number-support check.
//
// Drafting rule: every fact (amount, age, limit, deadline, phone number)
// comes from a fetched official page and is backed by a verbatim excerpt in
// sources[].excerpt. Code cannot check that a fact is true, but it can check
// that every number shown to a citizen also appears in at least one excerpt
// of the same record. A number that appears in no excerpt is either a
// typo, a conversion the reviewer must check, or an unsupported fact.
//
// Normalisation (applied to both sides):
//   - Host names and URLs are ignored ("7nishchay-yuvaupmission.bihar.gov.in"
//     is an address, not a fact).
//   - Devanagari digits ०-९ become ASCII 0-9.
//   - Indian/Western digit grouping is removed: "1,20,000" -> "120000".
//   - Decimals keep their fraction with trailing zeros dropped: "1.20" -> "1.2".
//   - Leading zeros are dropped: "06" -> "6".
//   - Phone-style groups ("1800-345-6444", "1800 345 6444") are joined into
//     one number when they total 8 or more digits.
//
// Excerpts only (they are verbatim quotes, so they may spell numbers out):
//   - Number words one..twelve / एक..बारह count as their value, and a number
//     or number word followed by hundred/thousand/lakh/crore (सौ, हजार,
//     लाख, करोड़) also counts as the multiplied value: "Rupees One Thousand"
//     supports "₹1000", "चार लाख" supports "₹4 lakh", "Rs. 1.20 lakh"
//     supports "120000".
//   - Roman ordinals such as "Xth" / "XIIth" count as 10 / 12.
//   This only widens what an excerpt can support; it never hides a number in
//   citizen-facing text. Some of these words have other meanings (Hindi
//   "एक" is also "a", "दो" is also "give"), so a small number can pass
//   because of an unrelated word; reviewers still read every excerpt.
//
// Number words in citizen-facing text ("one year", "एक साल") are not
// checked, for the same reason. Reviewers check those by hand
// (REVIEW_CHECKLIST.md).

var (
	numTokenRe = regexp.MustCompile(`\d+(?:[,.]\d+)*`)
	phoneRunRe = regexp.MustCompile(`\d[\d \-]{6,}\d`)
	hostRe     = regexp.MustCompile(`(?i)(?:https?://)?(?:[a-z0-9-]+\.)+(?:in|org|com|net)\b(?:/[^\s)]*)?`)
	wordRe     = regexp.MustCompile(`[\p{L}\p{M}]+|\d+(?:[,.]\d+)*`)
	romanOrdRe = regexp.MustCompile(`^([ivxl]+)th$`)
)

var devanagariDigits = strings.NewReplacer(
	"०", "0", "१", "1", "२", "2", "३", "3", "४", "4",
	"५", "5", "६", "6", "७", "7", "८", "8", "९", "9",
)

var numberWords = map[string]float64{
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6,
	"seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12,
	"एक": 1, "दो": 2, "तीन": 3, "चार": 4, "पांच": 5, "पाँच": 5, "छह": 6, "छः": 6,
	"सात": 7, "आठ": 8, "नौ": 9, "दस": 10, "ग्यारह": 11, "बारह": 12,
}

var multiplierWords = map[string]float64{
	"hundred": 100, "thousand": 1000, "lakh": 1e5, "lakhs": 1e5, "crore": 1e7, "crores": 1e7,
	"सौ": 100, "हजार": 1000, "हज़ार": 1000, "लाख": 1e5, "करोड़": 1e7, "करोड": 1e7,
}

// cleanForNumbers removes addresses and maps Devanagari digits to ASCII.
func cleanForNumbers(s string) string {
	s = hostRe.ReplaceAllString(s, " ")
	return devanagariDigits.Replace(s)
}

// numberTokens returns the normalised numbers in s, in order of appearance,
// without duplicates.
func numberTokens(s string) []string {
	s = cleanForNumbers(s)
	var out []string
	seen := map[string]bool{}
	add := func(t string) {
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	// Phone-style groups are joined first so that "1800-345-6444" and
	// "18003456444" compare equal (and the parts are not checked alone).
	s = phoneRunRe.ReplaceAllStringFunc(s, func(m string) string {
		d := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, m)
		if len(d) >= 8 {
			return d
		}
		return m
	})
	for _, m := range numTokenRe.FindAllString(s, -1) {
		add(normaliseNumber(m))
	}
	return out
}

// excerptNumbers returns every number an excerpt supports: its digit
// numbers plus the spelled-out forms described above.
func excerptNumbers(s string) map[string]bool {
	set := map[string]bool{}
	for _, t := range numberTokens(s) {
		set[t] = true
	}
	words := wordRe.FindAllString(strings.ToLower(cleanForNumbers(s)), -1)
	for i, w := range words {
		v, ok := wordValue(w)
		if !ok {
			continue
		}
		set[formatNumber(v)] = true
		if i+1 < len(words) {
			if m, ok := multiplierWords[words[i+1]]; ok {
				set[formatNumber(v*m)] = true
			}
		}
	}
	return set
}

// wordValue is the value of a digit token, a number word or a Roman ordinal.
func wordValue(w string) (float64, bool) {
	if v, ok := numberWords[w]; ok {
		return v, true
	}
	if m := romanOrdRe.FindStringSubmatch(w); m != nil {
		return romanValue(m[1])
	}
	if w[0] >= '0' && w[0] <= '9' {
		v, err := strconv.ParseFloat(normaliseNumber(w), 64)
		return v, err == nil
	}
	return 0, false
}

// romanValue parses a small Roman numeral (i..l).
func romanValue(s string) (float64, bool) {
	val := map[byte]int{'i': 1, 'v': 5, 'x': 10, 'l': 50}
	total := 0
	for i := 0; i < len(s); i++ {
		v := val[s[i]]
		if i+1 < len(s) && val[s[i+1]] > v {
			total -= v
		} else {
			total += v
		}
	}
	return float64(total), total > 0
}

// formatNumber renders v in the normalised form used by numberTokens.
func formatNumber(v float64) string {
	v = math.Round(v*100) / 100
	return normaliseNumber(strconv.FormatFloat(v, 'f', -1, 64))
}

// normaliseNumber turns a matched token into its canonical form. A trailing
// sentence full stop never reaches here because the regexp needs a digit
// after "." or ",".
func normaliseNumber(m string) string {
	// The last separator decides: "1,20,000" is grouping, "1.20" and
	// "12,000.50" have a decimal fraction. A "," before exactly 1-2 trailing
	// digits after a "." group is not handled specially; such forms do not
	// occur in the records.
	intPart, frac := m, ""
	if i := strings.LastIndexByte(m, '.'); i >= 0 {
		intPart, frac = m[:i], m[i+1:]
	}
	intPart = strings.NewReplacer(",", "", ".", "").Replace(intPart)
	intPart = strings.TrimLeft(intPart, "0")
	if intPart == "" {
		intPart = "0"
	}
	frac = strings.TrimRight(frac, "0")
	if frac == "" {
		return intPart
	}
	return intPart + "." + frac
}

// citizenTexts lists every citizen-facing text of a record with a label for
// error messages. aliases/keywords are speech-matching aids, not facts, and
// are not included; helpline is.
func citizenTexts(r scheme.Record) []labelled {
	var out []labelled
	addT := func(label string, t scheme.Text) {
		out = append(out, labelled{label + ".hi", t.Hi}, labelled{label + ".en", t.En})
	}
	addList := func(label string, ts []scheme.Text) {
		for i, t := range ts {
			addT(fmt.Sprintf("%s[%d]", label, i), t)
		}
	}
	addT("name", r.Name)
	addT("department", r.Department)
	addT("summary", r.Summary)
	addList("eligibility", r.Eligibility)
	addList("benefits", r.Benefits)
	if r.AgeRule != nil {
		addT("age_rule", *r.AgeRule)
	}
	if r.IncomeRule != nil {
		addT("income_rule", *r.IncomeRule)
	}
	for i, q := range r.Questions {
		addT(fmt.Sprintf("questions[%d].ask", i), q.Ask)
		addList(fmt.Sprintf("questions[%d].choices", i), q.Choices)
	}
	for _, d := range []struct {
		label string
		docs  []scheme.Doc
	}{{"mandatory_docs", r.MandatoryDocs}, {"optional_docs", r.OptionalDocs}} {
		for i, doc := range d.docs {
			addT(fmt.Sprintf("%s[%d].name", d.label, i), doc.Name)
			if doc.Note != nil {
				addT(fmt.Sprintf("%s[%d].note", d.label, i), *doc.Note)
			}
		}
	}
	addList("alternative_evidence", r.AlternativeEvidence)
	addList("application_steps", r.ApplicationSteps)
	addList("where_to_apply", r.WhereToApply)
	if r.FeesNote != nil {
		addT("fees_note", *r.FeesNote)
	}
	addList("exceptions", r.Exceptions)
	if r.Helpline != "" {
		out = append(out, labelled{"helpline", r.Helpline})
	}
	return out
}

type labelled struct{ label, text string }

// sourceSupport returns every source-support finding for r: numbers that
// appear in no excerpt, and an official_url on a site no source was read
// from. validate prints them as warnings; with -strict they are errors.
func sourceSupport(r scheme.Record) []string {
	msgs := unsupportedNumbers(r)
	if m := unreadOfficialURL(r); m != "" {
		msgs = append(msgs, m)
	}
	return msgs
}

// unreadOfficialURL reports an official_url whose host is not the host of
// any source: the page was never actually read, so citizens could be sent
// to a site nobody checked.
func unreadOfficialURL(r scheme.Record) string {
	if r.OfficialURL == "" {
		return ""
	}
	ou, err := url.Parse(r.OfficialURL)
	if err != nil {
		return "" // Validate reports malformed URLs
	}
	for _, s := range r.Sources {
		if su, err := url.Parse(s.URL); err == nil && strings.EqualFold(su.Hostname(), ou.Hostname()) {
			return ""
		}
	}
	return fmt.Sprintf("official_url host %s is not the host of any source — read the page and cite it, or leave official_url empty", ou.Hostname())
}

// unsupportedNumbers returns one message per (field, number) where the
// number appears in no source excerpt of the record.
func unsupportedNumbers(r scheme.Record) []string {
	supported := map[string]bool{}
	for _, s := range r.Sources {
		for t := range excerptNumbers(s.Excerpt) {
			supported[t] = true
		}
	}
	var msgs []string
	for _, lt := range citizenTexts(r) {
		for _, t := range numberTokens(lt.text) {
			if !supported[t] {
				msgs = append(msgs, fmt.Sprintf("number %q in %s appears in no source excerpt — add a verbatim excerpt or remove the fact", t, lt.label))
			}
		}
	}
	return msgs
}
