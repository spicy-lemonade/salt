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
		{"mnemosyne.db", "SQLite format 3\x00", true, true},
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
}
