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
	p, _ := s.path(root)
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
	r := &repo.Repo{RecipientStrings: []string{"age1z", "age1a"}, Format: repo.Format{EncryptPaths: true}}
	p := For(r)
	if strings.Join(p.Recipients, ",") != "age1a,age1z" || !p.EncryptPaths {
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
