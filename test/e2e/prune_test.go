//go:build e2e

package e2e

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
)

// with returns a copy of e that also sets the given environment variables.
func (e *env) with(vars ...string) *env {
	c := *e
	c.vars = append(slices.Clone(e.vars), vars...)
	return &c
}

// stubPath returns a PATH setting under which salt finds a stand-in for
// program, a shell script running script, before the real one.
func (e *env) stubPath(t *testing.T, program, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, program), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return "PATH=" + dir + ":" + filepath.Dir(e.bin) + ":/usr/bin:/bin"
}

// backupRepo is a salt repo cloned from a bare remote, with a snapshot folder
// whose contents a test changes day by day.
type backupRepo struct {
	e      *env
	base   string
	remote string
	dir    string
	src    string
	// files is the snapshot's content, path to text.
	files map[string]string
	// dates is each snapshot file's last-modified date.
	dates map[string]time.Time
	// sealed and sealedDates record, per commit subject, what that day's
	// backup holds.
	sealed      map[string]map[string]string
	sealedDates map[string]map[string]time.Time
}

func emptyBackupRepo(e *env, base string) *backupRepo {
	return &backupRepo{e: e, base: base, remote: filepath.Join(base, "remote.git"), dir: filepath.Join(base, "backup"),
		src: filepath.Join(base, "stage"), files: map[string]string{}, dates: map[string]time.Time{},
		sealed: map[string]map[string]string{}, sealedDates: map[string]map[string]time.Time{}}
}

// newBackupRepo gives the test its own copy of the template backup repo: the
// "Set up salt" commit, dated 2026-08-31 and pushed to a bare remote, with
// the key and signing key in e's home folder. Every copy shares the
// template's key.
func newBackupRepo(t *testing.T, e *env) *backupRepo {
	t.Helper()
	tmpl := backupTemplate(t)
	b := emptyBackupRepo(e, t.TempDir())
	// cp -a keeps the key files' 0600 permissions.
	e.must(b.base, "cp", "-a", tmpl.remote, b.remote)
	e.must(b.base, "cp", "-a", tmpl.dir, b.dir)
	e.must(b.base, "cp", "-a", tmpl.e.home+"/.", e.home)
	e.must(b.dir, "git", "remote", "set-url", "origin", b.remote)
	// Salt approves a repo by its full path, so the copy needs its own
	// approval. The signing key came with the home folder, so this asks
	// nothing.
	e.must(b.base, "salt", "trust", "--yes", b.dir)
	return b
}

// templateDir holds the template backup repo, its remote and its home
// folder. TestMain deletes it.
var (
	templateDir  string
	templateOnce sync.Once
	template     *backupRepo
)

// backupTemplate sets up the template backup repo the first time a test
// needs one. Only this runs salt init for newBackupRepo: it locks the key
// with scrypt at full strength, which takes seconds each time.
func backupTemplate(t *testing.T) *backupRepo {
	t.Helper()
	templateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "salt-e2e-template-")
		if err != nil {
			t.Fatal(err)
		}
		templateDir = dir
		home := filepath.Join(dir, "home")
		if err := os.Mkdir(home, 0o700); err != nil {
			t.Fatal(err)
		}
		b := emptyBackupRepo(newEnvAt(t, home), dir)
		b.e.must(dir, "git", "init", "-q", "--bare", "-b", "main", b.remote)
		b.e.must(dir, "git", "clone", "-q", b.remote, b.dir)
		passFile := filepath.Join(dir, "pass")
		write(t, passFile, "correct horse battery staple\n")
		b.e.must(dir, "salt", "init", b.dir, "--recovery", "passphrase", "--passphrase-file", passFile)
		b.e.must(b.dir, "git", "add", ".salt", ".gitattributes")
		b.commitOn(t, "2026-08-31", "Set up salt")
		b.e.must(b.dir, "git", "push", "-q", "origin", "main")
		template = b
	})
	if template == nil {
		t.Fatal("setting up the template backup repo failed in an earlier test")
	}
	return template
}

// commitOn commits whatever is staged, dated 06:00 UTC on day.
func (b *backupRepo) commitOn(t *testing.T, day, subject string) {
	t.Helper()
	at := "GIT_COMMITTER_DATE=" + day + "T06:00:00+00:00"
	b.e.with(at, "GIT_AUTHOR_DATE="+day+"T06:00:00+00:00").must(b.dir, "git", "commit", "-q", "-m", subject)
}

