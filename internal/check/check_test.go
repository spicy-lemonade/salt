package check

import (
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		path string
		head string
		ok   bool
		want bool // violation expected
	}{
		{"objects/ab/cd.age", "age-encryption.org/v1\n-> X25519", true, false},
		{"index.age", "-----BEGIN AGE ENCRYPTED FILE-----", true, false},
		{"memories/USER.md", "The user is", true, true},
		{"memory.db", "SQLite format 3\x00", true, true},
		{"empty", "", true, true},
		{"fake.age", "age-encryption.org/v2\n", true, true},
		{"README.md", "# backups", true, false},
		{".salt/format.json", "{", true, false},
		{".salt/recipients.txt", "age1", true, false},
		{"sub", "", false, true},
		{"nested/README.md", "# not public", true, true},
	}
	for _, tt := range tests {
		v := Classify(tt.path, []byte(tt.head), tt.ok)
		if (v != nil) != tt.want {
			t.Errorf("Classify(%q, %q) = %v, want violation=%v", tt.path, tt.head, v, tt.want)
		}
	}
}

func TestReport(t *testing.T) {
	var vs []Violation
	for i := 0; i < 25; i++ {
		vs = append(vs, Violation{"f", "not encrypted"})
	}
	r := Report(vs)
	if !strings.Contains(r, "25 staged file(s)") || !strings.Contains(r, "and 5 more") {
		t.Fatalf("Report:\n%s", r)
	}
	r = PushReport(vs)
	if !strings.HasPrefix(r, "salt check: refusing push: 25 file(s) in the commits being pushed are not encrypted:\n") ||
		!strings.Contains(r, "and 5 more") || !strings.HasSuffix(r, "for example with git rebase, and encrypt with `salt seal` instead.\n") {
		t.Fatalf("PushReport:\n%s", r)
	}
}

// A pushed file name holding a newline or ESC is quoted, so it can't fake a
// line of salt's output.
func TestViolationQuotesNames(t *testing.T) {
	for path, want := range map[string]string{
		"notes/USER.md":       "notes/USER.md: not encrypted",
		"x\n✓ all is well.md": `"x\n✓ all is well.md": not encrypted`,
		"a\x1b[2Jb":           `"a\x1b[2Jb": not encrypted`,
		"Shot\u202fPM.png":    "Shot\u202fPM.png: not encrypted",
	} {
		if got := (Violation{path, "not encrypted"}).String(); got != want {
			t.Errorf("Violation(%q) = %s, want %s", path, got, want)
		}
	}
}
