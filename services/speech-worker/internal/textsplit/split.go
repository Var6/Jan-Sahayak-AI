// Package textsplit breaks text into sentence-sized chunks for TTS.
//
// Supertonic quality and latency degrade on long inputs, and synthesising
// sentence by sentence lets the worker check its deadline between chunks.
package textsplit

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// isEnd reports whether r ends a sentence (Devanagari danda, double danda
// and Latin terminators).
func isEnd(r rune) bool {
	switch r {
	case '।', '॥', '.', '?', '!', '\n':
		return true
	}
	return false
}

// Split returns chunks of at most maxRunes runes each (maxRunes > 0).
// Sentences are kept whole when they fit; short neighbouring sentences are
// not merged, so each chunk is one natural utterance. A sentence longer than
// maxRunes is cut at the last space (or comma) before the limit, and as a
// last resort at the rune limit. Empty chunks are dropped.
func Split(text string, maxRunes int) []string {
	if maxRunes <= 0 {
		maxRunes = 200
	}
	var out []string
	var cur strings.Builder
	flush := func() {
		s := strings.TrimSpace(cur.String())
		cur.Reset()
		if s == "" || !hasLetterOrDigit(s) {
			return
		}
		for utf8.RuneCountInString(s) > maxRunes {
			head, tail := cut(s, maxRunes)
			out = append(out, head)
			s = tail
		}
		if s != "" {
			out = append(out, s)
		}
	}
	for _, r := range text {
		if r == '\n' {
			flush()
			continue
		}
		cur.WriteRune(r)
		if isEnd(r) {
			flush()
		}
	}
	flush()
	return out
}

// cut splits s (longer than max runes) at a natural boundary.
func cut(s string, max int) (string, string) {
	rs := []rune(s)
	for i := max; i >= max/2 && i > 0; i-- {
		switch {
		case unicode.IsSpace(rs[i]):
			return strings.TrimSpace(string(rs[:i])), trimTail(rs[i+1:])
		case i < max && (rs[i] == ',' || rs[i] == ';'):
			return strings.TrimSpace(string(rs[:i+1])), trimTail(rs[i+1:])
		}
	}
	return string(rs[:max]), trimTail(rs[max:])
}

func trimTail(rs []rune) string {
	return strings.TrimLeft(strings.TrimSpace(string(rs)), ",; ")
}

func hasLetterOrDigit(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}
