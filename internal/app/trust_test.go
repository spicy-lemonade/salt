package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/repo"
)

// addAttackerKey plays someone with push access adding their own key.
func addAttackerKey(t *testing.T, root string) string {
	t.Helper()
	id, _ := age.GenerateX25519Identity()
	rcpt := id.Recipient().String()
	f, err := os.OpenFile(filepath.Join(root, repo.RecipientsFile), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(rcpt + "\n")
	f.Close()
	return rcpt
}

func showFileNames(t *testing.T, root string) {
	t.Helper()
	p := filepath.Join(root, repo.FormatFile)
	var f repo.Format
	b, _ := os.ReadFile(p)
	json.Unmarshal(b, &f)
	f.EncryptPaths = false
	b, _ = json.Marshal(f)
	os.WriteFile(p, b, 0o644)
}

func TestSealRefusesChangedKeysOrSettings(t *testing.T) {
	for name, tamper := range map[string]func(t *testing.T, root string) string{
		"attacker key": func(t *testing.T, root string) string { return "key added: " + addAttackerKey(t, root) },
		"visible names": func(t *testing.T, root string) string {
			showFileNames(t, root)
			return "file names changed from hidden to visible"
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			healthyRepo(t, e)
			before := snapshotRepo(t, e.root)
			want := tamper(t, e.root)
			src := filepath.Join(t.TempDir(), "src")
			os.MkdirAll(src, 0o755)
			os.WriteFile(filepath.Join(src, "USER.md"), []byte("new secret"), 0o644)

			err := e.app.Seal(src, e.root, true)
			if !errors.Is(err, ErrNotTrusted) || !strings.Contains(err.Error(), want) {
				t.Fatalf("Seal after tampering: %v", err)
			}
			if !strings.Contains(err.Error(), "Don't back up until you've checked it") {
				t.Errorf("no warning in: %v", err)
			}
			// Nothing was encrypted to the attacker.
			if after := snapshotRepo(t, e.root); len(after) != len(before) {
				t.Fatalf("seal wrote files after refusing: %d -> %d", len(before), len(after))
			}
			e.ui.out.Reset()
			if err := e.app.Doctor(e.root); !errors.Is(err, ErrReported) || !strings.Contains(e.ui.out.String(), want) {
				t.Fatalf("doctor after tampering: %v\n%s", err, e.ui.out.String())
			}
		})
	}
}

// snapshotRepo lists the ciphertext files in a repo.
func snapshotRepo(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(filepath.Join(root, repo.ObjectsDir), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestTrustApprovesAChange(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	second := addAttackerKey(t, e.root) // here: the user's own second laptop

	// Answering no changes nothing.
	e.ui.answer = func(p, out string) (string, error) { return "n", nil }
	if err := e.app.Trust(e.root, false); err != nil {
		t.Fatal(err)
	}
	out := e.ui.out.String()
	for _, want := range []string{"(your key on this machine)", second, "Changed since you last approved", "key added: " + second, "Not approved"} {
		if !strings.Contains(out, want) {
			t.Errorf("trust output missing %q:\n%s", want, out)
		}
	}
	src := t.TempDir()
	if err := e.app.Seal(src, e.root, false); !errors.Is(err, ErrNotTrusted) {
		t.Fatalf("seal after declining: %v", err)
	}

	// Answering yes approves it.
	e.ui.out.Reset()
	e.ui.answer = func(p, out string) (string, error) { return "y", nil }
	if err := e.app.Trust(e.root, false); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Seal(src, e.root, false); err != nil {
		t.Fatalf("seal after approving: %v", err)
	}
	e.ui.out.Reset()
	e.app.Trust(e.root, true)
	if !strings.Contains(e.ui.out.String(), "Nothing has changed since you last approved") {
		t.Errorf("output:\n%s", e.ui.out.String())
	}
}

func TestTrustOnANewMachine(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	e.app.TrustDir = filepath.Join(t.TempDir(), "fresh") // a machine that never approved it
	e.ui.out.Reset()
	e.app.Doctor(e.root)
	if !strings.Contains(e.ui.out.String(), "has not approved the repo's keys yet") {
		t.Fatalf("doctor on a new machine:\n%s", e.ui.out.String())
	}
	if err := e.app.Seal(t.TempDir(), e.root, false); !errors.Is(err, ErrNotTrusted) || !strings.Contains(err.Error(), "salt trust") {
		t.Fatalf("seal on a new machine: %v", err)
	}
	e.ui.interactive = false
	if err := e.app.Trust(e.root, false); !errors.Is(err, ErrNotInteractive) {
		t.Fatalf("trust without a terminal: %v", err)
	}
	if err := e.app.Trust(e.root, true); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Seal(t.TempDir(), e.root, false); err != nil {
		t.Fatalf("seal after trust --yes: %v", err)
	}
}

func TestTrustErrors(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	if err := e.app.Trust(t.TempDir(), true); !errors.Is(err, repo.ErrNotInitialised) {
		t.Errorf("trust non-repo: %v", err)
	}
	e.ui.answer = eofAt("\nAnybody holding")
	if err := e.app.Trust(e.root, false); err == nil {
		t.Error("trust succeeded when the terminal closed")
	}
	e.app.Store = brokenStore{}
	if err := e.app.Trust(e.root, true); err == nil {
		t.Error("trust succeeded with a broken key store")
	}
	e.app.Store = &keys.MemStore{}

	// A corrupt approval file is reported, not ignored.
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, []byte("x"), 0o600)
	e.app.TrustDir = file
	if err := e.app.Trust(e.root, true); err == nil {
		t.Error("trust saved into a file path")
	}
	if err := e.app.Seal(t.TempDir(), e.root, false); err == nil {
		t.Error("seal with an unreadable approval store succeeded")
	}
	e.ui.out.Reset()
	e.app.Doctor(e.root)
	if !strings.Contains(e.ui.out.String(), "could not read the approved keys") && !strings.Contains(e.ui.out.String(), "has not approved") {
		t.Errorf("doctor output:\n%s", e.ui.out.String())
	}
	e.app.TrustDir = t.TempDir()
	e.ui.answer = phraseAnswers(0)
	os.RemoveAll(filepath.Join(e.root, repo.Dir))
	e.app.TrustDir = file
	if err := e.app.Init(InitOptions{Repo: e.root}); err == nil || !strings.Contains(err.Error(), "approved keys") {
		t.Errorf("init with an unwritable approval store: %v", err)
	}
}
