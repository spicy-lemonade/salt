package app

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/spicy-lemonade/salt/internal/check"
	"github.com/spicy-lemonade/salt/internal/escape"
	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/repo"
)

func TestTerminal(t *testing.T) {
	var out bytes.Buffer
	term := &Terminal{in: bufio.NewReader(strings.NewReader("first\r\nlast")), out: &out}
	term.Printf("hi %d\n", 1)
	if s, err := term.ReadLine("? "); err != nil || s != "first" {
		t.Fatalf("ReadLine = %q, %v", s, err)
	}
	// stdin is not a terminal in tests, so ReadSecret reads a plain line.
	if s, err := term.ReadSecret("pw: "); err != nil || s != "last" {
		t.Fatalf("ReadSecret = %q, %v", s, err)
	}
	if _, err := term.ReadLine("? "); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadLine at EOF: %v", err)
	}
	term.Clear() // stderr is not a terminal: nothing is printed
	if term.Interactive() {
		t.Error("tests should not look interactive")
	}
	if !strings.Contains(out.String(), "hi 1") || !strings.Contains(out.String(), "pw: ") {
		t.Fatalf("output = %q", out.String())
	}
	if NewTerminal() == nil {
		t.Fatal("NewTerminal returned nil")
	}
}

// A path or program output holding control characters reaches the terminal
// escaped, through Printf and prompts alike.
func TestTerminalEscapes(t *testing.T) {
	var out bytes.Buffer
	var raw bytes.Buffer
	term := &Terminal{in: bufio.NewReader(strings.NewReader("x\n")), out: escape.Writer(&out), raw: &raw}
	term.Printf("bad %s\n", "a\x1b[2Jb")
	if _, err := term.ReadLine("open \x1b]0;title\a? "); err != nil {
		t.Fatal(err)
	}
	if want := "bad a\\x1b[2Jb\nopen \\x1b]0;title\\a? "; out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
	term.Clear() // stderr is not a terminal: nothing is printed
	if raw.Len() != 0 {
		t.Fatalf("Clear printed %q", raw.String())
	}

	// On a terminal, Clear's own codes reach it unescaped, and only through raw.
	orig := isTerminal
	isTerminal = func(int) bool { return true }
	t.Cleanup(func() { isTerminal = orig })
	before := out.String()
	term.Clear()
	if raw.String() != "\033[H\033[2J\033[3J" || out.String() != before {
		t.Fatalf("Clear wrote raw %q, out %q", raw.String(), out.String())
	}
}

func TestFormatHelpers(t *testing.T) {
	for n, want := range map[int64]string{5: "5 bytes", 2048: "2.0 KB", 3 << 20: "3.0 MB", 2 << 30: "2.0 GB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
	for d, want := range map[time.Duration]string{5 * time.Minute: "5 minutes", 3 * time.Hour: "3 hours", 72 * time.Hour: "3 days"} {
		if got := roughDuration(d); got != want {
			t.Errorf("roughDuration(%v) = %q, want %q", d, got, want)
		}
	}
	if shortKey("short") != "short" || !strings.Contains(shortKey(strings.Repeat("a", 40)), "…") {
		t.Error("shortKey")
	}
}

func TestLookPath(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "tool"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "plain"), []byte("x"), 0o644)
	t.Setenv("PATH", ":"+dir)
	if p, ok := LookPath("tool"); !ok || p != filepath.Join(dir, "tool") {
		t.Errorf("LookPath(tool) = %q, %v", p, ok)
	}
	if _, ok := LookPath("plain"); ok {
		t.Error("non-executable file found")
	}
	if _, ok := LookPath("salt-definitely-missing"); ok {
		t.Error("missing command found")
	}
}

