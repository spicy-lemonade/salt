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
	"github.com/spicy-lemonade/salt/internal/seal"
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

// setRecovery plays someone with push access changing the recovery method.
func setRecovery(t *testing.T, root, method string) {
	t.Helper()
	p := filepath.Join(root, repo.FormatFile)
	var f repo.Format
	b, _ := os.ReadFile(p)
	json.Unmarshal(b, &f)
	f.Recovery = method
	b, _ = json.Marshal(f)
	os.WriteFile(p, b, 0o644)
}

// An approval saved by a salt that did not record the recovery method gains
// it on the next seal, which then refuses a change to it.
func TestSealRecordsRecoveryInAnOlderApproval(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	r, err := repo.Open(e.root)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := e.app.trustStore().Load(e.root)
	if err != nil {
		t.Fatal(err)
	}
	pin.Recovery = ""
	if err := e.app.trustStore().Save(e.root, pin); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Seal(SealOptions{Src: t.TempDir(), Repo: e.root}); err != nil {
		t.Fatalf("seal with an older approval: %v", err)
	}
	if pin, err := e.app.trustStore().Load(e.root); err != nil || pin.Recovery != r.Format.Recovery {
		t.Fatalf("approval after the seal: %+v, %v", pin, err)
	}
	setRecovery(t, e.root, repo.RecoveryPassphrase)
	if err := e.app.Seal(SealOptions{Src: t.TempDir(), Repo: e.root}); !errors.Is(err, ErrNotTrusted) {
		t.Fatalf("seal after the change: %v", err)
	}
}

