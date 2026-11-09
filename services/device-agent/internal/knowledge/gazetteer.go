package knowledge

import (
	"encoding/json"
	"fmt"

	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/textnorm"
)

// District is one Bihar district from packages/scheme-knowledge/gazetteer.
type District struct {
	ID           string   `json:"id"`
	En           string   `json:"en"`
	Hi           string   `json:"hi"`
	Variants     []string `json:"variants"`
	NeedsContext bool     `json:"needs_context,omitempty"`
}

// Gazetteer recognises district names (Devanagari, English, common STT
// misspellings) in citizen text. Immutable after load.
type Gazetteer struct {
	Districts []District
	byID      map[string]District
	v         *textnorm.Vocab
}

type gazFile struct {
	Schema    string     `json:"schema"`
	State     string     `json:"state"`
	Count     int        `json:"count"`
	Districts []District `json:"districts"`
}

// maxGazetteerBytes bounds the gazetteer file.
const maxGazetteerBytes = 1 << 20

// LoadGazetteer reads a gazetteer JSON file.
func LoadGazetteer(path string) (*Gazetteer, error) {
	b, err := readLimited(path, maxGazetteerBytes)
	if err != nil {
		return nil, err
	}
	var f gazFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("gazetteer: %w", err)
	}
	if f.Count != len(f.Districts) {
		return nil, fmt.Errorf("gazetteer: count %d but %d districts", f.Count, len(f.Districts))
	}
	g := &Gazetteer{Districts: f.Districts, byID: map[string]District{}}
	var pairs [][2]string
	for _, d := range f.Districts {
		if d.ID == "" || d.Hi == "" || d.En == "" {
			return nil, fmt.Errorf("gazetteer: district %+v incomplete", d)
		}
		if _, dup := g.byID[d.ID]; dup {
			return nil, fmt.Errorf("gazetteer: duplicate id %q", d.ID)
		}
		g.byID[d.ID] = d
		for _, n := range append([]string{d.Hi, d.En}, d.Variants...) {
			pairs = append(pairs, [2]string{n, d.ID})
		}
	}
	g.v = textnorm.NewVocab(pairs)
	return g, nil
}

// Get returns a district by id.
func (g *Gazetteer) Get(id string) (District, bool) {
	d, ok := g.byID[id]
	return d, ok
}

// Match finds the first district named in text. Districts flagged
// NeedsContext (names that are also ordinary words, e.g. गया = "went")
// match only next to a word meaning "district".
func (g *Gazetteer) Match(text string) (District, bool) {
	if g == nil {
		return District{}, false
	}
	a := textnorm.Analyze(text)
	ctxAt := map[int]bool{}
	for _, h := range a.Concepts {
		if h.Concept == textnorm.CDistrict {
			ctxAt[h.Start] = true
		}
	}
	for _, m := range g.v.Find(a.Tokens) {
		d := g.byID[m.Payload]
		if m.Distance > 0 && len([]rune(m.Term)) < 6 {
			continue // short names (पटना ≈ पढ़ना) must match exactly
		}
		if d.NeedsContext && !ctxAt[m.Start+m.Words] && !ctxAt[m.Start-1] {
			continue
		}
		return d, true
	}
	return District{}, false
}