func TestInitEdgeCases(t *testing.T) {
	e := newEnv(t)
	if err := e.app.Init(InitOptions{Repo: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("init outside git: %v", err)
	}
	if err := e.app.Init(InitOptions{Repo: e.root, Recovery: "carrier-pigeon"}); err == nil {
		t.Fatal("unknown recovery method accepted")
	}
	e.ui.interactive = false
	if err := e.app.Init(InitOptions{Repo: e.root}); !errors.Is(err, ErrNotInteractive) {
		t.Fatalf("non-interactive init without --recovery: %v", err)
	}
	if err := e.app.Init(InitOptions{Repo: e.root, Recovery: repo.RecoveryPassphrase}); !errors.Is(err, ErrNotInteractive) {
		t.Fatalf("non-interactive passphrase without a file: %v", err)
	}
	if err := e.app.Init(InitOptions{Repo: e.root, Recovery: repo.RecoveryPassphrase, PassphraseFile: "/nonexistent"}); err == nil {
		t.Fatal("missing passphrase file accepted")
	}
	if _, err := os.Stat(filepath.Join(e.root, repo.Dir)); !os.IsNotExist(err) {
		t.Fatal("a failed init wrote .salt")
	}
}

// Choosing option 2 from the menu after an invalid answer.
func TestChooseRecoveryMenu(t *testing.T) {
	e := newEnv(t)
	answers := []string{"7", "2"}
	e.ui.answer = func(p, out string) (string, error) {
		if strings.HasPrefix(p, "Choose [1]") {
			a := answers[0]
			answers = answers[1:]
			return a, nil
		}
		return "correct horse battery staple", nil
	}
	if err := e.app.Init(InitOptions{Repo: e.root}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.ui.out.String(), "Please type 1 or 2.") {
		t.Fatal("invalid choice not reported")
	}
	r, _ := repo.Open(e.root)
	if r.Format.Recovery != repo.RecoveryPassphrase {
		t.Fatalf("recovery = %q", r.Format.Recovery)
	}
}

func TestInitKeepsForeignHookAndAttributes(t *testing.T) {
	e := newEnv(t)
	os.MkdirAll(filepath.Dir(e.hook), 0o755)
	os.WriteFile(e.hook, []byte("#!/bin/sh\nnpm test\n"), 0o755)
	os.WriteFile(filepath.Join(e.root, ".gitattributes"), []byte("*.png binary"), 0o644)
	e.ui.answer = phraseAnswers(0)
	if err := e.app.Init(InitOptions{Repo: e.root}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.ui.out.String(), "add `salt check` to it") {
		t.Fatal("foreign hook not reported")
	}
	b, _ := os.ReadFile(filepath.Join(e.root, ".gitattributes"))
	if string(b) != "*.png binary\n*.age binary\n" {
		t.Fatalf(".gitattributes = %q", b)
	}
}

// treeSnapshot records every path and file's contents under p, without
// following symlinks, so a test can tell whether anything changed there.
func treeSnapshot(t *testing.T, p string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(p, q)
		b.WriteString(rel + "\n")
		if d.Type().IsRegular() {
			c, err := os.ReadFile(q)
			if err != nil {
				return err
			}
			b.Write(c)
			b.WriteString("\n")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Someone who can push before the owner sets up salt commits a symlink where
// init writes. Init must refuse before asking anything or saving anything,
// and nothing may change where the link points: outside the repo, where
// os.Root would also stop the write, or inside it, where only the up-front
// check does.
func TestInitRefusesForeignSymlinks(t *testing.T) {
	for _, tt := range []struct{ link, target string }{
		{".salt", "../outside"},
		{".salt/format.json", "../../outside/victim.txt"},
		{".salt/key.age", "../../outside/victim.txt"},
		{"index.age", "../outside/victim.txt"},
		{"objects", "../outside"},
		{"files", "../outside"},
		{".gitattributes", "../outside/victim.txt"},
		{".gitattributes", ".git/config"},
		{"objects", ".git"},
		{".salt", ".git"},
	} {
		// Asked in a terminal, init's first step would be a prompt. With a
		// passphrase file it would go straight to writing files.
		for mode, opts := range map[string]func(e *testEnv) InitOptions{
			"prompted": func(e *testEnv) InitOptions { return InitOptions{Repo: e.root} },
			"scripted": func(e *testEnv) InitOptions {
				passFile := filepath.Join(filepath.Dir(e.root), "pass")
				os.WriteFile(passFile, []byte("correct horse battery staple\n"), 0o600)
				return InitOptions{Repo: e.root, Recovery: repo.RecoveryPassphrase, PassphraseFile: passFile}
			},
		} {
			t.Run(mode+" "+tt.link+" -> "+tt.target, func(t *testing.T) {
				e := newEnv(t)
				outside := filepath.Join(filepath.Dir(e.root), "outside")
				if err := os.MkdirAll(outside, 0o755); err != nil {
					t.Fatal(err)
				}
				os.WriteFile(filepath.Join(outside, "victim.txt"), []byte("do not touch\n"), 0o644)
				os.WriteFile(filepath.Join(e.root, ".git", "config"), []byte("[core]\n\tbare = false\n"), 0o644)
				link := filepath.Join(e.root, filepath.FromSlash(tt.link))
				os.MkdirAll(filepath.Dir(link), 0o755)
				if err := os.Symlink(tt.target, link); err != nil {
					t.Fatal(err)
				}
				beforeOutside, beforeGit := treeSnapshot(t, outside), treeSnapshot(t, filepath.Join(e.root, ".git"))
				config, _ := os.ReadFile(filepath.Join(e.root, ".git", "config"))

				// Any prompt ends init, so a regression fails here rather than
				// looping through the phrase check.
				asked := 0
				e.ui.answer = func(p, out string) (string, error) {
					asked++
					return "", io.EOF
				}
				err := e.app.Init(opts(e))
				if !errors.Is(err, repo.ErrForeignSymlink) || !strings.Contains(err.Error(), "at "+tt.link) {
					t.Fatalf("Init error = %v, want a foreign symlink at %s", err, tt.link)
				}

				if asked != 0 || strings.Contains(e.ui.out.String(), "How do you want to recover") {
					t.Fatalf("Init asked %d questions before refusing:\n%s", asked, e.ui.out.String())
				}
				if e.store.Len() != 0 {
					t.Fatal("a key was saved")
				}
				if entries, _ := os.ReadDir(e.app.TrustDir); len(entries) != 0 {
					t.Fatalf("approved keys saved: %v", entries)
				}
				if treeSnapshot(t, outside) != beforeOutside {
					t.Fatal("something changed outside the repo")
				}
				if treeSnapshot(t, filepath.Join(e.root, ".git")) != beforeGit {
					t.Fatal("something changed in .git")
				}
				if after, _ := os.ReadFile(filepath.Join(e.root, ".git", "config")); !bytes.Equal(after, config) {
					t.Fatalf(".git/config changed to %q", after)
				}
			})
		}
	}
}

// Seal asks git about storage only after it has written the new objects:
// before that, git has nothing new to ignore.
func TestSealChecksStorageAfterSealing(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	src := filepath.Join(t.TempDir(), "agent")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "USER.md"), []byte("changed"), 0o644)

	var objectsAtCheck []string
	e.git.onStorage = func(root string) {
		objectsAtCheck, _ = filepath.Glob(filepath.Join(root, repo.ObjectsDir, "*", "*.age"))
	}
	e.git.storage = []check.StorageProblem{{Path: "objects/ab/new.age"}}
	e.git.storageTotal = 1
	calls := e.git.storageCalls
	err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Prune: true})
	if !errors.Is(err, ErrReported) {
		t.Fatalf("Seal with an ignored object: %v", err)
	}
	if e.git.storageCalls != calls+1 || len(objectsAtCheck) != 1 {
		t.Fatalf("storage checked %d time(s), seeing objects %v; want once, after the new object was written", e.git.storageCalls-calls, objectsAtCheck)
	}
	out := e.ui.out.String()
	for _, want := range []string{"salt: sealed 1 files", "salt: the backup was sealed, but 1 file(s) would not reach the remote intact:",
		"objects/ab/new.age is ignored by git"} {
		if !strings.Contains(out, want) {
			t.Errorf("seal output missing %q:\n%s", want, out)
		}
	}

	e.git.storage, e.git.storageTotal, e.git.storageErr = nil, 0, errors.New("no git")
	if err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Prune: true}); err == nil || !strings.Contains(err.Error(), "checking how git will store the backup: no git") {
		t.Fatalf("Seal when git fails: %v", err)
	}

	e.git.storageErr = nil
	if err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Prune: true}); err != nil {
		t.Fatalf("Seal with nothing wrong: %v", err)
	}

	// A repo that is not a git repo pushes nothing, so git is not asked.
	os.RemoveAll(filepath.Join(e.root, ".git"))
	calls = e.git.storageCalls
	e.git.storage, e.git.storageTotal = []check.StorageProblem{{Path: "index.age"}}, 1
	if err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Prune: true}); err != nil || e.git.storageCalls != calls {
		t.Fatalf("Seal outside git: %v, %d storage call(s)", err, e.git.storageCalls-calls)
	}
}