func TestSealRefusesChangedKeysOrSettings(t *testing.T) {
	for name, tamper := range map[string]func(t *testing.T, root string) string{
		"attacker key": func(t *testing.T, root string) string { return "key added: " + addAttackerKey(t, root) },
		"visible names": func(t *testing.T, root string) string {
			showFileNames(t, root)
			return "file names changed from hidden to visible"
		},
		"recovery": func(t *testing.T, root string) string {
			setRecovery(t, root, repo.RecoveryPassphrase)
			return "recovery changed from a recovery phrase to a passphrase"
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

			err := e.app.Seal(SealOptions{Src: src, Repo: e.root, Prune: true})
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
	for _, want := range []string{"(your key on this machine)", second + "  ⚠ NOT on this machine",
		"⚠ 1 key(s) are not stored on this machine", "Changed since you last approved", "key added: " + second, "Not approved"} {
		if !strings.Contains(out, want) {
			t.Errorf("trust output missing %q:\n%s", want, out)
		}
	}
	// The warnings come before the question.
	if strings.Index(out, "not stored on this machine") > strings.Index(out, "Approve them?") {
		t.Error("key warning printed after the question")
	}
	if strings.Contains(out, "File names are visible") {
		t.Error("visible-names warning shown for a repo with hidden names")
	}
	src := t.TempDir()
	if err := e.app.Seal(SealOptions{Src: src, Repo: e.root}); !errors.Is(err, ErrNotTrusted) {
		t.Fatalf("seal after declining: %v", err)
	}

	// Answering yes approves it.
	e.ui.out.Reset()
	e.ui.answer = func(p, out string) (string, error) { return "y", nil }
	if err := e.app.Trust(e.root, false); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Seal(SealOptions{Src: src, Repo: e.root}); err != nil {
		t.Fatalf("seal after approving: %v", err)
	}
	e.ui.out.Reset()
	e.app.Trust(e.root, true)
	if !strings.Contains(e.ui.out.String(), "Nothing has changed since you last approved") {
		t.Errorf("output:\n%s", e.ui.out.String())
	}
}

func TestTrustWarnsAboutVisibleNames(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	showFileNames(t, e.root)
	e.ui.out.Reset()
	e.ui.answer = func(p, out string) (string, error) { return "n", nil }
	if err := e.app.Trust(e.root, false); err != nil {
		t.Fatal(err)
	}
	out := e.ui.out.String()
	for _, want := range []string{"⚠ File names are visible", "file names changed from hidden to visible"} {
		if !strings.Contains(out, want) {
			t.Errorf("trust output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "not stored on this machine") {
		t.Error("key warning shown although the only key is on this machine")
	}
	if strings.Index(out, "File names are visible") > strings.Index(out, "Approve them?") {
		t.Error("visible-names warning printed after the question")
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
	if err := e.app.Seal(SealOptions{Src: t.TempDir(), Repo: e.root}); !errors.Is(err, ErrNotTrusted) || !strings.Contains(err.Error(), "salt trust") {
		t.Fatalf("seal on a new machine: %v", err)
	}
	e.ui.interactive = false
	if err := e.app.Trust(e.root, false); !errors.Is(err, ErrNotInteractive) {
		t.Fatalf("trust without a terminal: %v", err)
	}
	if err := e.app.Trust(e.root, true); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Seal(SealOptions{Src: t.TempDir(), Repo: e.root}); err != nil {
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
	if err := e.app.Seal(SealOptions{Src: t.TempDir(), Repo: e.root}); err == nil {
		t.Error("seal with an unreadable approval store succeeded")
	}
	e.ui.out.Reset()
	e.app.Doctor(e.root)
	if !strings.Contains(e.ui.out.String(), "cannot read the keys approved") && !strings.Contains(e.ui.out.String(), "has not approved") {
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

// transplant plays someone who can push to the repo at to, and can read the
// one at from, both opened by this machine's keys: they add from's key to
// to's key list and copy from's backup into to. It returns from's key.
func transplant(t *testing.T, from, to string) string {
	t.Helper()
	r, err := repo.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(to, repo.RecipientsFile), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(r.RecipientStrings[0] + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(to, repo.ObjectsDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(to, repo.ObjectsDir), os.DirFS(filepath.Join(from, repo.ObjectsDir))); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(from, repo.IndexFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(to, repo.IndexFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return r.RecipientStrings[0]
}

// approvalFile returns the one approval file the test env holds.
func approvalFile(t *testing.T, e *testEnv) string {
	t.Helper()
	pins, err := filepath.Glob(filepath.Join(e.app.TrustDir, "*.json"))
	if err != nil || len(pins) != 1 {
		t.Fatalf("approvals: %v, %v", pins, err)
	}
	return pins[0]
}

// A backup signed for one repo is refused by another whose key list gained
// the first repo's key, because only the keys this machine approved for a
// repo may have signed its backups.
func TestApprovedKeysRefuseABackupFromAnotherRepo(t *testing.T) {
	e := newEnv(t)
	other := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(filepath.Join(other, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.ui.answer = phraseAnswers(0)
	if err := e.app.Init(InitOptions{Repo: other}); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "other-src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "USER.md"), []byte("from the other repo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Seal(SealOptions{Src: src, Repo: other, Prune: true}); err != nil {
		t.Fatal(err)
	}
	healthyRepo(t, e)

	// The genuine backup restores with no warning.
	if err := e.app.Verify(e.root, false); err != nil || strings.Contains(e.ui.out.String(), "salt: !") {
		t.Fatalf("verify of the genuine backup: %v\n%s", err, e.ui.out.String())
	}
	e.ui.out.Reset()

	added := transplant(t, other, e.root)
	var unapproved *seal.UnapprovedError
	err := e.app.Verify(e.root, false)
	if !errors.As(err, &unapproved) || unapproved.Key != added || !errors.Is(err, seal.ErrNotSigned) ||
		!strings.Contains(e.ui.out.String(), "key added: "+added) {
		t.Fatalf("verify of the copied backup: %v\n%s", err, e.ui.out.String())
	}
	for _, want := range []string{"not approved for this repo", "salt trust", "copied in a backup from another repo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("verify error missing %q: %v", want, err)
		}
	}
	dest := filepath.Join(t.TempDir(), "restored")
	if err := e.app.Restore(RestoreOptions{Repo: e.root, To: dest}); !errors.As(err, &unapproved) {
		t.Fatalf("restore of the copied backup: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("a refused restore wrote files")
	}

	// It can still be looked at on purpose, with both warnings.
	e.ui.out.Reset()
	if err := e.app.Verify(e.root, true); err != nil || !strings.Contains(e.ui.out.String(), "signed by "+added+", a key not approved for this repo") {
		t.Fatalf("verify --allow-unsigned: %v\n%s", err, e.ui.out.String())
	}
	e.ui.out.Reset()
	if err := e.app.Restore(RestoreOptions{Repo: e.root, To: dest, AllowUnsigned: true}); err != nil {
		t.Fatal(err)
	}
	out := e.ui.out.String()
	if !strings.Contains(out, "key added: "+added) || strings.Contains(out, "not signed by your key") ||
		!strings.Contains(out, "signed by "+added+", a key not approved for this repo") {
		t.Fatalf("restore --allow-unsigned output:\n%s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "USER.md")); string(b) != "from the other repo" {
		t.Fatalf("restored %q", b)
	}

	// A machine that never approved the repo has nothing to check against,
	// and says so.
	e.app.TrustDir = t.TempDir()
	e.ui.out.Reset()
	if err := e.app.Verify(e.root, false); err != nil || !strings.Contains(e.ui.out.String(), "has not approved the keys") {
		t.Fatalf("verify with no approval: %v\n%s", err, e.ui.out.String())
	}
}

// A key added to the repo on another machine, such as a second laptop's,
// and not yet approved here still lets a genuine backup restore, with a
// warning naming it.
func TestRestoreWithAKeyAddedElsewhere(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	added := addAttackerKey(t, e.root)
	if err := e.app.Verify(e.root, false); err != nil {
		t.Fatalf("verify: %v\n%s", err, e.ui.out.String())
	}
	dest := filepath.Join(t.TempDir(), "restored")
	if err := e.app.Restore(RestoreOptions{Repo: e.root, To: dest}); err != nil {
		t.Fatalf("restore: %v\n%s", err, e.ui.out.String())
	}
	out := e.ui.out.String()
	if strings.Count(out, "key added: "+added) != 2 || !strings.Contains(out, "Only a backup signed by an approved key is accepted") {
		t.Fatalf("output:\n%s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "USER.md")); string(b) != "hello" {
		t.Fatalf("restored %q", b)
	}
}

// An approval that can't be read stops restore and verify, unless
// --allow-unsigned, which warns and accepts any key's signature.
func TestRestoreWithUnreadableApproval(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	pin := approvalFile(t, e)
	for name, body := range map[string]string{"corrupt": "{", "no keys": `{"recipients":[],"encrypt_paths":true}`} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(pin, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(t.TempDir(), "restored")
			for cmd, run := range map[string]func(allow bool) error{
				"verify": func(allow bool) error { return e.app.Verify(e.root, allow) },
				"restore": func(allow bool) error {
					return e.app.Restore(RestoreOptions{Repo: e.root, To: dest, AllowUnsigned: allow})
				},
			} {
				err := run(false)
				if err == nil || errors.Is(err, seal.ErrNotSigned) || !strings.Contains(err.Error(), "--allow-unsigned") || !strings.Contains(err.Error(), pin) {
					t.Fatalf("%s: %v", cmd, err)
				}
				e.ui.out.Reset()
				if err := run(true); err != nil || !strings.Contains(e.ui.out.String(), "cannot read the keys approved") {
					t.Fatalf("%s --allow-unsigned: %v\n%s", cmd, err, e.ui.out.String())
				}
			}
			if b, _ := os.ReadFile(filepath.Join(dest, "USER.md")); string(b) != "hello" {
				t.Fatalf("restored %q", b)
			}
		})
	}
}

// salt trust replaces an approval that can't be read, and seal says to run it.
func TestTrustRepairsAnUnreadableApproval(t *testing.T) {
	e := newEnv(t)
	healthyRepo(t, e)
	if err := os.WriteFile(approvalFile(t, e), []byte(`{"recipients":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := e.app.Seal(SealOptions{Src: t.TempDir(), Repo: e.root})
	if err == nil || !strings.Contains(err.Error(), "no keys") || !strings.Contains(err.Error(), "salt trust") {
		t.Fatalf("seal with an approval with no keys: %v", err)
	}
	if err := e.app.Trust(e.root, true); err != nil || !strings.Contains(e.ui.out.String(), "Approving replaces it") {
		t.Fatalf("trust: %v\n%s", err, e.ui.out.String())
	}
	if err := e.app.Seal(SealOptions{Src: t.TempDir(), Repo: e.root}); err != nil {
		t.Fatalf("seal after trust: %v", err)
	}
}
