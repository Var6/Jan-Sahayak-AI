package textsplit

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplit(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want []string
	}{
		{"", 50, nil},
		{"   ।  ", 50, nil},
		{"नमस्ते।", 50, []string{"नमस्ते।"}},
		{"नमस्ते। आप कैसे हैं? ठीक!", 50, []string{"नमस्ते।", "आप कैसे हैं?", "ठीक!"}},
		{"पहली पंक्ति\nदूसरी पंक्ति", 50, []string{"पहली पंक्ति", "दूसरी पंक्ति"}},
		{"बिना विराम का वाक्य", 50, []string{"बिना विराम का वाक्य"}},
		{"aaaa bbbb cccc", 9, []string{"aaaa bbbb", "cccc"}},
		{"aaaa bbbbb cccc", 9, []string{"aaaa", "bbbbb", "cccc"}},
		{"aaa,bbbbbbbbb", 9, []string{"aaa,bbbbb", "bbbb"}},
		{"aaaaa,bbbbbbb", 9, []string{"aaaaa,", "bbbbbbb"}},
		{"abcdefghij", 4, []string{"abcd", "efgh", "ij"}},
	}
	for _, c := range cases {
		got := Split(c.in, c.max)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("Split(%q,%d)=%q want %q", c.in, c.max, got, c.want)
		}
	}
}

func TestSplitBounded(t *testing.T) {
	in := strings.Repeat("यह एक बहुत लंबा वाक्य है जिसमें कोई विराम नहीं ", 30)
	for _, c := range Split(in, 120) {
		if n := utf8.RuneCountInString(c); n > 120 || n == 0 {
			t.Fatalf("chunk of %d runes", n)
		}
	}
}

func FuzzSplit(f *testing.F) {
	f.Add("नमस्ते। आप कैसे हैं?", 10)
	f.Fuzz(func(t *testing.T, s string, max int) {
		if max <= 0 || max > 1000 || !utf8.ValidString(s) {
			return
		}
		for _, c := range Split(s, max) {
			if utf8.RuneCountInString(c) > max {
				t.Fatalf("chunk %q longer than %d", c, max)
			}
		}
	})
}
