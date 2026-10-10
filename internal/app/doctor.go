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
	"github.com/spicy-lemonade/salt/internal/escape"
	"github.com/spicy-lemonade/salt/internal/hook"
	"github.com/spicy-lemonade/salt/internal/keys"
	"github.com/spicy-lemonade/salt/internal/regular"
	"github.com/spicy-lemonade/salt/internal/repo"
	"github.com/spicy-lemonade/salt/internal/seal"
	"github.com/spicy-lemonade/salt/internal/trust"
)

// StaleAfter is how old the last commit can be before doctor warns that the
// nightly backup may have stopped.
const StaleAfter = 48 * time.Hour

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
	r.addN(l, 1, format, a...)
}

// addN prints one line that stands for n problems or warnings, such as
// "… and 5 more", so the summary counts all of them.
func (r *report) addN(l level, n int, format string, a ...any) {
	mark := map[level]string{ok: "✓", warn: "!", fail: "✗"}[l]
	switch l {
	case warn:
		r.warns += n
	case fail:
		r.fails += n
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
	a.UI.Printf("salt doctor: %s\n\n", a.short(root))
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
		r.add(ok, "git found (%s)", a.short(p))
	} else {
		r.add(fail, "git not found on PATH")
	}

	a.doctorRestores(r)
	a.doctorDownloads(r)

	if err := a.requireGitRepo(root); err != nil {
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
	a.doctorSigning(r, rp)
	a.doctorHook(r, root)
	a.doctorTree(r, root)

	if vs, err := a.Git.Committed(root); err != nil {
		r.add(warn, "could not inspect the last commit: %v", err)
	} else if len(vs) > 0 {
		r.add(fail, "the last commit contains %d unencrypted file(s), e.g. %s; if it was pushed, that plaintext is on the remote",
			len(vs), escape.Name(vs[0].Path))
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
	a.doctorStorage(r, root)

	a.UI.Printf("\n  Tip: run `salt recovery test %q` now and then to make sure your written-down %s still works.\n",
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
		if data, err := repo.ReadSaltFile(rp.Root, repo.KeyFile); errors.Is(err, fs.ErrNotExist) {
			r.add(fail, "%s is missing: without it your passphrase cannot recover this backup", repo.KeyFile)
		} else if err != nil {
			r.add(fail, "%v", err)
		} else if check.Classify(repo.KeyFile, data, true) != nil {
			r.add(fail, "%s is not an age file", repo.KeyFile)
		} else {
			r.add(ok, "%s present (passphrase-protected key)", repo.KeyFile)
		}
	}

	for _, rcpt := range rp.RecipientStrings {
		_, err := a.storedIdentity(rcpt)
		if errors.Is(err, keys.ErrNotFound) {
			continue
		}
		if errors.Is(err, errDamagedKey) {
			r.add(fail, "the key saved for %s in the %s is damaged", shortKey(rcpt), a.StoreName)
			return
		}
		if err != nil {
			r.add(warn, "could not read the %s: %v", a.StoreName, err)
			return
		}
		where := "the " + a.StoreName
		if l, isLocator := a.Store.(interface{ Location(string) string }); isLocator {
			if loc := l.Location(rcpt); loc != "" {
				where = "private file " + a.short(loc)
			}
		}
		r.add(ok, "key for this backup is saved in %s", where)
		return
	}
	r.add(warn, "no key for this backup on this machine: backups still work (they only need the public key), but restoring will ask for your %s",
		recoveryNoun(rp))
}

func (a *App) doctorTrust(r *report, rp *repo.Repo) {
	switch _, d, err := a.approval(rp); {
	case errors.Is(err, trust.ErrNotApproved):
		r.add(warn, "this machine has not approved the repo's keys yet, so `salt seal` will refuse; run `salt trust %q`", rp.Root)
	case err != nil:
		r.add(warn, "%v", err)
	case len(d) > 0:
		r.add(fail, "the repo's keys or settings changed since you approved them: %s", strings.Join(d, "; "))
	default:
		r.add(ok, "keys and settings match what you approved")
	}
}

func (a *App) doctorSigning(r *report, rp *repo.Repo) {
	switch k, rcpt, err := a.signingKey(rp); {
	case errors.Is(err, errNoSigningKey):
		r.add(warn, "this machine has no key to sign backups, so `salt seal` will refuse; run `salt trust %q`", rp.Root)
	case err != nil:
		r.add(fail, "the signing key saved on this machine cannot be read: %v", err)
	case a.wrongSigningKey(k, rcpt):
		r.add(fail, "the signing key saved on this machine does not match your key, so restore would refuse the backups it signs; run `salt trust %q` to replace it", rp.Root)
	default:
		r.add(ok, "this machine can sign backups")
	}
}

// doctorStorage asks git whether the remote gets salt's files exactly as
// written. The working tree can be fine while what git stores is not.
func (a *App) doctorStorage(r *report, root string) {
	problems, total, err := a.Git.Storage(root)
	switch {
	case err != nil:
		r.add(warn, "could not ask git how it stores the backup: %v", err)
	case total == 0:
		r.add(ok, "git stores every salt file exactly as written")
	default:
		for _, p := range problems[:min(3, len(problems))] {
			r.add(fail, "%s", p)
		}
		if total > 3 {
			r.addN(fail, total-3, "… and %d more file(s) git would not store as written", total-3)
		}
	}
}

func (a *App) doctorHook(r *report, root string) {
	for _, h := range hook.All {
		missing := fail
		if h.WarnIfMissing {
			missing = warn
		}
		p, err := a.Git.HookPath(root, h.Name)
		if err != nil {
			r.add(fail, "could not locate the %s hook: %v", h.Name, err)
			continue
		}
		switch _, statErr := os.Stat(p); {
		case hook.Installed(p, h):
			r.add(ok, "%s hook runs `%s` (%s)", h.Name, h.Runs, a.short(p))
		case statErr == nil:
			r.add(missing, "%s hook at %s does not run `%s`; add it so plaintext %s are refused", h.Name, a.short(p), h.Runs, h.Refuses)
		default:
			r.add(missing, "no %s hook; run `salt hook install %q`", h.Name, root)
		}
	}
	if sp, found := a.LookPath("salt"); found {
		r.add(ok, "the hook can find salt (%s)", sp)
	} else {
		r.add(fail, "salt is not on PATH or in %s, so the hook will refuse every commit", strings.Join(HookSearchPath, ", "))
	}
}

// doctorTree scans the working tree, reading only file headers. Files are
// read through os.Root, so one swapped for a symlink after the scan found it
// cannot lead outside the repo.
func (a *App) doctorTree(r *report, root string) {
	rt, err := os.OpenRoot(root)
	if err != nil {
		r.add(warn, "could not scan the working tree: %v", err)
		return
	}
	defer rt.Close()
	// stray holds what is in .salt but not salt's, which seal --prune leaves
	// alone, and plain the rest.
	var plain, stray, big []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
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
		list := &plain
		if rel == repo.Dir || strings.HasPrefix(rel, repo.Dir+"/") {
			list = &stray
		}
		if !d.Type().IsRegular() {
			*list = append(*list, rel)
			return nil
		}
		head, err := readHead(rt, rel)
		if err != nil {
			return err
		}
		if check.Classify(rel, head, true) != nil {
			*list = append(*list, rel)
		}
		if fi, err := d.Info(); err == nil && fi.Size() > repo.GitHubFileLimit {
			big = append(big, fmt.Sprintf("%s (%d MB)", escape.Name(rel), fi.Size()>>20))
		}
		return nil
	})
	if err != nil {
		// The path in the error can be a name someone pushed.
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			err = fmt.Errorf("%s %s: %w", pathErr.Op, escape.Name(pathErr.Path), pathErr.Err)
		}
		r.add(warn, "could not scan the working tree: %v", err)
		return
	}
	if len(plain) > 0 {
		slices.Sort(plain)
		r.add(fail, "%d unencrypted file(s) in the working tree, e.g. %s; `salt seal --prune` removes them",
			len(plain), examples(plain))
	}
	if len(stray) > 0 {
		slices.Sort(stray)
		r.add(fail, "%d unexpected file(s) in %s, e.g. %s; `salt seal --prune` leaves %s alone, so check the repo's recent commits and remove them by hand",
			len(stray), repo.Dir, examples(stray), repo.Dir)
	}
	if len(plain)+len(stray) == 0 {
		r.add(ok, "working tree contains only encrypted files and public salt settings")
	}
	if len(big) > 0 {
		r.add(warn, "%d file(s) over GitHub's 100 MB limit, so the push will fail, e.g. %s", len(big), examples(big))
	}
}

