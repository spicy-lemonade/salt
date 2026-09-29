package repo

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

func TestWriteOpen(t *testing.T) {
	root := t.TempDir()
	id, _ := age.GenerateX25519Identity()
	rcpt := id.Recipient().String()
	f := Format{Version: FormatVersion, EncryptPaths: true, Recovery: RecoveryPhrase}
	if err := Write(root, f, []string{rcpt}); err != nil {
		t.Fatal(err)
	}
	r, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if r.Format != f || len(r.Recipients) != 1 || r.RecipientStrings[0] != rcpt {
		t.Fatalf("Open = %+v", r)
	}
}

func TestOpenErrors(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	good := id.Recipient().String()
	tests := []struct {
		name       string
		format     string // "" means no format file
		recipients *string
		want       string
	}{
		{"missing", "", nil, "not a salt repository"},
		{"bad json", "{", ptr(good), "format.json"},
		{"future version", `{"version": 99}`, ptr(good), "upgrade salt"},
		{"no recipients file", `{"version": 1}`, nil, "recipients.txt"},
		{"bad recipient", `{"version": 1}`, ptr("age1notakey"), "recipients.txt"},
		{"only comments", `{"version": 1}`, ptr("# nothing\n\n"), "no recipients"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			os.MkdirAll(filepath.Join(root, Dir), 0o755)
			if tt.format != "" {
				os.WriteFile(filepath.Join(root, FormatFile), []byte(tt.format), 0o644)
			}
			if tt.recipients != nil {
				os.WriteFile(filepath.Join(root, RecipientsFile), []byte(*tt.recipients), 0o644)
			}
			_, err := Open(root)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Open error = %v, want %q", err, tt.want)
			}
		})
	}
	if _, err := Open(t.TempDir()); !errors.Is(err, ErrNotInitialised) {
		t.Fatalf("empty dir: %v", err)
	}
}

func TestWriteErrors(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, Dir), []byte("a file, not a dir"), 0o644)
	if err := Write(root, Format{Version: FormatVersion}, []string{"age1x"}); err == nil {
		t.Fatal("Write succeeded with .salt as a file")
	}
}

func TestCleanPath(t *testing.T) {
	ok := map[string]string{"a": "a", "a/b": "a/b", "a/./b": "a/b", "a/b/../c": "a/c"}
	for in, want := range ok {
		if got, err := CleanPath(in); err != nil || got != want {
			t.Errorf("CleanPath(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "/etc", "..", "../x", "a/../../x", ".", `a\b`, "a\x00b"} {
		if _, err := CleanPath(bad); err == nil {
			t.Errorf("CleanPath(%q) accepted", bad)
		}
	}
}

func ptr(s string) *string { return &s }