// salt check looks at every salt file, so it refuses even when nothing salt
// wrote is staged.
func TestCheckRefusesStorageProblems(t *testing.T) {
	e := newEnv(t)
	e.git.storage = []check.StorageProblem{{Path: "objects/ab/c.age", Attrs: []check.BadAttr{{Name: "text", Value: "set"}}}}
	e.git.storageTotal = 1
	if err := e.app.Check(e.root); !errors.Is(err, ErrReported) {
		t.Fatalf("Check: %v", err)
	}
	if out := e.ui.out.String(); !strings.Contains(out, "salt check: refusing commit: 1 file(s) would not reach the remote intact:") ||
		!strings.Contains(out, "git would change objects/ab/c.age when storing it (text is set)") {
		t.Fatalf("check output:\n%s", out)
	}
	e.git.storageErr = errors.New("no git")
	if err := e.app.Check(e.root); err == nil || errors.Is(err, ErrReported) {
		t.Fatalf("Check when git fails: %v", err)
	}
	e.git.storage, e.git.storageTotal, e.git.storageErr = nil, 0, nil
	if err := e.app.Check(e.root); err != nil {
		t.Fatalf("Check with nothing wrong: %v", err)
	}
}

func TestDoctorReportsStorageProblems(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	for i := 0; i < 5; i++ {
		e.git.storage = append(e.git.storage, check.StorageProblem{Path: fmt.Sprintf("objects/ab/%d.age", i)})
	}
	e.git.storageTotal = 40
	if err := e.app.Doctor(e.root); !errors.Is(err, ErrReported) {
		t.Fatalf("Doctor: %v", err)
	}
	out := e.ui.out.String()
	for _, want := range []string{"✗ objects/ab/0.age is ignored by git", "✗ objects/ab/2.age is ignored", "✗ … and 37 more file(s) git would not store as written"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "objects/ab/3.age") {
		t.Errorf("doctor listed more than three:\n%s", out)
	}
	// The summary counts files, not lines.
	if !strings.Contains(out, "\n40 problem(s)") {
		t.Errorf("summary does not count 40 files:\n%s", out)
	}
}

