package dialogue

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/knowledge"
)

// tx picks the language of a bilingual record string (falling back to the
// other language when one is missing).
func tx(t scheme.Text, l Lang) string {
	if l == English {
		if t.En != "" {
			return t.En
		}
		return t.Hi
	}
	if t.Hi != "" {
		return t.Hi
	}
	return t.En
}

func txp(t *scheme.Text, l Lang) string {
	if t == nil {
		return ""
	}
	return tx(*t, l)
}

func txs(ts []scheme.Text, l Lang) []string {
	var out []string
	for _, t := range ts {
		if s := tx(t, l); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func docs(ds []scheme.Doc, l Lang) []string {
	var out []string
	for _, d := range ds {
		s := tx(d.Name, l)
		if s == "" {
			continue
		}
		if n := txp(d.Note, l); n != "" {
			s += " — " + n
		}
		out = append(out, s)
	}
	return out
}

func section(title bi, l Lang, items []string) (CardSection, bool) {
	if len(items) == 0 {
		return CardSection{}, false
	}
	return CardSection{Title: title.in(l), Items: items}, true
}

// buildCard renders a record according to its presentation mode. In
// ModeNameOnly nothing but the name and the generic message leaves the
// record. extraNotes are general (non-record) notes such as "Aadhaar proves
// identity, not caste".
func buildCard(r scheme.Record, mode knowledge.Mode, l Lang, extraNotes []string) Card {
	c := Card{SchemeID: r.ID, Title: tx(r.Name, l)}
	if len(extraNotes) > 0 {
		c.Sections = append(c.Sections, CardSection{Title: m.sectionNote.in(l), Items: extraNotes})
	}
	if mode == knowledge.ModeNameOnly {
		c.Sections = append(c.Sections, CardSection{Title: m.sectionInfo.in(l), Items: []string{knowledge.NotVerifiedMessage(string(l))}})
		c.Disclaimer = knowledge.NotVerifiedMessage(string(l))
		return c
	}
	elig := txs(r.Eligibility, l)
	if s := txp(r.AgeRule, l); s != "" {
		elig = append(elig, s)
	}
	if s := txp(r.IncomeRule, l); s != "" {
		elig = append(elig, s)
	}
	add := func(title bi, items []string) {
		if s, ok := section(title, l, items); ok {
			c.Sections = append(c.Sections, s)
		}
	}
	add(m.sectionEligibility, elig)
	add(m.sectionBenefits, txs(r.Benefits, l))
	add(m.sectionDocs, docs(r.MandatoryDocs, l))
	add(m.sectionOptionalDocs, docs(r.OptionalDocs, l))
	add(m.sectionAltEvidence, txs(r.AlternativeEvidence, l))
	add(m.sectionSteps, txs(r.ApplicationSteps, l))
	add(m.sectionWhere, txs(r.WhereToApply, l))
	if s := txp(r.FeesNote, l); s != "" {
		add(m.sectionFees, []string{s})
	}
	add(m.sectionNote, txs(r.Exceptions, l))
	c.OfficialURL = r.OfficialURL
	c.Helpline = r.Helpline
	if mode == knowledge.ModeFactual {
		c.Verified = true
		c.LastVerified = r.LastVerified
		if r.LastVerified != "" {
			c.Disclaimer = fmt.Sprintf(m.factualDisclaimer.in(l), r.LastVerified)
		} else {
			c.Disclaimer = m.factualDisclaimerNoDate.in(l)
		}
	} else {
		c.Disclaimer = knowledge.UnverifiedDisclaimer(string(l))
	}
	return c
}

var urlRe = regexp.MustCompile(`(?i)\b(?:https?://|www\.)\S+`)

// speakable strips URLs and collapses whitespace for TTS.
func speakable(s string) string {
	s = urlRe.ReplaceAllString(s, "")
	return strings.Join(strings.Fields(s), " ")
}

// answerParts is the composed (template) answer for one record.
type answerParts struct {
	summary string // the part that may be rephrased: "<name>. <summary>"
	prefix  string // disclaimers / general notes shown before the summary
	notes   string // shown after the summary (ineligibility note)
	card    Card
	mode    knowledge.Mode
}

func composeAnswer(r scheme.Record, mode knowledge.Mode, l Lang, extraNotes []string, ineligible bool) answerParts {
	p := answerParts{mode: mode, card: buildCard(r, mode, l, extraNotes)}
	name := tx(r.Name, l)
	var pre []string
	if mode == knowledge.ModeUnverifiedDev {
		pre = append(pre, knowledge.UnverifiedDisclaimer(string(l)))
	}
	pre = append(pre, extraNotes...)
	p.prefix = strings.Join(pre, " ")
	if mode == knowledge.ModeNameOnly {
		p.summary = name + "। " + knowledge.NotVerifiedMessage(string(l))
		return p
	}
	sum := tx(r.Summary, l)
	if sum == "" {
		p.summary = name + "।"
	} else {
		p.summary = name + "। " + sum
	}
	if ineligible {
		p.notes = m.ineligibleNote.in(l)
	}
	return p
}

// render produces the final Text and Speech for an answer, using summary
// (template or validated rephrase).
func (p answerParts) render(l Lang, summary string) (text, speech string) {
	var tb, sb []string
	if p.prefix != "" {
		tb = append(tb, p.prefix)
		sb = append(sb, p.prefix)
	}
	tb = append(tb, summary)
	sb = append(sb, summary)
	if p.notes != "" {
		tb = append(tb, p.notes)
		sb = append(sb, p.notes)
	}
	if p.mode != knowledge.ModeNameOnly {
		tb = append(tb, m.cardsBelow.in(l))
		sb = append(sb, m.cardsBelowSpeech.in(l))
	}
	tb = append(tb, m.moreHelp.in(l))
	sb = append(sb, m.moreHelp.in(l))
	return strings.Join(tb, "\n\n"), speakable(strings.Join(sb, " "))
}