// examples names the first three of names, which can come from the repo, so
// each goes through escape.Name. Text escape.Name returned is unchanged by it.
func examples(names []string) string {
	shown := make([]string, min(3, len(names)))
	for i := range shown {
		shown[i] = escape.Name(names[i])
	}
	return strings.Join(shown, ", ")
}

// readHead reads the start of the file rel in rt, refusing anything that is
// not a regular file without waiting on it.
func readHead(rt *os.Root, rel string) ([]byte, error) {
	f, _, err := regular.Open(rt.OpenFile, filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, check.HeadSize)
	n, err := io.ReadFull(f, head)
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		err = nil
	}
	return head[:n], err
}

// Verify decrypts every file in the backup (without writing plaintext) and
// checks it against the index. allowUnsigned checks the files even if the
// index is not signed by the person's key.
func (a *App) Verify(repoRoot string, allowUnsigned bool) error {
	rp, err := repo.Open(repoRoot)
	if err != nil {
		return err
	}
	signedBy, err := a.approvedSigners(rp, allowUnsigned)
	if err != nil {
		return err
	}
	ids, err := a.identities(rp)
	if err != nil {
		return err
	}
	res, err := seal.Verify(rp.Root, ids, seal.VerifyOptions{
		SignatureOptions: seal.SignatureOptions{AllowUnsigned: allowUnsigned, SignedBy: signedBy},
	})
	if err != nil {
		return explainUnsigned(err, rp.Root)
	}
	if res.Unsigned {
		a.UI.Printf("%s", unsignedWarning(res.Unapproved))
	}
	for _, u := range res.Unreferenced {
		a.UI.Printf("  ! %s is not in the index (the next `salt seal` removes it)\n", escape.Name(u))
	}
	if res.ProblemCount > 0 {
		a.UI.Printf("✗ %d of %d files cannot be restored:\n", res.ProblemCount, res.Files)
		for _, p := range res.Problems {
			a.UI.Printf("  %s\n", p)
		}
		if more := res.ProblemCount - len(res.Problems); more > 0 {
			a.UI.Printf("  … and %d more\n", more)
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
