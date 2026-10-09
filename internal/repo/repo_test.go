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
		{"no recipients file", `{"version": 1}`, nil, RecipientsFile + " is missing"},
		{"bad recipient", `{"version": 1}`, ptr("# keys\n\n age1notakey\n"), "recipients.txt line 3 is not an age public key"},
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
			if err == nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), "notakey") {
				t.Fatalf("Open error = %v, want %q and no line quoted", err, tt.want)
			}
		})
	}
	for name, root := range map[string]string{"empty dir": t.TempDir(), "no dir": filepath.Join(t.TempDir(), "gone")} {
		if _, err := Open(root); !errors.Is(err, ErrNotInitialised) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// Someone who can push can commit .salt or its files as symlinks, which git
// checks out. Open must refuse them, whether they lead outside the repo or
// back inside it, before reading anything through them, and must say nothing
// of what they point at.
func TestOpenRefusesSymlinks(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	f := Format{Version: FormatVersion, EncryptPaths: true, Recovery: RecoveryPhrase}
	for _, tt := range []struct{ link, target string }{
		{FormatFile, "../../outside/secret.txt"},
		{RecipientsFile, "../../outside/secret.txt"},
		{FormatFile, "../README.md"},
		{FormatFile, "missing.json"},
		{Dir, "../outside/" + Dir},
		{Dir, "elsewhere/" + Dir},
	} {
		t.Run(tt.link+" -> "+tt.target, func(t *testing.T) {
			base := t.TempDir()
			root, outside := filepath.Join(base, "repo"), filepath.Join(base, "outside")
			// Both the outside folder and the one inside the repo hold a
			// working salt setup, so only the symlink check can refuse them.
			for _, dir := range []string{root, outside, filepath.Join(root, "elsewhere")} {
				if err := Write(dir, f, []string{id.Recipient().String()}); err != nil {
					t.Fatal(err)
				}
			}
			os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("the secret line\n"), 0o644)
			os.WriteFile(filepath.Join(root, "README.md"), []byte("the secret line\n"), 0o644)
			link := filepath.Join(root, filepath.FromSlash(tt.link))
			if err := os.RemoveAll(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(tt.target, link); err != nil {
				t.Fatal(err)
			}
			// A plain-path read would find a working setup through a .salt
			// link, so the test shows the symlink check refusing it.
			if _, err := os.Stat(filepath.Join(root, FormatFile)); tt.link == Dir && err != nil {
				t.Fatalf("the .salt link leads to no %s: %v", FormatFile, err)
			}
			_, err := Open(root)
			if !errors.Is(err, ErrForeignSymlink) || !strings.HasPrefix(err.Error(), ForeignSymlink(tt.link).Error()) {
				t.Fatalf("Open error = %v, want a foreign symlink at %s", err, tt.link)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("Open error shows what the link points at: %v", err)
			}
		})
	}
}

// Open reads salt's own files only if they are regular files no larger than
// salt writes them, so a crafted one can't make it read without end.
func TestOpenRefusesOddFiles(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	good := id.Recipient().String()
	tooBig := `{"version": 1, "pad": "` + strings.Repeat("x", maxSaltFile) + `"}`
	fits := strings.Repeat("#", maxSaltFile-len(good)-1) + "\n" + good
	for _, tt := range []struct {
		name, format, recipients, want string
	}{
		{"format.json too large", tooBig, good, FormatFile + " is larger than 64 KiB"},
		{"recipients.txt too large", `{"version": 1}`, fits + "\n", RecipientsFile + " is larger than 64 KiB"},
		{"format.json a folder", "", good, FormatFile + " is not a regular file"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			os.MkdirAll(filepath.Join(root, Dir), 0o755)
			if tt.format == "" {
				os.Mkdir(filepath.Join(root, FormatFile), 0o755)
			} else {
				os.WriteFile(filepath.Join(root, FormatFile), []byte(tt.format), 0o644)
			}
			os.WriteFile(filepath.Join(root, RecipientsFile), []byte(tt.recipients), 0o644)
			if _, err := Open(root); err == nil || !strings.HasPrefix(err.Error(), tt.want) {
				t.Fatalf("Open error = %v, want %q", err, tt.want)
			}
		})
	}

	// A file of exactly the limit is read.
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, Dir), 0o755)
	os.WriteFile(filepath.Join(root, FormatFile), []byte(`{"version": 1}`), 0o644)
	os.WriteFile(filepath.Join(root, RecipientsFile), []byte(fits), 0o644)
	if r, err := Open(root); err != nil || len(r.RecipientStrings) != 1 || r.RecipientStrings[0] != good {
		t.Fatalf("Open at the size limit = %+v, %v", r, err)
	}

	// .salt as a file is neither a repo nor one that is not set up yet.
	root = t.TempDir()
	os.WriteFile(filepath.Join(root, Dir), []byte("a file, not a dir"), 0o644)
	if _, err := Open(root); err == nil || errors.Is(err, ErrNotInitialised) {
		t.Fatalf("Open with .salt as a file: %v", err)
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

// Write works through os.Root, so a .salt link can't lead the files outside
// the repo.
func TestWriteRefusesALinkOutside(t *testing.T) {
	base := t.TempDir()
	root, outside := filepath.Join(base, "repo"), filepath.Join(base, "outside")
	os.MkdirAll(root, 0o755)
	os.MkdirAll(outside, 0o755)
	if err := os.Symlink("../outside", filepath.Join(root, Dir)); err != nil {
		t.Fatal(err)
	}
	id, _ := age.GenerateX25519Identity()
	f := Format{Version: FormatVersion, EncryptPaths: true, Recovery: RecoveryPhrase}
	if err := Write(root, f, []string{id.Recipient().String()}); err == nil {
		t.Fatal("Write followed a link outside the repo")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("files written outside the repo: %v", entries)
	}
}