// With 8 bad files doctor shows 3 and "… and 5 more", and the summary says 8.
func TestDoctorCountsEveryBadFile(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	for i := 0; i < 8; i++ {
		e.git.storage = append(e.git.storage, check.StorageProblem{Path: fmt.Sprintf("objects/ab/%d.age", i), Attrs: []check.BadAttr{{Name: "text", Value: "set"}}})
	}
	e.git.storageTotal = 8
	e.app.Doctor(e.root)
	out := e.ui.out.String()
	if n := strings.Count(out, "✗ git would change"); n != 3 || !strings.Contains(out, "✗ … and 5 more file(s)") || !strings.Contains(out, "\n8 problem(s) and ") {
		t.Fatalf("%d files listed; output:\n%s", n, out)
	}
}

func TestRecoveryShowBranches(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)

	e.ui.answer = func(p, out string) (string, error) { return "n", nil }
	if err := e.app.RecoveryShow(e.root); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.ui.out.String(), " 1. ") {
		t.Fatal("phrase shown after answering no")
	}

	e.ui.interactive = false
	if err := e.app.RecoveryShow(e.root); !errors.Is(err, ErrNotInteractive) {
		t.Fatalf("non-interactive: %v", err)
	}
	e.ui.interactive = true
	e.app.Store = &keys.MemStore{}
	if err := e.app.RecoveryShow(e.root); err == nil || !strings.Contains(err.Error(), "no recovery phrase") {
		t.Fatalf("no key: %v", err)
	}
	if err := e.app.RecoveryShow(t.TempDir()); !errors.Is(err, repo.ErrNotInitialised) {
		t.Fatalf("not a repo: %v", err)
	}
	e.ui.interactive = false
	if err := e.app.RecoveryTest(e.root); !errors.Is(err, ErrNotInteractive) {
		t.Fatalf("RecoveryTest non-interactive: %v", err)
	}
	if err := e.app.Restore(RestoreOptions{Repo: e.root, To: t.TempDir()}); err == nil {
		t.Fatal("restore without a key or terminal succeeded")
	}
}

