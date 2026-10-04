package app

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spicy-lemonade/salt/internal/check"
	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/prune"
	"github.com/spicy-lemonade/salt/internal/repo"
	"github.com/zalando/go-keyring"
)

func init() {
	keys.WrapWorkFactor = 10 // cheap scrypt in tests
	keyring.MockInit()       // never the real keychain
}

// scriptUI answers prompts through a callback that can see all output so far.
type scriptUI struct {
	out         strings.Builder
	answer      func(prompt, out string) (string, error)
	interactive bool
	clears      int
}

func (s *scriptUI) Printf(f string, a ...any) { fmt.Fprintf(&s.out, f, a...) }
func (s *scriptUI) ReadLine(p string) (string, error) {
	s.out.WriteString(p)
	return s.answer(p, s.out.String())
}
func (s *scriptUI) ReadSecret(p string) (string, error) { return s.ReadLine(p) }
func (s *scriptUI) Interactive() bool                   { return s.interactive }
func (s *scriptUI) Clear()                              { s.clears++ }

type testEnv struct {
	t     *testing.T
	app   *App
	ui    *scriptUI
	store *keys.MemStore
	root  string
	hook  string
	now   time.Time
	git   *fakeGit
}

type fakeGit struct {
	hook      string
	staged    []check.Violation
	stagedErr error
	committed []check.Violation
	last      time.Time
	hasLast   bool
	remote    string
	// storage is what Storage reports; storageCalls counts the calls.
	storage      []check.StorageProblem
	storageTotal int
	storageErr   error
	storageCalls int
	onStorage    func(root string)
	// prune is what Prune reports; pruneDays records each call's keepDays.
	prune     *prune.Result
	pruneErr  error
	pruneDays []int
}

func (f *fakeGit) HookPath(string) (string, error) { return f.hook, nil }
func (f *fakeGit) Staged(string) ([]check.Violation, error) {
	return f.staged, f.stagedErr
}
func (f *fakeGit) Committed(string) ([]check.Violation, error) { return f.committed, nil }
func (f *fakeGit) LastCommit(string) (time.Time, bool, error) {
	return f.last, f.hasLast, nil
}
func (f *fakeGit) Remote(string) string { return f.remote }
func (f *fakeGit) Storage(root string) ([]check.StorageProblem, int, error) {
	f.storageCalls++
	if f.onStorage != nil {
		f.onStorage(root)
	}
	return f.storage, f.storageTotal, f.storageErr
}

func (f *fakeGit) Prune(_ string, keepDays int) (*prune.Result, error) {
	f.pruneDays = append(f.pruneDays, keepDays)
	return f.prune, f.pruneErr
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "backup")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &testEnv{t: t, ui: &scriptUI{interactive: true}, store: &keys.MemStore{}, root: root,
		hook: filepath.Join(root, ".git", "hooks", "pre-commit"),
		now:  time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
	e.git = &fakeGit{hook: e.hook, last: e.now.Add(-6 * time.Hour), hasLast: true, remote: "https://github.com/me/backup.git"}
	e.app = &App{
		UI:        e.ui,
		Store:     e.store,
		StoreName: "test store",
		CacheDir:  filepath.Join(base, "cache"),
		TrustDir:  filepath.Join(base, "trusted"),
		SignDir:   filepath.Join(base, "signing"),
		Git:       e.git,
		LookPath:  func(name string) (string, bool) { return "/usr/local/bin/" + name, true },
		Now:       func() time.Time { return e.now },
		Version:   "test",
	}
	return e
}

var wordRe = regexp.MustCompile(`(\d+)\. ([a-z]+)`)

// lastPhrase extracts the most recently displayed phrase from the output.
func lastPhrase(out string) string {
	block := out[strings.LastIndex(out, "Write these down"):]
	type w struct {
		n    int
		word string
	}
	var ws []w
	for _, m := range wordRe.FindAllStringSubmatch(block, -1) {
		n, _ := strconv.Atoi(m[1])
		ws = append(ws, w{n, m[2]})
	}
	sort.Slice(ws, func(i, j int) bool { return ws[i].n < ws[j].n })
	var words []string
	for _, x := range ws {
		words = append(words, x.word)
	}
	return strings.Join(words, " ")
}

