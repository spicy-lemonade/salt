package app

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spicy-lemonade/salt/internal/check"
	"github.com/spicy-lemonade/salt/internal/hook"
	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/repo"
	"github.com/spicy-lemonade/salt/internal/seal"
	"github.com/spicy-lemonade/salt/internal/trust"
)

// StaleAfter is how old the last commit can be before doctor warns that the
// nightly backup may have stopped.
const StaleAfter = 48 * time.Hour

// GitHubFileLimit is the size above which GitHub rejects a pushed file.
const GitHubFileLimit = 100 << 20

type level int

const (
	ok level = iota
	warn
	fail
)

type report struct {
	ui    UI
	fails int
	warns int
}

func (r *report) add(l level, format string, a ...any) {
	mark := map[level]string{ok: "✓", warn: "!", fail: "✗"}[l]
	switch l {
	case warn:
		r.warns++
	case fail:
		r.fails++
	}
	r.ui.Printf("  %s %s\n", mark, fmt.Sprintf(format, a...))
}

// Doctor checks that salt and a backup repository are healthy. It needs no
// secrets and changes nothing.
func (a *App) Doctor(repoRoot string) error {
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return err
	}
	r := &report{ui: a.UI}
	a.UI.Printf("salt doctor: %s\n\n", root)
	defer func() {
		a.UI.Printf("\n")
		switch {
		case r.fails > 0:
			a.UI.Printf("%d problem(s) and %d warning(s) found.\n", r.fails, r.warns)
		case r.warns > 0:
			a.UI.Printf("No problems; %d warning(s).\n", r.warns)
		default:
			a.UI.Printf("Everything looks healthy.\n")
		}
	}()

	r.add(ok, "salt %s", a.Version)
	if p, found := a.LookPath("git"); found {
		r.add(ok, "git found (%s)", p)
	} else {
		r.add(fail, "git not found on PATH")
	}

	if err := requireGitRepo(root); err != nil {
		r.add(fail, "%v", err)
		return ErrReported
	}
	rp, err := repo.Open(root)
	if err != nil {
		r.add(fail, "%v", err)
		return ErrReported
	}
	r.add(ok, "salt repo: format %d, file paths %s, recovery by %s", rp.Format.Version,
		map[bool]string{true: "encrypted", false: "visible"}[rp.Format.EncryptPaths], recoveryNoun(rp))

	a.doctorKeys(r, rp)
	a.doctorTrust(r, rp)
	a.doctorHook(r, root)
	a.doctorTree(r, root)

	if vs, err := a.Git.Committed(root); err != nil {
		r.add(warn, "could not inspect the last commit: %v", err)
	} else if len(vs) > 0 {
		r.add(fail, "the last commit contains %d unencrypted file(s), e.g. %s; if it was pushed, that plaintext is on the remote",
			len(vs), vs[0].Path)
	} else {
		r.add(ok, "last commit contains no unencrypted files")
	}

	if _, err := os.Stat(filepath.Join(root, repo.IndexFile)); err != nil {
		r.add(warn, "no backup sealed yet (no %s); run `salt seal`", repo.IndexFile)
	}
	if t, found, err := a.Git.LastCommit(root); err != nil {
		r.add(warn, "could not read the last commit: %v", err)
	} else if !found {
		r.add(warn, "no commits yet")
	} else if age := a.Now().Sub(t); age > StaleAfter {
		r.add(warn, "last commit was %s ago (%s); is the nightly backup still running?", roughDuration(age), t.Format("2006-01-02 15:04"))
	} else {
		r.add(ok, "last commit %s ago (%s)", roughDuration(age), t.Format("2006-01-02 15:04"))
	}
	if url := a.Git.Remote(root); url != "" {
		r.add(ok, "remote: %s", url)
	} else {
		r.add(warn, "no `origin` remote; backups are not leaving this machine")
	}
	if b, _ := os.ReadFile(filepath.Join(root, ".gitattributes")); !strings.Contains(string(b), "*.age binary") {
		r.add(warn, ".gitattributes does not mark *.age as binary")
	}

	a.UI.Printf("\n  Tip: run `salt recovery test %s` now and then to make sure your written-down %s still works.\n",
		repoRoot, recoveryNoun(rp))
	if r.fails > 0 {
		return ErrReported
	}
	return nil
}