// editedOn is when a file changed on day: 05:00 UTC, before the backup.
func editedOn(t *testing.T, day string) time.Time {
	t.Helper()
	at, err := time.Parse(time.DateOnly, day)
	if err != nil {
		t.Fatal(err)
	}
	return at.Add(5 * time.Hour)
}

// touch changes only a snapshot file's last-modified date, to day.
func (b *backupRepo) touch(t *testing.T, path, day string) {
	t.Helper()
	b.dates[path] = editedOn(t, day)
	if err := os.Chtimes(filepath.Join(b.src, path), time.Time{}, b.dates[path]); err != nil {
		t.Fatal(err)
	}
}

// backup seals the snapshot and, as the README's script does, commits only if
// something changed. It reports whether a commit was made.
func (b *backupRepo) backup(t *testing.T, day string) bool {
	t.Helper()
	// Only changed files are written, and they are dated the morning they
	// changed. The others keep their dates, as a script copying with
	// `cp -p` keeps them.
	for p, text := range b.files {
		if old, err := os.ReadFile(filepath.Join(b.src, p)); err != nil || string(old) != text {
			write(t, filepath.Join(b.src, p), text)
			b.touch(t, p, day)
		}
	}
	b.e.must(b.base, "salt", "seal", "--prune", b.src, b.dir)
	b.e.must(b.dir, "git", "add", "-A")
	if _, code := b.e.run(b.dir, "git", "diff", "--cached", "--quiet"); code == 0 {
		return false
	}
	subject := "Backup " + day
	b.commitOn(t, day, subject)
	b.sealed[subject] = maps.Clone(b.files)
	b.sealedDates[subject] = maps.Clone(b.dates)
	return true
}

// log lists the branch newest first as "subject|tree".
func (b *backupRepo) log() []string {
	return strings.Split(strings.TrimSpace(b.e.must(b.dir, "git", "log", "--format=%s|%T")), "\n")
}

func (b *backupRepo) subjects() []string {
	return strings.Split(strings.TrimSpace(b.e.must(b.dir, "git", "log", "--format=%s")), "\n")
}

// objectCount is how many objects the local repo holds, loose and packed.
func (b *backupRepo) objectCount(t *testing.T) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(b.e.must(b.dir, "git", "count-objects", "-v"), "\n") {
		k, v, _ := strings.Cut(line, ": ")
		if k == "count" || k == "in-pack" {
			c, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				t.Fatalf("count-objects: %q", line)
			}
			n += c
		}
	}
	return n
}

// restoreCommit checks out sha in a separate work tree and restores it with
// salt, returning the restored files and their last-modified dates.
func (b *backupRepo) restoreCommit(t *testing.T, sha string) (map[string]string, map[string]time.Time) {
	t.Helper()
	wt := filepath.Join(b.base, "wt-"+sha[:12])
	b.e.must(b.dir, "git", "worktree", "add", "-q", "--detach", wt, sha)
	dest := filepath.Join(b.base, "restored-"+sha[:12])
	b.e.must(b.base, "salt", "restore", wt, "--to", dest)
	got, dates := map[string]string{}, map[string]time.Time{}
	filepath.WalkDir(dest, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dest, p)
			data, _ := os.ReadFile(p)
			got[filepath.ToSlash(rel)] = string(data)
			if fi, err := d.Info(); err == nil {
				dates[filepath.ToSlash(rel)] = fi.ModTime().UTC()
			}
		}
		return err
	})
	b.e.must(b.dir, "git", "worktree", "remove", "--force", wt)
	return got, dates
}

// sameDates compares last-modified dates by instant, not by time zone.
func sameDates(a, b map[string]time.Time) bool {
	return maps.EqualFunc(a, b, time.Time.Equal)
}

