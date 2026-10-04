package preset

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// Every built-in preset loads, and backs up each path under its own name, so
// two of its paths never clash in the backup.
func TestBuiltinPresetsAreValid(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("no built-in presets")
	}
	for _, n := range names {
		p, err := Get(n)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if p.About == "" {
			t.Errorf("%s: no about", n)
		}
		var tos []string
		for _, x := range p.Paths {
			for _, prev := range tos {
				if clash(prev, x.To) {
					t.Errorf("%s: %s and %s clash", n, prev, x.To)
				}
			}
			tos = append(tos, x.To)
		}
	}
}

// clash reports whether a and b are the same path or one is a folder above
// the other.
func clash(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func TestGetUnknownPreset(t *testing.T) {
	_, err := Get("nope")
	if !errors.Is(err, ErrUnknown) || !strings.Contains(err.Error(), `"nope"`) || !strings.Contains(err.Error(), Names()[0]) {
		t.Fatalf("Get(nope) = %v", err)
	}
}

func TestNamesAreSorted(t *testing.T) {
	if names := Names(); !slices.IsSorted(names) || slices.ContainsFunc(names, func(n string) bool { return strings.HasSuffix(n, ".json") }) {
		t.Fatalf("Names() = %v", names)
	}
}

func TestParseRefusesBadPresets(t *testing.T) {
	for want, body := range map[string]string{
		"unexpected end of JSON":  `{`,
		"not the file's name":     `{"name": "other", "paths": [{"from": "/a", "to": "a"}]}`,
		"no paths":                `{"name": "t"}`,
		"has no from":             `{"name": "t", "paths": [{"to": "a"}]}`,
		`"../a" cannot be used`:   `{"name": "t", "paths": [{"from": "/a", "to": "../a"}]}`,
		`"/a" cannot be used`:     `{"name": "t", "paths": [{"from": "/a", "to": "/a"}]}`,
		`"a//b" cannot be used`:   `{"name": "t", "paths": [{"from": "/a", "to": "a//b"}]}`,
		"same number of *":        `{"name": "t", "paths": [{"from": "/a/*", "to": "a"}]}`,
		"same number of *, each":  `{"name": "t", "paths": [{"from": "/a/*", "to": "a/x*"}]}`,
		"/a/p* and a/* must have": `{"name": "t", "paths": [{"from": "/a/p*", "to": "a/*"}]}`,
		"needs files and keys":    `{"name": "t", "paths": [{"from": "/a", "to": "a"}], "secrets": [{"files": ["x"]}]}`,
		`bad pattern "["`:         `{"name": "t", "paths": [{"from": "/a", "to": "a"}], "skip": ["["]}`,
		`bad pattern "[k"`:        `{"name": "t", "paths": [{"from": "/a", "to": "a"}], "secrets": [{"files": ["x"], "keys": ["[k"]}]}`,
	} {
		_, err := Parse("t", []byte(body))
		if err == nil || !strings.Contains(err.Error(), want) || !strings.HasPrefix(err.Error(), "preset t: ") {
			t.Errorf("Parse(%s) = %v, want %q", body, err, want)
		}
	}
}

func TestParseGoodPreset(t *testing.T) {
	p, err := Parse("t", []byte(`{"name": "t", "about": "test", "paths": [{"from": "${HOME_X:-~/x}/*/data", "to": "x/*/data"}],
		"skip": ["*.log"], "secrets": [{"files": ["*.yaml"], "keys": ["*api_key"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Paths[0].To != "x/*/data" || p.Skip[0] != "*.log" || p.Secrets[0].Keys[0] != "*api_key" {
		t.Fatalf("Parse = %+v", p)
	}
}