func (a *App) doctorKeys(r *report, rp *repo.Repo) {
	n := len(rp.RecipientStrings)
	r.add(ok, "%d recipient(s): %s", n, strings.Join(shortKeys(rp.RecipientStrings), ", "))

	if rp.Format.Recovery == repo.RecoveryPassphrase {
		if head, err := readHead(filepath.Join(rp.Root, repo.KeyFile)); err != nil {
			r.add(fail, "%s is missing: without it your passphrase cannot recover this backup", repo.KeyFile)
		} else if check.Classify(repo.KeyFile, head, true) != nil {
			r.add(fail, "%s is not an age file", repo.KeyFile)
		} else {
			r.add(ok, "%s present (passphrase-protected key)", repo.KeyFile)
		}
	}

	for _, rcpt := range rp.RecipientStrings {
		s, err := a.Store.Get(rcpt)
		if errors.Is(err, keys.ErrNotFound) {
			continue
		}
		if err != nil {
			r.add(warn, "could not read the %s: %v", a.StoreName, err)
			return
		}
		if id, err := s.Identity(); err != nil || id.Recipient().String() != rcpt {
			r.add(fail, "the key saved for %s in the %s is damaged", shortKey(rcpt), a.StoreName)
			return
		}
		where := "the " + a.StoreName
		if l, isLocator := a.Store.(interface{ Location(string) string }); isLocator {
			if loc := l.Location(rcpt); loc != "" {
				where = loc
			}
		}
		r.add(ok, "key for this backup is saved in %s", where)
		return
	}
	r.add(warn, "no key for this backup on this machine: backups still work (they only need the public key), but restoring will ask for your %s",
		recoveryNoun(rp))
}

func (a *App) doctorTrust(r *report, rp *repo.Repo) {
	approved, err := a.trustStore().Load(rp.Root)
	switch {
	case errors.Is(err, trust.ErrNotApproved):
		r.add(warn, "this machine has not approved the repo's keys yet, so `salt seal` will refuse; run `salt trust %s`", rp.Root)
	case err != nil:
		r.add(warn, "could not read the approved keys: %v", err)
	default:
		if d := trust.Diff(approved, trust.For(rp)); len(d) > 0 {
			r.add(fail, "the repo's keys or settings changed since you approved them: %s", strings.Join(d, "; "))
		} else {
			r.add(ok, "keys and settings match what you approved")
		}
	}
}

func (a *App) doctorHook(r *report, root string) {
	p, err := a.Git.HookPath(root)
	if err != nil {
		r.add(fail, "could not locate the pre-commit hook: %v", err)
		return
	}
	switch _, statErr := os.Stat(p); {
	case hook.Installed(p):
		r.add(ok, "pre-commit hook runs `salt check` (%s)", p)
	case statErr == nil:
		r.add(fail, "pre-commit hook at %s does not run `salt check`; add it so plaintext commits are refused", p)
	default:
		r.add(fail, "no pre-commit hook; run `salt hook install %s`", root)
	}
	if sp, found := a.LookPath("salt"); found {
		r.add(ok, "the hook can find salt (%s)", sp)
	} else {
		r.add(fail, "salt is not on PATH or in %s, so the hook will refuse every commit", strings.Join(HookSearchPath, ", "))
	}
}

// doctorTree scans the working tree, reading only file headers.
func (a *App) doctorTree(r *report, root string) {
	var plain, big []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".salt-tmp-") {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			plain = append(plain, rel)
			return nil
		}
		head, err := readHead(p)
		if err != nil {
			return err
		}
		if check.Classify(rel, head, true) != nil {
			plain = append(plain, rel)
		}
		if fi, err := d.Info(); err == nil && fi.Size() > GitHubFileLimit*95/100 {
			big = append(big, fmt.Sprintf("%s (%d MB)", rel, fi.Size()>>20))
		}
		return nil
	})
	if err != nil {
		r.add(warn, "could not scan the working tree: %v", err)
		return
	}
	if len(plain) > 0 {
		slices.Sort(plain)
		r.add(fail, "%d unencrypted file(s) in the working tree, e.g. %s; `salt seal --prune` removes them",
			len(plain), strings.Join(plain[:min(3, len(plain))], ", "))
	} else {
		r.add(ok, "working tree contains only encrypted files and public salt settings")
	}
	if len(big) > 0 {
		r.add(warn, "file(s) close to GitHub's 100 MB limit: %s", strings.Join(big, ", "))
	}
}

func readHead(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, check.HeadSize)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return head[:n], nil
}

// Verify decrypts every file in the backup (without writing plaintext) and
// checks it against the index.
func (a *App) Verify(repoRoot string) error {
	rp, err := repo.Open(repoRoot)
	if err != nil {
		return err
	}
	ids, err := a.identities(rp)
	if err != nil {
		return err
	}
	res, err := seal.Verify(rp.Root, ids, 0)
	if err != nil {
		return err
	}
	for _, u := range res.Unreferenced {
		a.UI.Printf("  ! %s is not in the index (the next `salt seal` removes it)\n", u)
	}
	if len(res.Problems) > 0 {
		a.UI.Printf("✗ %d of %d files cannot be restored:\n", len(res.Problems), res.Files)
		for _, p := range res.Problems {
			a.UI.Printf("  %s\n", p)
		}
		return ErrReported
	}
	a.UI.Printf("✓ All %d files (%s) and %d symlinks decrypt and match the index.\n",
		res.Files, humanBytes(res.Bytes), res.Symlinks)
	return nil
}

func shortKeys(ks []string) []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = shortKey(k)
	}
	return out
}

func shortKey(k string) string {
	if len(k) <= 16 {
		return k
	}
	return k[:10] + "…" + k[len(k)-6:]
}

func roughDuration(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}