func TestRestoreAnswerNoToSaving(t *testing.T) {
	e := newEnv(t)
	phrase := healthyRepo(t, e)
	e.app.Store = &keys.MemStore{}
	e.ui.answer = func(p, out string) (string, error) {
		if strings.HasPrefix(p, "Type your 12 words") {
			return phrase, nil
		}
		return "n", nil
	}
	if err := e.app.Restore(RestoreOptions{Repo: e.root, To: filepath.Join(t.TempDir(), "r")}); err != nil {
		t.Fatal(err)
	}
	if e.app.Store.(*keys.MemStore).Len() != 0 {
		t.Fatal("key saved after answering no")
	}
	// Force over a non-empty destination reports where the old one went.
	dest := t.TempDir()
	os.WriteFile(filepath.Join(dest, "mine"), []byte("x"), 0o644)
	if err := e.app.Restore(RestoreOptions{Repo: e.root, To: dest, Force: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.ui.out.String(), "previous contents were moved to") {
		t.Fatal("moved-aside path not reported")
	}
}

func TestPromptIdentityWrongPassphrase(t *testing.T) {
	e := newEnv(t)
	e.ui.interactive = false
	pf := filepath.Join(t.TempDir(), "pass")
	os.WriteFile(pf, []byte("correct horse battery staple\n"), 0o600)
	if err := e.app.Init(InitOptions{Repo: e.root, Recovery: repo.RecoveryPassphrase, PassphraseFile: pf}); err != nil {
		t.Fatal(err)
	}
	e.ui.interactive = true
	e.ui.answer = func(p, out string) (string, error) { return "wrong horse battery staple", nil }
	if err := e.app.RecoveryTest(e.root); !errors.Is(err, keys.ErrWrongPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	os.Remove(filepath.Join(e.root, repo.KeyFile))
	if err := e.app.RecoveryTest(e.root); err == nil || !strings.Contains(err.Error(), "key.age") {
		t.Fatalf("missing key.age: %v", err)
	}
}

func TestSealCommandReportsSkipped(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	src := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(src, "pipe"), 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	if err := e.app.Seal(SealOptions{Src: src, Repo: e.root}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.ui.out.String(), "skipped pipe") {
		t.Fatalf("output = %q", e.ui.out.String())
	}
	if err := e.app.Seal(SealOptions{Src: src, Repo: t.TempDir()}); !errors.Is(err, repo.ErrNotInitialised) {
		t.Fatalf("seal into non-salt dir: %v", err)
	}
	if err := e.app.Seal(SealOptions{Src: filepath.Join(src, "missing"), Repo: e.root}); err == nil {
		t.Fatal("seal of a missing dir succeeded")
	}
}

// An empty SRC, as from an unset variable in a backup script, is refused
// before the repo changes, instead of sealing an empty backup that removes
// every file sealed before.
func TestSealRefusesEmptySource(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	index, _ := os.ReadFile(filepath.Join(e.root, repo.IndexFile))
	objects, _ := filepath.Glob(filepath.Join(e.root, repo.ObjectsDir, "*", "*"))
	err := e.app.Seal(SealOptions{Src: "", Repo: e.root, Prune: true})
	if err == nil || !strings.Contains(err.Error(), "SRC is empty") {
		t.Fatalf("Seal: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(e.root, repo.IndexFile))
	left, _ := filepath.Glob(filepath.Join(e.root, repo.ObjectsDir, "*", "*"))
	if len(objects) == 0 || !bytes.Equal(index, after) || len(left) != len(objects) {
		t.Fatalf("the repo changed: %d objects before, %d after", len(objects), len(left))
	}
}

type failingGit struct{ fakeGit }

func (failingGit) HookPath(string) (string, error)             { return "", errors.New("no git") }
func (failingGit) Committed(string) ([]check.Violation, error) { return nil, errors.New("no git") }
func (failingGit) LastCommit(string) (time.Time, bool, error) {
	return time.Time{}, false, errors.New("no git")
}
func (failingGit) Storage(string) ([]check.StorageProblem, int, error) {
	return nil, 0, errors.New("no git")
}

type brokenStore struct{}

func (brokenStore) Get(string) (keys.Secret, error) { return keys.Secret{}, errors.New("locked") }
func (brokenStore) Set(string, keys.Secret) error   { return errors.New("locked") }
func (brokenStore) Delete(string) error             { return nil }

func TestDoctorMoreBranches(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)

	// git missing, git commands failing, keychain unreadable.
	e.app.LookPath = func(n string) (string, bool) { return "", false }
	e.app.Git = &failingGit{}
	e.app.Store = brokenStore{}
	e.ui.out.Reset()
	if err := e.app.Doctor(e.root); !errors.Is(err, ErrReported) {
		t.Fatalf("Doctor: %v", err)
	}
	out := e.ui.out.String()
	for _, want := range []string{"git not found", "could not locate the pre-commit hook", "could not inspect the last commit",
		"could not read the last commit", "could not read the test store", "no `origin` remote", "could not ask git how it stores the backup"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorKeyAndTreeBranches(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	r, _ := repo.Open(e.root)
	other, _ := age.GenerateX25519Identity()
	e.store.Set(r.RecipientStrings[0], keys.IdentitySecret(other)) // wrong key saved
	os.Symlink("x", filepath.Join(e.root, "link"))
	// git's view of the attributes, not the file's text, decides this.
	e.git.storage = []check.StorageProblem{{Path: "index.age", Attrs: []check.BadAttr{{Name: "text", Value: "unspecified"}}}}
	e.git.storageTotal = 1
	e.git.hasLast = false
	e.ui.out.Reset()
	e.app.Doctor(e.root)
	out := e.ui.out.String()
	for _, want := range []string{"is damaged", "unencrypted file(s) in the working tree, e.g. link", "no commits yet", "git may change index.age when storing it (*.age is not marked binary)"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
	e.ui.out.Reset()
	e.app.Doctor(t.TempDir()) // not a git repo
	if !strings.Contains(e.ui.out.String(), "not a git repository") {
		t.Error("non-git dir not reported")
	}
}

// A FIFO in the repo folder can only be made on this machine, as git cannot
// store one. Doctor once opened it to read its head and waited for a writer
// that never came. It must list it as unencrypted without opening it.
func TestDoctorDoesNotOpenAPipe(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	if err := syscall.Mkfifo(filepath.Join(e.root, "pipe"), 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	e.ui.out.Reset()
	done := make(chan error, 1)
	go func() { done <- e.app.Doctor(e.root) }()
	select {
	case err := <-done:
		if out := e.ui.out.String(); !errors.Is(err, ErrReported) || !strings.Contains(out, "1 unencrypted file(s) in the working tree, e.g. pipe") {
			t.Fatalf("Doctor: %v\n%s", err, out)
		}
	case <-time.After(10 * time.Second):
		// A writer coming and going lets the stuck open return, so doctor
		// does not outlive the test.
		if w, err := os.OpenFile(filepath.Join(e.root, "pipe"), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
			}
		}
		t.Fatal("doctor is still waiting on the pipe")
	}
}

// seal --prune leaves .salt alone, so doctor never tells the person it will
// remove what is there. A file there that is not salt's is named apart.
func TestDoctorStrayFilesInSaltDir(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	os.WriteFile(filepath.Join(e.root, repo.Dir, "notes.txt"), []byte("plain"), 0o644)
	os.Symlink("x", filepath.Join(e.root, repo.Dir, "link"))
	e.ui.out.Reset()
	err := e.app.Doctor(e.root)
	out := e.ui.out.String()
	want := "2 unexpected file(s) in .salt, e.g. .salt/link, .salt/notes.txt; `salt seal --prune` leaves .salt alone"
	if !errors.Is(err, ErrReported) || !strings.Contains(out, want) || strings.Contains(out, "unencrypted file(s)") || strings.Contains(out, "working tree contains only") {
		t.Fatalf("Doctor: %v\n%s", err, out)
	}
	os.WriteFile(filepath.Join(e.root, "USER.md"), []byte("plain"), 0o644)
	e.ui.out.Reset()
	e.app.Doctor(e.root)
	if out := e.ui.out.String(); !strings.Contains(out, "1 unencrypted file(s) in the working tree, e.g. USER.md;") || !strings.Contains(out, want) {
		t.Fatalf("doctor with both:\n%s", out)
	}
}

// A working tree that cannot be opened is a warning, not a crash.
func TestDoctorTreeCannotOpen(t *testing.T) {
	e := newEnv(t)
	r := &report{ui: e.ui}
	e.app.doctorTree(r, filepath.Join(t.TempDir(), "missing"))
	if r.warns != 1 || !strings.Contains(e.ui.out.String(), "could not scan the working tree") {
		t.Fatalf("warns %d:\n%s", r.warns, e.ui.out.String())
	}
}

// A name someone pushed is shown quoted in doctor's examples, which are
// sorted by the names themselves, and in an error from scanning the tree.
func TestDoctorTreeQuotesNames(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	e := newEnv(t)
	for _, name := range []string{"b.md", "x\n✓ fine.md", "a.md", "c.md"} {
		os.WriteFile(filepath.Join(e.root, name), []byte("plaintext"), 0o644)
	}
	r := &report{ui: e.ui}
	e.app.doctorTree(r, e.root)
	if want := "4 unencrypted file(s) in the working tree, e.g. a.md, b.md, c.md;"; !strings.Contains(e.ui.out.String(), want) {
		t.Fatalf("want %q:\n%s", want, e.ui.out.String())
	}

	locked := filepath.Join(e.root, "a\n✓ fine.age")
	os.WriteFile(locked, []byte("age-encryption.org/v1\n"), 0o000)
	defer os.Chmod(locked, 0o644)
	e.ui.out.Reset()
	e.app.doctorTree(&report{ui: e.ui}, e.root)
	if out := e.ui.out.String(); !strings.Contains(out, `"a\n✓ fine.age": permission denied`) || strings.Contains(out, "\n✓ fine") {
		t.Fatalf("scan error:\n%s", out)
	}
}

func TestExamplesNamesThree(t *testing.T) {
	if got := examples([]string{"a", "b\x1b", "c", "d"}); got != `a, "b\x1b", c` {
		t.Errorf("examples = %s", got)
	}
	if got := examples(nil); got != "" {
		t.Errorf("examples(nil) = %q", got)
	}
}

func TestInstallHookError(t *testing.T) {
	e := newEnv(t)
	e.app.Git = &failingGit{}
	if _, err := e.app.InstallHook(e.root); err == nil {
		t.Fatal("InstallHook succeeded without git")
	}
	e.app.Git = e.git
	os.MkdirAll(filepath.Join(e.root, repo.Dir), 0o755)
	e.ui.answer = phraseAnswers(0)
	e.app.Git = &failingGit{}
	if err := e.app.Init(InitOptions{Repo: e.root}); err == nil || !strings.Contains(err.Error(), "pre-commit hook") {
		t.Fatalf("Init with hook failure: %v", err)
	}
}

func TestVerifyReportsUnreferenced(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	stray := filepath.Join(e.root, repo.ObjectsDir, "zz", "stray.age")
	os.MkdirAll(filepath.Dir(stray), 0o755)
	os.WriteFile(stray, []byte("age-encryption.org/v1\n"), 0o644)
	if err := e.app.Verify(e.root, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.ui.out.String(), "stray.age is not in the index") {
		t.Fatalf("output = %q", e.ui.out.String())
	}
	if err := e.app.Verify(t.TempDir(), false); !errors.Is(err, repo.ErrNotInitialised) {
		t.Fatalf("verify non-repo: %v", err)
	}
}