// Daily backups for nine days, with one day on which nothing changed, pushed
// once at the end. Pruning keeps the last 5 days with a change, which here
// span 6 calendar days; every kept backup still restores exactly, with each
// file as it was that day; and only a force push (with lease) publishes the
// result.
func TestPruneKeepsTheLastDaysWithAChange(t *testing.T) {
	e := newEnv(t)
	b := newBackupRepo(t, e)
	var dropped string
	for d := 1; d <= 9; d++ {
		day := fmt.Sprintf("2026-09-%02d", d)
		if d != 7 { // the 7th: nothing changes, so no commit
			b.files["memories/MEMORY.md"] = "notes as of " + day + "\n"
		}
		if d%7 == 2 { // USER.md changes once a week: the 2nd and 9th
			b.files["memories/USER.md"] = "user, version of " + day + "\n"
		}
		if made := b.backup(t, day); made == (d == 7) {
			t.Fatalf("%s: commit made = %v", day, made)
		}
		if d == 2 {
			dropped = strings.TrimSpace(e.must(b.dir, "git", "rev-parse", "HEAD"))
		}
	}
	e.must(b.dir, "git", "push", "-q", "origin", "main")
	before := b.log()

	out := e.must(b.base, "salt", "prune", b.dir)
	if !strings.Contains(out, "kept 5 backup(s) from the last 5 day(s) with a change and dropped 4 older one(s)") ||
		!strings.Contains(out, "git push --force-with-lease") {
		t.Fatalf("prune output:\n%s", out)
	}
	want := []string{"Backup 2026-09-09", "Backup 2026-09-08", "Backup 2026-09-06", "Backup 2026-09-05", "Backup 2026-09-04"}
	if got := b.subjects(); !slices.Equal(got, want) {
		t.Fatalf("kept %q, want %q", got, want)
	}
	// Each kept commit has the tree and message it had before.
	if got := b.log(); !slices.Equal(got, before[:5]) {
		t.Fatalf("kept commits changed:\n%q\nwant\n%q", got, before[:5])
	}
	if roots := strings.TrimSpace(e.must(b.dir, "git", "rev-list", "--max-parents=0", "HEAD")); roots != strings.TrimSpace(e.must(b.dir, "git", "rev-parse", "HEAD~4")) {
		t.Fatalf("the oldest kept backup is not the first commit: roots %q", roots)
	}
	if st := e.must(b.dir, "git", "status", "--porcelain"); st != "" {
		t.Fatalf("prune changed the working tree:\n%s", st)
	}

	// Every kept backup restores with each file, and its last-modified
	// date, as it was that day: USER.md from the 2nd until it changed on
	// the 9th.
	for _, line := range strings.Split(strings.TrimSpace(e.must(b.dir, "git", "log", "--format=%H %s")), "\n") {
		sha, subject, _ := strings.Cut(line, " ")
		got, dates := b.restoreCommit(t, sha)
		if !maps.Equal(got, b.sealed[subject]) {
			t.Fatalf("%s restored %v, want %v", subject, got, b.sealed[subject])
		}
		if !sameDates(dates, b.sealedDates[subject]) {
			t.Fatalf("%s restored dates %v, want %v", subject, dates, b.sealedDates[subject])
		}
	}
	if got := b.sealed["Backup 2026-09-04"]["memories/USER.md"]; got != "user, version of 2026-09-02\n" {
		t.Fatalf("the oldest kept backup should hold USER.md from the 2nd, has %q", got)
	}
	if got := b.sealedDates["Backup 2026-09-04"]["memories/USER.md"]; !got.Equal(editedOn(t, "2026-09-02")) {
		t.Fatalf("the oldest kept backup should date USER.md the 2nd, has %v", got)
	}

	// A plain push is refused, so a script that forgets --force-with-lease
	// fails loudly rather than leaving the remote unpruned.
	if out, code := e.run(b.dir, "git", "push", "-q", "origin", "main"); code == 0 {
		t.Fatalf("a plain push after prune succeeded:\n%s", out)
	}
	e.must(b.dir, "git", "push", "-q", "--force-with-lease", "origin", "main")
	clone := filepath.Join(b.base, "clone")
	e.must(b.base, "git", "clone", "-q", b.remote, clone)
	if n := strings.TrimSpace(e.must(clone, "git", "rev-list", "--count", "HEAD")); n != "5" {
		t.Fatalf("a fresh clone has %s commits, want 5", n)
	}
	if out := e.must(b.base, "salt", "verify", clone); !strings.Contains(out, "All 2 files") {
		t.Fatalf("verify of the fresh clone:\n%s", out)
	}

	// Pruning again changes nothing.
	head := e.must(b.dir, "git", "rev-parse", "HEAD")
	if out := e.must(b.base, "salt", "prune", b.dir); !strings.Contains(out, "nothing to prune: all 5 backup(s)") {
		t.Fatalf("second prune:\n%s", out)
	}
	if e.must(b.dir, "git", "rev-parse", "HEAD") != head {
		t.Fatal("a prune with nothing to drop rewrote history")
	}

	// Twelve more nightly backups, each pruned and pushed. The local repo
	// stops growing, and the dropped commits leave it once the remote no
	// longer points at them.
	var counts []int
	for d := 1; d <= 12; d++ {
		day := fmt.Sprintf("2026-10-%02d", d)
		b.files["memories/MEMORY.md"] = "notes as of " + day + "\n"
		b.backup(t, day)
		e.must(b.base, "salt", "prune", b.dir)
		e.must(b.dir, "git", "push", "-q", "--force-with-lease", "origin", "main")
		counts = append(counts, b.objectCount(t))
	}
	if n := strings.TrimSpace(e.must(b.dir, "git", "rev-list", "--count", "HEAD")); n != "5" {
		t.Fatalf("after twelve nights the branch has %s commits, want 5", n)
	}
	// Each night adds one commit, tree, index and MEMORY.md object and drops
	// as many, so the count settles (once USER.md from the 2nd is dropped,
	// on the 5th night) and stays flat.
	settled := counts[5]
	for i, c := range counts[6:] {
		if c != settled {
			t.Fatalf("object count on night %d is %d, not the settled %d: %v", i+7, c, settled, counts)
		}
	}
	if _, code := e.run(b.dir, "git", "cat-file", "-e", dropped); code == 0 {
		t.Fatalf("a dropped backup (%s) is still in the local repo", dropped)
	}
	// The remote only reaches the 5 kept backups. (Like GitHub, a bare
	// remote may keep the unreachable objects until it cleans up itself.)
	if n := strings.TrimSpace(e.must(b.remote, "git", "rev-list", "--count", "main")); n != "5" {
		t.Fatalf("the remote's branch has %s commits", n)
	}
}

