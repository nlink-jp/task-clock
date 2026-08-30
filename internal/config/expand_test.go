package config

import (
	"strings"
	"testing"
)

func lookupMap(m map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}

func TestExpand(t *testing.T) {
	env := lookupMap(map[string]string{"HOME": "/Users/x", "A_1": "one", "_X": "u"})
	cases := []struct {
		in, want string
	}{
		{"plain", "plain"},
		{"${HOME}/data", "/Users/x/data"},
		{"${HOME}${A_1}", "/Users/xone"},
		{"pre-${_X}-post", "pre-u-post"},
		{"$${HOME}", "${HOME}"},          // escape: literal ${HOME}
		{"$$", "$$"},                     // $$ not followed by { stays literal
		{"cost: $5", "cost: $5"},         // lone $ stays literal
		{"a$", "a$"},                     // trailing $
		{"$${A_1} is ${A_1}", "${A_1} is one"},
	}
	for _, c := range cases {
		got, err := Expand(c.in, env)
		if err != nil {
			t.Errorf("Expand(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Expand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExpandErrors(t *testing.T) {
	env := lookupMap(map[string]string{"HOME": "/Users/x"})
	cases := []struct {
		in, wantSub string
	}{
		{"${NOPE}", "undefined variable ${NOPE}"}, // never silently empty
		{"${HOME", "unterminated"},
		{"${}", "invalid variable name"},
		{"${1BAD}", "invalid variable name"},
		{"${A-B}", "invalid variable name"},
	}
	for _, c := range cases {
		_, err := Expand(c.in, env)
		if err == nil {
			t.Errorf("Expand(%q): expected error, got nil", c.in)
			continue
		}
		if !strings.Contains(err.Error(), c.wantSub) {
			t.Errorf("Expand(%q) error = %q, want substring %q", c.in, err, c.wantSub)
		}
	}
}
