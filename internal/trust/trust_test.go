package trust

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spicy-lemonade/salt/internal/repo"
)

func TestSaveLoad(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "trusted")}
	root := t.TempDir()
	if _, err := s.Load(root); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("Load before Save: %v", err)
	}
	pin := Pin{Recipients: []string{"age1a", "age1b"}, EncryptPaths: true}
	if err := s.Save(root, pin); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(root)
	if err != nil || len(Diff(pin, got)) != 0 {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	p, _, _ := s.paths(root)
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("pin file mode %v, want 0600", fi.Mode().Perm())
	}
	os.WriteFile(p, []byte("{"), 0o600)
	if _, err := s.Load(root); err == nil || errors.Is(err, ErrNotApproved) {
		t.Fatalf("corrupt pin: %v", err)
	}
	os.WriteFile(p, []byte(`{"recipients":[],"encrypt_paths":true}`), 0o600)
	if _, err := s.Load(root); err == nil || !strings.Contains(err.Error(), "no keys") {
		t.Fatalf("pin with no keys: %v", err)
	}
}

func TestSaveErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, []byte("x"), 0o600)
	if err := (Store{Dir: file}).Save(t.TempDir(), Pin{}); err == nil {
		t.Fatal("Save into a file path succeeded")
	}
}

func TestFor(t *testing.T) {
	r := &repo.Repo{RecipientStrings: []string{"age1z", "age1a"}, Format: repo.Format{EncryptPaths: true, Recovery: repo.RecoveryPassphrase}}
	p := For(r)
	if strings.Join(p.Recipients, ",") != "age1a,age1z" || !p.EncryptPaths || p.Recovery != repo.RecoveryPassphrase {
		t.Fatalf("For = %+v", p)
	}
	if r.RecipientStrings[0] != "age1z" {
		t.Fatal("For reordered the repo's recipients")
	}
}

func TestDiff(t *testing.T) {
	approved := Pin{Recipients: []string{"age1mine"}, EncryptPaths: true}
	if d := Diff(approved, approved); len(d) != 0 {
		t.Fatalf("no change: %v", d)
	}
	got := Pin{Recipients: []string{"age1attacker"}, EncryptPaths: false}
	d := strings.Join(Diff(approved, got), "\n")
	for _, want := range []string{"key added: age1attacker", "key removed: age1mine", "file names changed from hidden to visible"} {
		if !strings.Contains(d, want) {
			t.Errorf("Diff missing %q:\n%s", want, d)
		}
	}
	if !strings.Contains(strings.Join(Diff(got, approved), ""), "from visible to hidden") {
		t.Error("reverse path change not described")
	}
}

// A pin is named by the repo's real path, so a symlink to the repo finds the
// same one, and a pin an older salt saved under the path it was given is
// still found.
func TestPinsByRealPath(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "trusted")}
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	pin := Pin{Recipients: []string{"age1a"}, Recovery: repo.RecoveryPhrase}
	if err := s.Save(link, pin); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Load(real); err != nil || len(Diff(pin, got)) != 0 {
		t.Fatalf("Load through the real path = %+v, %v", got, err)
	}

	// An older salt named the pin by the absolute path it was given.
	older := Store{Dir: filepath.Join(t.TempDir(), "trusted")}
	_, oldPath, err := older.paths(link)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(older.Dir, 0o700)
	os.WriteFile(oldPath, []byte(`{"recipients":["age1old"],"encrypt_paths":true}`), 0o600)
	if got, err := older.Load(link); err != nil || got.Recipients[0] != "age1old" {
		t.Fatalf("Load of an older pin = %+v, %v", got, err)
	}
	// Saving again moves it to the real path, which wins from then on.
	if err := older.Save(link, pin); err != nil {
		t.Fatal(err)
	}
	if got, err := older.Load(link); err != nil || got.Recipients[0] != "age1a" {
		t.Fatalf("Load after saving = %+v, %v", got, err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("the older approval is still there: %v", err)
	}
	if _, err := s.Load(filepath.Join(t.TempDir(), "gone")); err == nil {
		t.Fatal("Load of a missing repo succeeded")
	}
}

func TestDiffRecovery(t *testing.T) {
	phrase := Pin{Recipients: []string{"age1a"}, Recovery: repo.RecoveryPhrase}
	for _, tt := range []struct {
		got  string
		want string
	}{
		{repo.RecoveryPhrase, ""},
		{repo.RecoveryPassphrase, "recovery changed from a recovery phrase to a passphrase"},
		{"x\x1b[2J", `recovery changed from a recovery phrase to "x\x1b[2J"`},
	} {
		got := phrase
		got.Recovery = tt.got
		if d := strings.Join(Diff(phrase, got), "\n"); d != tt.want {
			t.Errorf("Diff to %q = %q, want %q", tt.got, d, tt.want)
		}
	}
	// A pin from a salt that did not record the method accepts any.
	if d := Diff(Pin{Recipients: []string{"age1a"}}, phrase); len(d) != 0 {
		t.Errorf("Diff from an older pin = %v", d)
	}
}

// A relative repo path needs the current folder, which may be gone.
func TestStoreWithoutCurrentFolder(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	os.Mkdir(gone, 0o700)
	t.Chdir(gone)
	os.Remove(gone)
	if _, err := os.Getwd(); err == nil {
		t.Skip("this system still reports a removed current folder")
	}
	s := Store{Dir: t.TempDir()}
	if _, err := s.Load("repo"); err == nil || errors.Is(err, ErrNotApproved) {
		t.Fatalf("Load without a current folder: %v", err)
	}
	if err := s.Save("repo", Pin{Recipients: []string{"age1a"}}); err == nil {
		t.Fatal("Save without a current folder succeeded")
	}
}