func swapFirstTwo(phrase string) string {
	f := strings.Fields(phrase)
	f[0], f[1] = f[1], f[0]
	return strings.Join(f, " ")
}

// phraseAnswers drives init: choose option 1, get the phrase wrong `wrong`
// times, then right.
func phraseAnswers(wrong int) func(p, out string) (string, error) {
	return func(p, out string) (string, error) {
		switch {
		case strings.HasPrefix(p, "Choose"):
			return "", nil // default is the recovery phrase
		case strings.HasPrefix(p, "Type your 12 words"):
			if wrong > 0 {
				wrong--
				return swapFirstTwo(lastPhrase(out)), nil
			}
			return lastPhrase(out), nil
		default:
			return "", nil
		}
	}
}

func TestInitPhrase(t *testing.T) {
	e := newEnv(t)
	e.ui.answer = phraseAnswers(1)
	if err := e.app.Init(InitOptions{Repo: e.root}); err != nil {
		t.Fatalf("Init: %v\n%s", err, e.ui.out.String())
	}
	out := e.ui.out.String()
	for _, want := range []string{
		"The words are your decryption key. With this method, no decryption key\n     is stored in your backup repo.",
		"Consider using a password manager like\n     Bitwarden.",
		"Anybody with these words can decrypt and read your backups.",
		"If you lose them and this laptop, your backups cannot be recovered.",
		"Let's start again. Nothing has been saved yet.",
		"✓ Your recovery phrase is correct.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
	if strings.Count(out, "Write these down") != 2 {
		t.Errorf("phrase shown %d times, want 2 (one restart)", strings.Count(out, "Write these down"))
	}
	if e.ui.clears < 2 {
		t.Errorf("phrase hidden %d times, want 2", e.ui.clears)
	}

	r, err := repo.Open(e.root)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Format.EncryptPaths || r.Format.Recovery != repo.RecoveryPhrase {
		t.Fatalf("format = %+v", r.Format)
	}
	if _, err := os.Stat(filepath.Join(e.root, repo.KeyFile)); !os.IsNotExist(err) {
		t.Fatal("recovery-phrase init wrote key.age")
	}
	s, err := e.store.Get(r.RecipientStrings[0])
	if err != nil || s.Kind != keys.KindPhrase {
		t.Fatalf("stored secret = %+v, %v", s, err)
	}
	words, _ := s.Phrase()
	if strings.Join(words, " ") != lastPhrase(out) {
		t.Fatal("stored phrase differs from the one shown")
	}
	if b, err := os.ReadFile(e.hook); err != nil || !strings.Contains(string(b), "exec salt check") {
		t.Fatalf("hook not installed: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(e.root, ".gitattributes")); !strings.Contains(string(b), "*.age binary") {
		t.Fatal(".gitattributes not written")
	}
	if err := e.app.Init(InitOptions{Repo: e.root}); err == nil {
		t.Fatal("second init succeeded")
	}
}

func TestInitAbortSavesNothing(t *testing.T) {
	e := newEnv(t)
	e.ui.answer = func(p, out string) (string, error) {
		if strings.HasPrefix(p, "Type your 12 words") {
			return "", io.EOF // Ctrl-D / closed terminal
		}
		return "", nil
	}
	if err := e.app.Init(InitOptions{Repo: e.root}); err == nil {
		t.Fatal("aborted init succeeded")
	}
	if _, err := os.Stat(filepath.Join(e.root, repo.Dir)); !os.IsNotExist(err) {
		t.Fatal("aborted init wrote .salt")
	}
	if _, err := os.Stat(e.hook); !os.IsNotExist(err) {
		t.Fatal("aborted init installed the hook")
	}
	if e.store.Len() != 0 {
		t.Fatal("aborted init saved a key")
	}
	if _, err := os.Stat(e.app.SignDir); !os.IsNotExist(err) {
		t.Fatal("aborted init saved a signing key")
	}
}

func TestInitPhraseNeedsTerminal(t *testing.T) {
	e := newEnv(t)
	e.ui.interactive = false
	err := e.app.Init(InitOptions{Repo: e.root, Recovery: repo.RecoveryPhrase})
	if !errors.Is(err, ErrNotInteractive) {
		t.Fatalf("err = %v", err)
	}
}

func TestInitPassphrase(t *testing.T) {
	e := newEnv(t)
	const good = "correct horse battery staple"
	secrets := []string{"weak", good, "typo horse battery staple", good, good}
	e.ui.answer = func(p, out string) (string, error) {
		s := secrets[0]
		secrets = secrets[1:]
		return s, nil
	}
	if err := e.app.Init(InitOptions{Repo: e.root, Recovery: repo.RecoveryPassphrase, PlainPaths: true}); err != nil {
		t.Fatalf("Init: %v\n%s", err, e.ui.out.String())
	}
	out := e.ui.out.String()
	for _, want := range []string{"Too weak", "don't match", "Consider using a password manager like Bitwarden", "✓ Passphrase set."} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
	r, _ := repo.Open(e.root)
	if r.Format.EncryptPaths || r.Format.Recovery != repo.RecoveryPassphrase {
		t.Fatalf("format = %+v", r.Format)
	}
	data, err := os.ReadFile(filepath.Join(e.root, repo.KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	id, err := keys.UnwrapIdentity(data, good)
	if err != nil || id.Recipient().String() != r.RecipientStrings[0] {
		t.Fatalf("key.age does not open with the passphrase: %v", err)
	}
}

func TestInitPassphraseFile(t *testing.T) {
	e := newEnv(t)
	e.ui.interactive = false
	pf := filepath.Join(t.TempDir(), "pass")
	os.WriteFile(pf, []byte("short\n"), 0o600)
	if err := e.app.Init(InitOptions{Repo: e.root, Recovery: repo.RecoveryPassphrase, PassphraseFile: pf}); err == nil {
		t.Fatal("weak passphrase file accepted")
	}
	os.WriteFile(pf, []byte("correct horse battery staple\n"), 0o600)
	if err := e.app.Init(InitOptions{Repo: e.root, Recovery: repo.RecoveryPassphrase, PassphraseFile: pf}); err != nil {
		t.Fatal(err)
	}
}

// End to end at package level: init, seal, then restore on a "new machine"
// (empty key store) using only the recovery phrase.
func TestRestoreOnNewMachine(t *testing.T) {
	for _, method := range []string{repo.RecoveryPhrase, repo.RecoveryPassphrase} {
		t.Run(method, func(t *testing.T) {
			e := newEnv(t)
			const pass = "correct horse battery staple"
			var phrase string
			e.ui.answer = func(p, out string) (string, error) {
				if strings.HasPrefix(p, "Type your 12 words") {
					phrase = lastPhrase(out)
					return phrase, nil
				}
				if strings.Contains(p, "passphrase") || strings.HasPrefix(p, "Type it again") {
					return pass, nil
				}
				return "", nil
			}
			if err := e.app.Init(InitOptions{Repo: e.root, Recovery: method}); err != nil {
				t.Fatal(err)
			}
			src := filepath.Join(t.TempDir(), "agent")
			os.MkdirAll(filepath.Join(src, "memories"), 0o755)
			os.WriteFile(filepath.Join(src, "memories", "USER.md"), []byte("hello"), 0o644)
			if err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Prune: true}); err != nil {
				t.Fatal(err)
			}

			// New machine: nothing in the key store.
			e.store = &keys.MemStore{}
			e.app.Store = e.store
			e.ui.out.Reset()
			e.ui.answer = func(p, out string) (string, error) {
				switch {
				case strings.HasPrefix(p, "Type your 12 words"):
					return phrase, nil
				case strings.HasPrefix(p, "Passphrase"):
					return pass, nil
				}
				return "", nil // yes, save the key
			}
			dest := filepath.Join(t.TempDir(), "restored")
			if err := e.app.Restore(RestoreOptions{Repo: e.root, To: dest}); err != nil {
				t.Fatalf("Restore: %v\n%s", err, e.ui.out.String())
			}
			if b, _ := os.ReadFile(filepath.Join(dest, "memories", "USER.md")); string(b) != "hello" {
				t.Fatalf("restored USER.md = %q", b)
			}
			if e.store.Len() != 1 {
				t.Fatal("key not saved after restore")
			}
			// Second restore uses the saved key without prompting.
			e.ui.answer = func(p, out string) (string, error) { return "", fmt.Errorf("unexpected prompt %q", p) }
			if err := e.app.Restore(RestoreOptions{Repo: e.root, To: filepath.Join(t.TempDir(), "again")}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoveryTestAndShow(t *testing.T) {
	e := newEnv(t)
	e.ui.answer = phraseAnswers(0)
	if err := e.app.Init(InitOptions{Repo: e.root}); err != nil {
		t.Fatal(err)
	}
	phrase := lastPhrase(e.ui.out.String())

	e.ui.answer = func(p, out string) (string, error) { return phrase, nil }
	if err := e.app.RecoveryTest(e.root); err != nil {
		t.Fatalf("RecoveryTest with the right phrase: %v", err)
	}
	other, _ := keys.EncodePhrase(make([]byte, 16))
	e.ui.answer = func(p, out string) (string, error) { return strings.Join(other, " "), nil }
	if err := e.app.RecoveryTest(e.root); err == nil || !strings.Contains(err.Error(), "not the one for this backup") {
		t.Fatalf("RecoveryTest with another phrase: %v", err)
	}

	e.ui.out.Reset()
	e.ui.answer = func(p, out string) (string, error) {
		if strings.Contains(p, "Show them now?") {
			return "y", nil
		}
		return "", nil
	}
	clears := e.ui.clears
	if err := e.app.RecoveryShow(e.root); err != nil {
		t.Fatal(err)
	}
	for _, w := range strings.Fields(phrase) {
		if !strings.Contains(e.ui.out.String(), w) {
			t.Fatalf("RecoveryShow did not show %q", w)
		}
	}
	if e.ui.clears != clears+1 {
		t.Fatal("RecoveryShow did not hide the phrase")
	}
}

func TestCheckReportsViolations(t *testing.T) {
	e := newEnv(t)
	e.git.staged = []check.Violation{{Path: "memories/USER.md", Reason: "not encrypted"}}
	if err := e.app.Check(e.root); !errors.Is(err, ErrReported) {
		t.Fatalf("Check = %v", err)
	}
	if !strings.Contains(e.ui.out.String(), "memories/USER.md") {
		t.Fatal("violation not reported")
	}
	e.git.staged, e.git.stagedErr = nil, errors.New("git broke")
	if err := e.app.Check(e.root); err == nil {
		t.Fatal("Check passed although inspection failed (must fail closed)")
	}
}

// healthyRepo inits with a phrase, seals one file and returns the phrase.
func healthyRepo(t *testing.T, e *testEnv) string {
	t.Helper()
	e.ui.answer = phraseAnswers(0)
	if err := e.app.Init(InitOptions{Repo: e.root}); err != nil {
		t.Fatal(err)
	}
	phrase := lastPhrase(e.ui.out.String())
	src := filepath.Join(t.TempDir(), "agent")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "USER.md"), []byte("hello"), 0o644)
	if err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Prune: true}); err != nil {
		t.Fatal(err)
	}
	e.ui.out.Reset()
	return phrase
}

func TestDoctorHealthy(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	if err := e.app.Doctor(e.root); err != nil {
		t.Fatalf("Doctor: %v\n%s", err, e.ui.out.String())
	}
	out := e.ui.out.String()
	for _, want := range []string{"Everything looks healthy", "key for this backup is saved", "pre-commit hook runs `salt check`",
		"working tree contains only encrypted files", "salt recovery test"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorProblems(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(e *testEnv)
		want   string
		fails  bool
	}{
		{"no hook", func(e *testEnv) { os.Remove(e.hook) }, "no pre-commit hook", true},
		{"foreign hook", func(e *testEnv) { os.WriteFile(e.hook, []byte("#!/bin/sh\nnpm test\n"), 0o755) }, "does not run `salt check`", true},
		{"salt missing", func(e *testEnv) {
			e.app.LookPath = func(n string) (string, bool) { return "/usr/bin/" + n, n != "salt" }
		}, "hook will refuse every commit", true},
		{"plaintext in tree", func(e *testEnv) { os.WriteFile(filepath.Join(e.root, "USER.md"), []byte("hi"), 0o644) }, "1 unencrypted file(s) in the working tree", true},
		{"plaintext committed", func(e *testEnv) { e.git.committed = []check.Violation{{Path: "USER.md"}} }, "last commit contains 1 unencrypted", true},
		{"no key here", func(e *testEnv) { e.app.Store = &keys.MemStore{} }, "no key for this backup on this machine", false},
		{"stale", func(e *testEnv) { e.git.last = e.now.Add(-5 * 24 * time.Hour) }, "is the nightly backup still running?", false},
		{"no remote", func(e *testEnv) { e.git.remote = "" }, "backups are not leaving this machine", false},
		{"not salt", func(e *testEnv) { os.RemoveAll(filepath.Join(e.root, repo.Dir)) }, "not a salt repository", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			healthyRepo(t, e)
			tt.break_(e)
			err := e.app.Doctor(e.root)
			out := e.ui.out.String()
			if !strings.Contains(out, tt.want) {
				t.Errorf("output missing %q:\n%s", tt.want, out)
			}
			if tt.fails != errors.Is(err, ErrReported) {
				t.Errorf("err = %v, want failure=%v\n%s", err, tt.fails, out)
			}
		})
	}
}

// Doctor warns about a file over GitHub's limit, which salt never writes
// but an older salt or a person might have, and not about one at the limit.
// The files are sparse, so they take no space on disk.
func TestDoctorGitHubLimit(t *testing.T) {
	for _, size := range []int64{repo.GitHubFileLimit, repo.GitHubFileLimit + 1} {
		e := newEnv(t)
		healthyRepo(t, e)
		p := filepath.Join(e.root, "objects", "aa", "big.age")
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("age-encryption.org/v1\n"), 0o644)
		if err := os.Truncate(p, size); err != nil {
			t.Fatal(err)
		}
		if err := e.app.Doctor(e.root); err != nil {
			t.Fatalf("Doctor: %v\n%s", err, e.ui.out.String())
		}
		warned := strings.Contains(e.ui.out.String(), "file(s) over GitHub's 100 MB limit, so the push will fail: objects/aa/big.age (100 MB)")
		if warned != (size > repo.GitHubFileLimit) {
			t.Errorf("%d bytes: warned = %v\n%s", size, warned, e.ui.out.String())
		}
	}
}

func TestDoctorPassphraseKeyFile(t *testing.T) {
	e := newEnv(t)
	e.ui.interactive = false
	pf := filepath.Join(t.TempDir(), "pass")
	os.WriteFile(pf, []byte("correct horse battery staple\n"), 0o600)
	if err := e.app.Init(InitOptions{Repo: e.root, Recovery: repo.RecoveryPassphrase, PassphraseFile: pf}); err != nil {
		t.Fatal(err)
	}
	e.ui.out.Reset()
	e.app.Doctor(e.root)
	if !strings.Contains(e.ui.out.String(), "key.age present") {
		t.Fatalf("doctor:\n%s", e.ui.out.String())
	}
	os.Remove(filepath.Join(e.root, repo.KeyFile))
	e.ui.out.Reset()
	if err := e.app.Doctor(e.root); !errors.Is(err, ErrReported) || !strings.Contains(e.ui.out.String(), "key.age is missing") {
		t.Fatalf("doctor without key.age: %v\n%s", err, e.ui.out.String())
	}
}

func TestVerifyCommand(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	if err := e.app.Verify(e.root, false); err != nil {
		t.Fatalf("Verify: %v\n%s", err, e.ui.out.String())
	}
	if !strings.Contains(e.ui.out.String(), "All 1 files") {
		t.Fatalf("verify output:\n%s", e.ui.out.String())
	}
	// Corrupt every object.
	filepath.WalkDir(filepath.Join(e.root, repo.ObjectsDir), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			os.WriteFile(p, []byte("age-encryption.org/v1\ngarbage"), 0o644)
		}
		return nil
	})
	e.ui.out.Reset()
	if err := e.app.Verify(e.root, false); !errors.Is(err, ErrReported) || !strings.Contains(e.ui.out.String(), "1 of 1 files cannot be restored") {
		t.Fatalf("verify corrupted: %v\n%s", err, e.ui.out.String())
	}
}
