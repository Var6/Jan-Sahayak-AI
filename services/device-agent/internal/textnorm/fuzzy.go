package textnorm

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// Distance returns the Levenshtein edit distance between a and b in runes.
// It stops early and returns max+1 once the distance is known to exceed max
// (max < 0 means unbounded). Memory is O(len(b)).
func Distance(a, b string, max int) int {
	ra, rb := []rune(a), []rune(b)
	if max >= 0 && abs(len(ra)-len(rb)) > max {
		return max + 1
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			rowMin = min(rowMin, cur[j])
		}
		if max >= 0 && rowMin > max {
			return max + 1
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// MaxEdits is the fuzzy tolerance for a folded term of n runes: short words
// must match exactly (लोन ≠ लाल), longer words tolerate one or two edits.
func MaxEdits(n int) int {
	switch {
	case n <= 3:
		return 0
	case n <= 6:
		return 1
	case n <= 12:
		return 2
	default:
		return 3
	}
}

// MaxExactLatinRunes: romanised (Latin-script) words of up to this many
// letters are matched exactly, never fuzzily. Short romanised Hindi words
// are too close to each other for edit distance to mean anything: "pati"
// (husband) is one edit from "jati" (caste), "mar" (die) from "maar"
// (beat). A misroute there can turn a safety disclosure into a welfare
// answer, so these words must be spelled as listed.
const MaxExactLatinRunes = 4

// fuzzyEdits is MaxEdits for a folded term, with short Latin terms forced
// to exact matching (see MaxExactLatinRunes).
func fuzzyEdits(folded string, runes int) int {
	if runes <= MaxExactLatinRunes && isLatin(folded) {
		return 0
	}
	return MaxEdits(runes)
}

// isLatin reports whether s contains only ASCII letters, digits and spaces.
func isLatin(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == ' ') {
			return false
		}
	}
	return true
}

// Vocab is a fuzzy-matchable set of folded terms (single words or
// multi-word phrases). Each term maps to an arbitrary payload string (a
// concept key, a record id …). It is immutable after Build and safe for
// concurrent reads.
type Vocab struct {
	terms []vterm
	exact map[string][]int
	words map[int][]int // number of words → term indexes
	maxW  int
}

type vterm struct {
	folded  string
	runes   int
	words   int
	payload string
}

// Match is a vocabulary hit.
type Match struct {
	Term     string // folded vocabulary term
	Payload  string
	Input    string // folded input n-gram that matched
	Distance int
	Start    int // token index in the input where the n-gram starts
	Words    int // n-gram length in tokens
}

// MaxVocabTerms bounds memory for a vocabulary.
const MaxVocabTerms = 50000

// NewVocab builds a vocabulary from (surface form → payload) pairs. Surface
// forms are folded; empty forms are ignored; at most MaxVocabTerms are kept.
func NewVocab(pairs [][2]string) *Vocab {
	v := &Vocab{exact: map[string][]int{}, words: map[int][]int{}}
	seen := map[string]bool{}
	for _, p := range pairs {
		f := Fold(p[0])
		if f == "" || seen[f+"\x00"+p[1]] {
			continue
		}
		if len(v.terms) >= MaxVocabTerms {
			break
		}
		seen[f+"\x00"+p[1]] = true
		w := strings.Count(f, " ") + 1
		v.terms = append(v.terms, vterm{folded: f, runes: utf8.RuneCountInString(f), words: w, payload: p[1]})
		idx := len(v.terms) - 1
		v.exact[f] = append(v.exact[f], idx)
		v.words[w] = append(v.words[w], idx)
		v.maxW = max(v.maxW, w)
	}
	return v
}

// Len returns the number of terms.
func (v *Vocab) Len() int { return len(v.terms) }

// MaxInputTokens bounds how many input tokens Find examines.
const MaxInputTokens = 128

// Find matches every n-gram of the folded tokens (n up to the longest
// vocabulary phrase) against the vocabulary, exactly first and then within
// MaxEdits. Longer and closer matches win; overlapping hits for the same
// tokens keep only the best. Results are ordered by start position.
func (v *Vocab) Find(toks []string) []Match {
	if v == nil || len(v.terms) == 0 || len(toks) == 0 {
		return nil
	}
	if len(toks) > MaxInputTokens {
		toks = toks[:MaxInputTokens]
	}
	var cands []Match
	for n := min(v.maxW, len(toks)); n >= 1; n-- {
		for i := 0; i+n <= len(toks); i++ {
			gram := strings.Join(toks[i:i+n], " ")
			if ids, ok := v.exact[gram]; ok {
				for _, id := range ids {
					cands = append(cands, Match{Term: gram, Payload: v.terms[id].payload, Input: gram, Start: i, Words: n})
				}
				continue
			}
			gr := utf8.RuneCountInString(gram)
			maxD := fuzzyEdits(gram, gr)
			if maxD == 0 {
				continue
			}
			best := -1
			bestD := maxD + 1
			for _, id := range v.words[n] {
				t := v.terms[id]
				tEdits := fuzzyEdits(t.folded, t.runes)
				if abs(t.runes-gr) > maxD || tEdits == 0 {
					continue
				}
				lim := min(maxD, tEdits)
				if d := Distance(gram, t.folded, lim); d <= lim && d < bestD {
					best, bestD = id, d
				}
			}
			if best >= 0 {
				cands = append(cands, Match{Term: v.terms[best].folded, Payload: v.terms[best].payload, Input: gram, Distance: bestD, Start: i, Words: n})
			}
		}
	}
	// Greedy selection: prefer more words, then smaller distance; a token
	// may be covered by only one match per payload, and by at most one
	// multi-word match overall.
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].Words != cands[b].Words {
			return cands[a].Words > cands[b].Words
		}
		if cands[a].Distance != cands[b].Distance {
			return cands[a].Distance < cands[b].Distance
		}
		return cands[a].Start < cands[b].Start
	})
	covered := make([]bool, len(toks))
	var out []Match
	for _, c := range cands {
		free := true
		for k := c.Start; k < c.Start+c.Words; k++ {
			if covered[k] {
				free = false
				break
			}
		}
		if !free {
			// allow additional payloads for exactly the same span
			same := false
			for _, o := range out {
				if o.Start == c.Start && o.Words == c.Words && o.Distance == c.Distance && o.Payload != c.Payload {
					same = true
				}
			}
			if !same {
				continue
			}
		}
		dup := false
		for _, o := range out {
			if o.Payload == c.Payload && o.Start == c.Start {
				dup = true
			}
		}
		if dup {
			continue
		}
		for k := c.Start; k < c.Start+c.Words; k++ {
			covered[k] = true
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Start < out[b].Start })
	return out
}

// Closest returns the best fuzzy match of a single folded term, if any.
func (v *Vocab) Closest(term string) (Match, bool) {
	ms := v.Find(strings.Fields(term))
	for _, m := range ms {
		if m.Start == 0 && m.Words == len(strings.Fields(term)) {
			return m, true
		}
	}
	return Match{}, false
}