// --keep-days 1 keeps only the latest day, including every backup made that
// day.
func TestPruneKeepDaysOne(t *testing.T) {
	e := newEnv(t)
	b := newBackupRepo(t, e)
	for i, day := range []string{"2026-09-01", "2026-09-02", "2026-09-03"} {
		b.files["MEMORY.md"] = fmt.Sprintf("version %d\n", i)
		b.backup(t, day)
	}
	b.files["MEMORY.md"] = "later the same day\n"
	b.backup(t, "2026-09-03")
	e.must(b.base, "salt", "prune", "--keep-days", "1", b.dir)
	if got := b.subjects(); !slices.Equal(got, []string{"Backup 2026-09-03", "Backup 2026-09-03"}) {
		t.Fatalf("kept %q", got)
	}
	if out := e.must(b.base, "salt", "verify", b.dir); !strings.Contains(out, "All 1 files") {
		t.Fatalf("verify:\n%s", out)
	}
}

// Prune refuses, and rewrites nothing, when it cannot be sure what it is
// rewriting.
func TestPruneRefusals(t *testing.T) {
	e := newEnv(t)
	b := newBackupRepo(t, e)
	for _, day := range []string{"2026-09-01", "2026-09-02"} {
		b.files["MEMORY.md"] = day
		b.backup(t, day)
	}
	e.must(b.dir, "git", "push", "-q", "origin", "main")

	refuse := func(dir, want string, code int, args ...string) {
		t.Helper()
		head, _ := e.run(dir, "git", "rev-parse", "HEAD")
		out, got := e.run(b.base, "salt", append([]string{"prune"}, args...)...)
		if got != code || !strings.Contains(out, want) {
			t.Fatalf("salt prune %v: exit %d, want %d with %q:\n%s", args, got, code, want, out)
		}
		if after, _ := e.run(dir, "git", "rev-parse", "HEAD"); after != head {
			t.Fatalf("salt prune %v rewrote history", args)
		}
	}
	refuse(b.dir, "--keep-days must be 1 or more", 2, "--keep-days", "0", b.dir)

	// A shallow clone hides the older backups' dates.
	shallow := filepath.Join(b.base, "shallow")
	e.must(b.base, "git", "clone", "-q", "--depth", "1", "file://"+b.remote, shallow)
	e.must(b.base, "salt", "trust", "--yes", shallow)
	refuse(shallow, "shallow clone", 1, "--keep-days", "1", shallow)

	// Detached HEAD: there is no branch to rewrite.
	e.must(b.dir, "git", "checkout", "-q", "--detach")
	refuse(b.dir, "detached HEAD", 1, "--keep-days", "1", b.dir)
	e.must(b.dir, "git", "checkout", "-q", "main")

	// A salt folder inside another repository, whose own .git is not a
	// repository: git would find the outer one, which must not be touched.
	outer := filepath.Join(b.base, "outer")
	e.must(b.base, "git", "clone", "-q", b.remote, outer)
	inner := filepath.Join(outer, "inner")
	os.MkdirAll(filepath.Join(inner, ".git"), 0o755)
	os.CopyFS(filepath.Join(inner, ".salt"), os.DirFS(filepath.Join(b.dir, ".salt")))
	e.must(b.base, "salt", "trust", "--yes", inner)
	refuse(outer, "not at its top", 1, "--keep-days", "1", inner)

	// Keys changed since this machine approved them.
	f, err := os.OpenFile(filepath.Join(b.dir, ".salt", "recipients.txt"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(other.Recipient().String() + "\n")
	f.Close()
	refuse(b.dir, "changed since you approved them", 1, "--keep-days", "1", b.dir)
}

// A repo set up but never committed has nothing to prune. A crafted commit
// larger than salt reads is refused before anything is rewritten.
func TestPruneEdgeCases(t *testing.T) {
	e := newEnv(t)
	base := t.TempDir()
	fresh := filepath.Join(base, "fresh")
	e.must(base, "git", "init", "-q", "-b", "main", fresh)
	passFile := filepath.Join(base, "pass")
	write(t, passFile, "correct horse battery staple\n")
	e.must(base, "salt", "init", fresh, "--recovery", "passphrase", "--passphrase-file", passFile)
	if out := e.must(base, "salt", "prune", fresh); !strings.Contains(out, "nothing to prune: all 0 backup(s)") {
		t.Fatalf("prune with no commits:\n%s", out)
	}

	b := newBackupRepo(t, e)
	b.files["MEMORY.md"] = "x"
	b.backup(t, "2026-09-01")
	huge := filepath.Join(b.base, "message")
	write(t, huge, strings.Repeat("a long message line\n", 60_000))
	e.with("GIT_COMMITTER_DATE=2026-09-02T06:00:00+00:00").must(b.dir, "git", "commit", "-q", "--allow-empty", "-F", huge)
	head := e.must(b.dir, "git", "rev-parse", "HEAD")
	out, code := e.run(b.base, "salt", "prune", "--keep-days", "1", b.dir)
	if code != 1 || !strings.Contains(out, "larger than 1048576 bytes") {
		t.Fatalf("prune with an oversized commit: exit %d\n%s", code, out)
	}
	if e.must(b.dir, "git", "rev-parse", "HEAD") != head {
		t.Fatal("a refused prune rewrote history")
	}
}

// A day on which only a file's last-modified date changed is a day with a
// change: seal rewrites index.age, the backup makes a commit, and the day
// counts. The kept backups restore with the dates they recorded.
func TestPruneCountsADateOnlyChange(t *testing.T) {
	e := newEnv(t)
	b := newBackupRepo(t, e)
	b.files["MEMORY.md"] = "first\n"
	b.backup(t, "2026-09-01")
	b.touch(t, "MEMORY.md", "2026-09-02")
	if !b.backup(t, "2026-09-02") {
		t.Fatal("a date-only change made no commit")
	}
	if st := strings.TrimSpace(e.must(b.dir, "git", "show", "--name-status", "--format=", "HEAD")); st != "M\tindex.age" {
		t.Fatalf("a date-only change should rewrite only the index, got:\n%s", st)
	}
	b.files["MEMORY.md"] = "second\n"
	b.backup(t, "2026-09-03")

	e.must(b.base, "salt", "prune", "--keep-days", "2", b.dir)
	if got := b.subjects(); !slices.Equal(got, []string{"Backup 2026-09-03", "Backup 2026-09-02"}) {
		t.Fatalf("kept %q", got)
	}
	for _, line := range strings.Split(strings.TrimSpace(e.must(b.dir, "git", "log", "--format=%H %s")), "\n") {
		sha, subject, _ := strings.Cut(line, " ")
		got, dates := b.restoreCommit(t, sha)
		if !maps.Equal(got, b.sealed[subject]) || !sameDates(dates, b.sealedDates[subject]) {
			t.Fatalf("%s restored %v %v, want %v %v", subject, got, dates, b.sealed[subject], b.sealedDates[subject])
		}
	}
}

// mustInput runs a command with stdin and returns its trimmed output.
func (e *env) mustInput(dir, stdin, name string, args ...string) string {
	e.t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = e.vars
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		e.t.Fatalf("%s %v: %v", name, args, err)
	}
	return strings.TrimSpace(string(out))
}

// gitChecksCommits reports whether the git salt runs refuses to write raw as
// a commit. Since git 2.40, hash-object checks a commit with fsck first;
// older versions write any commit they can parse.
func (e *env) gitChecksCommits(dir, raw string) bool {
	e.t.Helper()
	// sh finds git on the test's PATH, the one salt uses; exec would look on
	// this process's PATH instead.
	_, code := e.runInput(dir, raw, "sh", "-c", "git hash-object -t commit --stdin")
	return code != 0
}

// plantCommit writes raw as a commit without git's checks and makes it the
// branch's newest commit, as a damaged or crafted history would.
func (b *backupRepo) plantCommit(t *testing.T, raw string) {
	t.Helper()
	sha := b.e.mustInput(b.dir, raw, "git", "hash-object", "-t", "commit", "-w", "--literally", "--stdin")
	b.e.must(b.dir, "git", "update-ref", "refs/heads/main", sha)
}

// prunesAndFails runs a prune that must fail with want in its output and
// leave the branch exactly as it was.
func (b *backupRepo) prunesAndFails(t *testing.T, want string) {
	t.Helper()
	head := b.e.must(b.dir, "git", "rev-parse", "HEAD")
	out, code := b.e.run(b.base, "salt", "prune", "--keep-days", "1", b.dir)
	if code != 1 || !strings.Contains(out, want) {
		t.Fatalf("prune: exit %d, want 1 with %q:\n%s", code, want, out)
	}
	if b.e.must(b.dir, "git", "rev-parse", "HEAD") != head {
		t.Fatal("a failed prune moved the branch")
	}
}

// A history salt cannot read stops the prune with git's reason, and the
// branch is left exactly as it was.
func TestPruneStopsOnABrokenHistory(t *testing.T) {
	e := newEnv(t)
	b := newBackupRepo(t, e)
	b.files["MEMORY.md"] = "x"
	b.backup(t, "2026-09-01")
	tree := strings.TrimSpace(e.must(b.dir, "git", "rev-parse", "HEAD^{tree}"))
	b.plantCommit(t, "tree "+tree+"\nparent 1111111111111111111111111111111111111111\n"+
		"author t <t@t> 1788328800 +0000\ncommitter t <t@t> 1788328800 +0000\n\nmissing parent\n")
	b.prunesAndFails(t, "Failed to traverse parents")
}

// Prune copies a kept commit exactly and leaves judging it to git. A commit
// with no email is refused by git 2.40 and later, which stops the prune and
// leaves the branch alone; older git copies it, so it is kept unchanged.
func TestPruneLeavesAMalformedCommitToGit(t *testing.T) {
	e := newEnv(t)
	b := newBackupRepo(t, e)
	b.files["MEMORY.md"] = "x"
	b.backup(t, "2026-09-01")
	tree := strings.TrimSpace(e.must(b.dir, "git", "rev-parse", "HEAD^{tree}"))
	parent := strings.TrimSpace(e.must(b.dir, "git", "rev-parse", "HEAD"))
	copied := "tree " + tree + "\n" +
		"author nobody 1788328800 +0000\ncommitter t <t@t> 1788328800 +0000\n\nno email\n"
	planted := strings.Replace(copied, "\n", "\nparent "+parent+"\n", 1)
	b.plantCommit(t, planted)

	if e.gitChecksCommits(b.dir, copied) {
		b.prunesAndFails(t, "missing email")
		return
	}
	e.must(b.base, "salt", "prune", "--keep-days", "1", b.dir)
	if got := e.must(b.dir, "git", "cat-file", "commit", "HEAD"); got != copied {
		t.Fatalf("kept commit is\n%s\nwant\n%s", got, copied)
	}
}

// A git that cannot write the copied commits stops the prune with git's
// reason, and the branch is left exactly as it was, on every git version.
func TestPruneStopsWhenGitCannotWrite(t *testing.T) {
	e := newEnv(t)
	b := newBackupRepo(t, e)
	b.files["MEMORY.md"] = "x"
	b.backup(t, "2026-09-01")
	b.files["MEMORY.md"] = "y"
	b.backup(t, "2026-09-02")

	objects := filepath.Join(b.dir, ".git", "objects")
	setDirModes(t, objects, 0o555)
	t.Cleanup(func() { setDirModes(t, objects, 0o755) })
	b.prunesAndFails(t, "insufficient permission")
}

// setDirModes sets the permissions of root and every folder under it.
func setDirModes(t *testing.T, root string, mode os.FileMode) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		return os.Chmod(path, mode)
	})
	if err != nil {
		t.Fatal(err)
	}
}
