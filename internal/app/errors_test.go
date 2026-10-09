package app

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/regular"
	"github.com/spicy-lemonade/salt/internal/repo"
)

// eofAt answers every prompt with "" (or phrase-typing with the shown phrase)
// until a prompt starting with prefix, which gets io.EOF.
func eofAt(prefix string) func(p, out string) (string, error) {
	return func(p, out string) (string, error) {
		if strings.HasPrefix(p, prefix) {
			return "", io.EOF
		}
		if strings.HasPrefix(p, "Type your 12 words") {
			return lastPhrase(out), nil
		}
		return "", nil
	}
}

// Closing the terminal at any prompt aborts init and saves nothing.
func TestInitAbortsAtEveryPrompt(t *testing.T) {
	for _, prompt := range []string{"Choose [1]", "Press Enter once", "Press Enter to see"} {
		t.Run(prompt, func(t *testing.T) {
			e := newEnv(t)
			e.ui.answer = eofAt(prompt)
			if prompt == "Press Enter to see" {
				// Get the phrase wrong once so this prompt appears.
				e.ui.answer = func(p, out string) (string, error) {
					switch {
					case strings.HasPrefix(p, "Press Enter to see"):
						return "", io.EOF
					case strings.HasPrefix(p, "Type your 12 words"):
						return swapFirstTwo(lastPhrase(out)), nil
					}
					return "", nil
				}
			}
			if err := e.app.Init(InitOptions{Repo: e.root}); err == nil {
				t.Fatal("init succeeded after the terminal closed")
			}
			if _, err := os.Stat(filepath.Join(e.root, repo.Dir)); !os.IsNotExist(err) {
				t.Fatal("aborted init wrote .salt")
			}
			if e.store.Len() != 0 {
				t.Fatal("aborted init saved a key")
			}
		})
	}
	for _, prompt := range []string{"Choose a passphrase", "Type it again"} {
		t.Run(prompt, func(t *testing.T) {
			e := newEnv(t)
			e.ui.answer = func(p, out string) (string, error) {
				if strings.HasPrefix(p, prompt) {
					return "", io.EOF
				}
				return "correct horse battery staple", nil
			}
			if err := e.app.Init(InitOptions{Repo: e.root, Recovery: repo.RecoveryPassphrase}); err == nil {
				t.Fatal("init succeeded after the terminal closed")
			}
		})
	}
}

// Typing a different valid phrase is caught and restarts the step.
func TestInitRejectsADifferentValidPhrase(t *testing.T) {
	e := newEnv(t)
	other, _ := keys.EncodePhrase(make([]byte, 16))
	tries := 0
	e.ui.answer = func(p, out string) (string, error) {
		if strings.HasPrefix(p, "Type your 12 words") {
			tries++
			if tries == 1 {
				return strings.Join(other, " "), nil
			}
			return lastPhrase(out), nil
		}
		return "", nil
	}
	if err := e.app.Init(InitOptions{Repo: e.root}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.ui.out.String(), "valid phrase, but not the one shown") {
		t.Fatal("different phrase not reported")
	}
}

func TestInitStorageFailures(t *testing.T) {
	t.Run("key store", func(t *testing.T) {
		e := newEnv(t)
		e.app.Store = brokenStore{}
		e.ui.answer = phraseAnswers(0)
		if err := e.app.Init(InitOptions{Repo: e.root}); err == nil || !strings.Contains(err.Error(), "saving your key") {
			t.Fatalf("Init: %v", err)
		}
	})
	t.Run("corrupt format file", func(t *testing.T) {
		e := newEnv(t)
		os.MkdirAll(filepath.Join(e.root, repo.Dir), 0o755)
		os.WriteFile(filepath.Join(e.root, repo.FormatFile), []byte("{"), 0o644)
		if err := e.app.Init(InitOptions{Repo: e.root}); err == nil || !strings.Contains(err.Error(), "format.json") {
			t.Fatalf("Init: %v", err)
		}
	})
	t.Run("gitattributes is a folder", func(t *testing.T) {
		e := newEnv(t)
		os.MkdirAll(filepath.Join(e.root, ".gitattributes"), 0o755)
		e.ui.answer = phraseAnswers(0)
		if err := e.app.Init(InitOptions{Repo: e.root}); err == nil {
			t.Fatal("Init succeeded")
		}
	})
}

