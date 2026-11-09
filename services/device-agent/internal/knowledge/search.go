package knowledge

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/textnorm"
)

// The FTS5 table uses the unicode61 tokenizer, which splits Devanagari
// words at every dependent vowel sign (पेंशन → प | श | न …). To get whole-
// word tokens (and meaningful bm25 ranking) we index an ASCII encoding of
// each folded token instead of the raw text: Latin/digit tokens are kept
// as-is, any other token becomes "u" + the hex code points of its runes.
// Concept keys from textnorm are indexed as "zc<concept>" and the category
// as "zk<category>", so Hindi, Hinglish and English queries meet on the
// same tokens.
func ftsToken(tok string) string {
	ascii := true
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			ascii = false
			break
		}
	}
	if ascii {
		return tok
	}
	var b strings.Builder
	b.WriteByte('u')
	for _, r := range tok {
		fmt.Fprintf(&b, "%x", r)
	}
	return b.String()
}

func conceptToken(c string) string { return "zc" + strings.ReplaceAll(c, "_", "") }

// indexStrings returns the strings of a record that are searchable.
func indexStrings(r scheme.Record) (names []string, other []string) {
	names = append(names, r.Name.Hi, r.Name.En)
	names = append(names, r.Aliases...)
	other = append(other, r.Keywords...)
	return names, other
}

func indexText(r scheme.Record) string {
	names, other := indexStrings(r)
	var toks []string
	add := func(s string, weight int) {
		a := textnorm.Analyze(s)
		for w := 0; w < weight; w++ {
			for _, t := range a.Tokens {
				if !textnorm.IsStopword(t) {
					toks = append(toks, ftsToken(t))
				}
			}
			for _, c := range a.ConceptSet() {
				toks = append(toks, conceptToken(c))
			}
		}
	}
	for _, s := range names {
		add(s, 2) // names count double in bm25
	}
	for _, s := range other {
		add(s, 1)
	}
	toks = append(toks, "zk"+strings.ReplaceAll(r.Category, "_", ""))
	return strings.Join(toks, " ")
}

// rebuild recreates the in-memory name and term vocabularies.
func (s *Store) rebuild(recs []scheme.Record) {
	var namePairs, termPairs [][2]string
	for _, r := range recs {
		names, other := indexStrings(r)
		for _, n := range names {
			namePairs = append(namePairs, [2]string{n, r.ID})
		}
		for _, x := range append(names, other...) {
			for _, t := range textnorm.Tokens(x) {
				if !textnorm.IsStopword(t) {
					termPairs = append(termPairs, [2]string{t, t})
				}
			}
		}
	}
	names := textnorm.NewVocab(namePairs)
	terms := textnorm.NewVocab(termPairs)
	s.mu.Lock()
	s.names, s.terms = names, terms
	s.mu.Unlock()
}

// NameHit is a scheme name or alias recognised in text.
type NameHit struct {
	ID       string
	Term     string // folded name/alias that matched
	Distance int
}

// generic reports whether every token of a folded phrase is a function
// word or an ordinary topic word from the lexicon ("वृद्धावस्था पेंशन"):
// such aliases describe a category, not a specific scheme, and must not
// count as the citizen naming a scheme.
func generic(phrase string) bool {
	lex := textnorm.DefaultLexicon()
	for _, t := range strings.Fields(phrase) {
		if textnorm.IsStopword(t) {
			continue
		}
		if ms := lex.Find([]string{t}); len(ms) > 0 && ms[0].Distance == 0 {
			continue
		}
		if t == "योजना" || t == "yojana" || t == "scheme" || t == "yojna" {
			continue
		}
		return false
	}
	return true
}

// MatchNames finds specific scheme names/aliases in text (exact or within
// the fuzzy tolerance), ignoring generic category phrases.
func (s *Store) MatchNames(text string) []NameHit {
	s.mu.RLock()
	v := s.names
	s.mu.RUnlock()
	toks := textnorm.Tokens(text)
	var out []NameHit
	seen := map[string]bool{}
	for _, m := range v.Find(toks) {
		if seen[m.Payload] || generic(m.Term) {
			continue
		}
		seen[m.Payload] = true
		out = append(out, NameHit{ID: m.Payload, Term: m.Term, Distance: m.Distance})
	}
	return out
}

// Candidate is a search result.
type Candidate struct {
	Record    scheme.Record
	Score     float64 // higher is better
	NameMatch bool    // the citizen named this scheme
}

// SearchOptions narrows a search.
type SearchOptions struct {
	Limit      int
	Categories []string // if set, only these categories
}

// Search returns records ranked by relevance to free text (Hindi,
// Hinglish or English, possibly STT-garbled). Query tokens are folded,
// spelling-corrected against the indexed vocabulary and mapped to concept
// keys before the FTS5 query; scheme names found in the text get a bonus.
func (s *Store) Search(ctx context.Context, text string, o SearchOptions) ([]Candidate, error) {
	if o.Limit <= 0 || o.Limit > MaxSearchResults {
		o.Limit = 5
	}
	s.mu.RLock()
	terms := s.terms
	s.mu.RUnlock()

	a := textnorm.Analyze(text)
	qset := map[string]bool{}
	var content []string
	for _, t := range a.Tokens {
		if !textnorm.IsStopword(t) {
			content = append(content, t)
		}
	}
	for _, t := range content {
		qset[ftsToken(t)] = true
	}
	for _, m := range terms.Find(content) {
		for _, t := range strings.Fields(m.Term) {
			qset[ftsToken(t)] = true
		}
	}
	for _, c := range a.ConceptSet() {
		qset[conceptToken(c)] = true
	}
	scores := map[string]float64{}
	if len(qset) > 0 {
		q := make([]string, 0, len(qset))
		for t := range qset {
			q = append(q, `"`+strings.ReplaceAll(t, `"`, `""`)+`"`)
		}
		sort.Strings(q)
		if len(q) > maxQueryTerms {
			q = q[:maxQueryTerms]
		}
		rows, err := s.db.QueryContext(ctx, `SELECT id, bm25(schemes_fts) FROM schemes_fts WHERE schemes_fts MATCH ? ORDER BY bm25(schemes_fts) LIMIT ?`,
			strings.Join(q, " OR "), maxFTSRows)
		if err != nil {
			return nil, fmt.Errorf("knowledge: search: %w", err)
		}
		for rows.Next() {
			var id string
			var bm float64
			if err := rows.Scan(&id, &bm); err != nil {
				rows.Close()
				return nil, err
			}
			scores[id] = -bm // bm25() is lower-is-better
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	named := map[string]bool{}
	for _, h := range s.MatchNames(text) {
		named[h.ID] = true
		scores[h.ID] += 10
	}
	ids := sortedKeys(scores)
	sort.SliceStable(ids, func(i, j int) bool { return scores[ids[i]] > scores[ids[j]] })
	var out []Candidate
	for _, id := range ids {
		if len(out) >= o.Limit {
			break
		}
		r, err := s.Get(ctx, id)
		if err != nil {
			continue // row vanished between queries (concurrent reload)
		}
		if len(o.Categories) > 0 && !containsStr(o.Categories, r.Category) {
			continue
		}
		out = append(out, Candidate{Record: r, Score: scores[id], NameMatch: named[id]})
	}
	return out, nil
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