func TestEnsureGitattributesIsIdempotent(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 2; i++ {
		if err := ensureGitattributes(root); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, ".gitattributes")); string(b) != "*.age binary\n" {
		t.Fatalf(".gitattributes = %q", b)
	}
}

// ensureGitattributes works through os.Root, so even without init's
// up-front check a link can't lead it to a file outside the repo.
func TestEnsureGitattributesRefusesALinkOutside(t *testing.T) {
	base := t.TempDir()
	root, victim := filepath.Join(base, "repo"), filepath.Join(base, "victim.txt")
	os.MkdirAll(root, 0o755)
	os.WriteFile(victim, []byte("do not touch\n"), 0o644)
	if err := os.Symlink("../victim.txt", filepath.Join(root, ".gitattributes")); err != nil {
		t.Fatal(err)
	}
	if err := ensureGitattributes(root); err == nil {
		t.Fatal("ensureGitattributes followed a link outside the repo")
	}
	if b, _ := os.ReadFile(victim); string(b) != "do not touch\n" {
		t.Fatalf("file outside the repo changed to %q", b)
	}
}

func TestRecoveryAndRestoreFailures(t *testing.T) {
	e := newEnv(t)
	phrase := healthyRepo(t, e)
	notRepo := t.TempDir()

	if err := e.app.RecoveryTest(notRepo); !errors.Is(err, repo.ErrNotInitialised) {
		t.Errorf("RecoveryTest non-repo: %v", err)
	}
	if err := e.app.Restore(RestoreOptions{Repo: notRepo, To: t.TempDir()}); !errors.Is(err, repo.ErrNotInitialised) {
		t.Errorf("Restore non-repo: %v", err)
	}

	// Terminal closes at each prompt.
	e.ui.answer = eofAt("Type your 12 words")
	if err := e.app.RecoveryTest(e.root); err == nil {
		t.Error("RecoveryTest succeeded with no input")
	}
	e.ui.answer = eofAt("Anybody with these words")
	if err := e.app.RecoveryShow(e.root); err == nil {
		t.Error("RecoveryShow succeeded with no input")
	}
	e.ui.answer = func(p, out string) (string, error) {
		if strings.HasPrefix(p, "Press Enter to hide") {
			return "", io.EOF
		}
		return "y", nil
	}
	if err := e.app.RecoveryShow(e.root); err == nil {
		t.Error("RecoveryShow succeeded when the terminal closed")
	}

	// A broken key store.
	e.app.Store = brokenStore{}
	if err := e.app.RecoveryShow(e.root); err == nil {
		t.Error("RecoveryShow succeeded with a broken store")
	}
	if err := e.app.Restore(RestoreOptions{Repo: e.root, To: filepath.Join(t.TempDir(), "r")}); err == nil {
		t.Error("Restore succeeded with a broken store")
	}

	// A damaged saved secret.
	r, _ := repo.Open(e.root)
	bad := &keys.MemStore{}
	bad.Set(r.RecipientStrings[0], keys.Secret{Kind: "weird"})
	e.app.Store = bad
	if err := e.app.Restore(RestoreOptions{Repo: e.root, To: filepath.Join(t.TempDir(), "r")}); err == nil {
		t.Error("Restore succeeded with a damaged secret")
	}

	// A passphrase-kind secret in a phrase repo can't be shown as a phrase.
	idOnly := &keys.MemStore{}
	id, _ := keys.IdentityFromEntropy(make([]byte, 16))
	idOnly.Set(r.RecipientStrings[0], keys.IdentitySecret(id))
	e.app.Store = idOnly
	e.ui.answer = func(p, out string) (string, error) { return "y", nil }
	if err := e.app.RecoveryShow(e.root); err == nil || !strings.Contains(err.Error(), "no recovery phrase") {
		t.Errorf("RecoveryShow with an identity secret: %v", err)
	}

	// No saved key: closing the terminal at the save question, or a store
	// that cannot save.
	e.app.Store = &keys.MemStore{}
	e.ui.answer = func(p, out string) (string, error) {
		if strings.HasPrefix(p, "Type your 12 words") {
			return phrase, nil
		}
		return "", io.EOF
	}
	if err := e.app.Restore(RestoreOptions{Repo: e.root, To: filepath.Join(t.TempDir(), "r")}); err == nil {
		t.Error("Restore succeeded when the terminal closed")
	}
	e.app.Store = saveFails{&keys.MemStore{}}
	e.ui.answer = func(p, out string) (string, error) {
		if strings.HasPrefix(p, "Type your 12 words") {
			return phrase, nil
		}
		return "y", nil
	}
	if err := e.app.Restore(RestoreOptions{Repo: e.root, To: filepath.Join(t.TempDir(), "r")}); err == nil || !strings.Contains(err.Error(), "saving your key") {
		t.Errorf("Restore with a store that cannot save: %v", err)
	}
	e.ui.answer = eofAt("Type your 12 words")
	if err := e.app.Restore(RestoreOptions{Repo: e.root, To: filepath.Join(t.TempDir(), "r")}); err == nil {
		t.Error("Restore succeeded with no phrase typed")
	}
}

type saveFails struct{ *keys.MemStore }

func (saveFails) Set(string, keys.Secret) error { return errors.New("read-only") }

func TestPromptIdentityOddRepos(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	r, _ := repo.Open(e.root)
	r.Format.Recovery = "carrier-pigeon"
	if _, _, err := e.app.promptIdentity(r); err == nil || !strings.Contains(err.Error(), "unknown recovery method") {
		t.Errorf("unknown method: %v", err)
	}
	r.Format.Recovery = repo.RecoveryPassphrase
	os.WriteFile(filepath.Join(e.root, repo.KeyFile), []byte("x"), 0o644)
	e.ui.answer = eofAt("Passphrase")
	if _, _, err := e.app.promptIdentity(r); err == nil {
		t.Error("passphrase prompt succeeded with no input")
	}
}

// A store that reports where keys live.
type locatedStore struct {
	*keys.MemStore
	loc string
}

func (l locatedStore) Location(string) string { return l.loc }

func TestDoctorSmallBranches(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	r, _ := repo.Open(e.root)
	s, _ := e.store.Get(r.RecipientStrings[0])
	ls := locatedStore{&keys.MemStore{}, "/somewhere"}
	ls.Set(r.RecipientStrings[0], s)
	e.app.Store = ls
	os.WriteFile(filepath.Join(e.root, ".salt-tmp-123"), []byte("partial"), 0o644)
	e.ui.out.Reset()
	if err := e.app.Doctor(e.root); err != nil {
		t.Fatalf("Doctor: %v\n%s", err, e.ui.out.String())
	}
	if !strings.Contains(e.ui.out.String(), "saved in private file /somewhere") {
		t.Errorf("location not reported:\n%s", e.ui.out.String())
	}

	// An unreadable file in the tree.
	locked := filepath.Join(e.root, "locked.age")
	os.WriteFile(locked, []byte("age-encryption.org/v1\n"), 0o000)
	defer os.Chmod(locked, 0o644)
	e.ui.out.Reset()
	e.app.Doctor(e.root)
	if os.Getuid() != 0 && !strings.Contains(e.ui.out.String(), "could not scan the working tree") {
		t.Errorf("unreadable file not reported:\n%s", e.ui.out.String())
	}
}

func TestDoctorKeyFileNotAge(t *testing.T) {
	e := newEnv(t)
	e.ui.interactive = false
	pf := filepath.Join(t.TempDir(), "pass")
	os.WriteFile(pf, []byte("correct horse battery staple\n"), 0o600)
	if err := e.app.Init(InitOptions{Repo: e.root, Recovery: repo.RecoveryPassphrase, PassphraseFile: pf}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(e.root, repo.KeyFile), []byte("plain text"), 0o644)
	e.ui.out.Reset()
	if err := e.app.Doctor(e.root); !errors.Is(err, ErrReported) || !strings.Contains(e.ui.out.String(), "is not an age file") {
		t.Fatalf("Doctor: %v\n%s", err, e.ui.out.String())
	}
}

// A .gitattributes that a process on this machine made a named pipe fails
// init at once, rather than waiting for a writer.
func TestEnsureGitattributesRefusesAPipe(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, ".gitattributes"), 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	if err := ensureGitattributes(root); !errors.Is(err, regular.ErrNotRegular) {
		t.Fatalf("ensureGitattributes with a pipe: %v", err)
	}
}
