package prune

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spicy-lemonade/salt/internal/gitx"
)

// memGit is an in-memory repository with one branch. Commit objects are
// stored in git's real format and hashed the way git hashes them.
type memGit struct {
	top      string
	shallow  bool
	branch   string
	head     string
	objects  map[string][]byte
	reclaims int
	// fail makes the named method return an error.
	fail map[string]error
	// corrupt makes CatCommit return this instead of the stored object.
	corrupt []byte
}

func hashObject(raw []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "commit %d\x00", len(raw))
	h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
}

func newMemGit(top string) *memGit {
	return &memGit{top: top, branch: "refs/heads/main", objects: map[string][]byte{}, fail: map[string]error{}}
}

// commit adds a commit on top of the branch, dated day, with a tree named by
// tree and the given extra headers.
func (g *memGit) commit(day, tree string, extra ...string) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "tree %s\n", tree)
	if g.head != "" {
		fmt.Fprintf(&b, "parent %s\n", g.head)
	}
	at, err := time.Parse(time.DateOnly, day)
	if err != nil {
		panic(err)
	}
	fmt.Fprintf(&b, "author t <t@t> %[1]d +0000\ncommitter t <t@t> %[1]d +0000\n", at.Add(6*time.Hour).Unix())
	for _, e := range extra {
		b.WriteString(e + "\n")
	}
	fmt.Fprintf(&b, "\nBackup %s\n", day)
	sha := hashObject(b.Bytes())
	g.objects[sha] = b.Bytes()
	g.head = sha
	return sha
}

func (g *memGit) err(name string) error { return g.fail[name] }

func (g *memGit) Toplevel(string) (string, error) { return g.top, g.err("Toplevel") }
func (g *memGit) IsShallow(string) (bool, error)  { return g.shallow, g.err("IsShallow") }
func (g *memGit) CurrentBranch(string) (string, error) {
	return g.branch, g.err("CurrentBranch")
}

func (g *memGit) FirstParentLog(string) ([]gitx.Commit, error) {
	if err := g.err("FirstParentLog"); err != nil {
		return nil, err
	}
	var out []gitx.Commit
	for sha := g.head; sha != ""; {
		ps := parents(g.objects[sha])
		out = append(out, gitx.Commit{SHA: sha, Parents: len(ps), Day: committerDay(g.objects[sha])})
		sha = ""
		if len(ps) > 0 {
			sha = ps[0]
		}
	}
	return out, nil
}

func (g *memGit) CatCommit(_, sha string) ([]byte, error) {
	if g.corrupt != nil {
		return g.corrupt, nil
	}
	raw, ok := g.objects[sha]
	if !ok {
		return nil, fmt.Errorf("no object %s", sha)
	}
	return raw, g.err("CatCommit")
}

func (g *memGit) HashCommit(_ string, raw []byte) (string, error) {
	if err := g.err("HashCommit"); err != nil {
		return "", err
	}
	sha := hashObject(raw)
	g.objects[sha] = bytes.Clone(raw)
	return sha, nil
}

func (g *memGit) UpdateRef(_, ref, newSHA, oldSHA, reason string) error {
	if err := g.err("UpdateRef"); err != nil {
		return err
	}
	if ref != g.branch || oldSHA != g.head || !strings.Contains(reason, "salt prune") {
		return fmt.Errorf("update-ref %s %s %s: branch moved", ref, newSHA, oldSHA)
	}
	g.head = newSHA
	return nil
}

func (g *memGit) ReclaimSpace(string) error {
	g.reclaims++
	return g.err("ReclaimSpace")
}

// committerDay reads the committer date as git log would, in UTC here.
func committerDay(raw []byte) string {
	for _, l := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(l, "committer "); ok {
			f := strings.Fields(rest)
			sec, _ := strconv.ParseInt(f[len(f)-2], 10, 64)
			return time.Unix(sec, 0).UTC().Format(time.DateOnly)
		}
	}
	return ""
}

func parents(raw []byte) []string {
	header, _, _ := bytes.Cut(raw, []byte("\n\n"))
	var ps []string
	for _, l := range strings.Split(string(header), "\n") {
		if p, ok := strings.CutPrefix(l, "parent "); ok {
			ps = append(ps, p)
		}
	}
	return ps
}

// branchTrees lists the tree and message of each commit on the branch,
// newest first.
func (g *memGit) branchTrees(t *testing.T) []string {
	t.Helper()
	log, _ := g.FirstParentLog("")
	var out []string
	for _, c := range log {
		raw := g.objects[c.SHA]
		tree, _, _ := bytes.Cut(raw, []byte("\n"))
		_, msg, _ := bytes.Cut(raw, []byte("\n\n"))
		out = append(out, string(tree)+" | "+strings.TrimSpace(string(msg)))
	}
	return out
}

func repoDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func commits(days ...string) []gitx.Commit {
	out := make([]gitx.Commit, len(days))
	for i, d := range days {
		out[i] = gitx.Commit{SHA: fmt.Sprint(i), Day: d}
	}
	return out
}

func TestSelect(t *testing.T) {
	for _, tt := range []struct {
		name       string
		days       []string
		keepDays   int
		keep, seen int
	}{
		{"no commits", nil, 5, 0, 0},
		{"one commit", []string{"09-15"}, 5, 1, 1},
		{"fewer days than kept", []string{"09-15", "09-14", "09-13"}, 5, 3, 3},
		{"exactly the days kept", []string{"5", "4", "3", "2", "1"}, 5, 5, 5},
		{"daily backups", []string{"8", "7", "6", "5", "4", "3", "2", "1"}, 5, 5, 5},
		// A day with no change makes no commit and is skipped: the 5 days
		// kept span 6 calendar days.
		{"a quiet day", []string{"09-15", "09-14", "09-12", "09-11", "09-10", "09-09", "09-08"}, 5, 5, 5},
		// Several backups on one day count as one day and are all kept.
		{"several on one day", []string{"3", "3", "3", "2", "2", "1"}, 2, 5, 2},
		{"one day keeps only the latest day", []string{"3", "3", "2", "1"}, 1, 2, 1},
		{"one day, daily backups", []string{"3", "2", "1"}, 1, 1, 1},
		// A commit with an odd clock ends the window where it sits.
		{"odd clock", []string{"5", "4", "1999", "3", "2", "1"}, 3, 3, 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			keep, seen := Select(commits(tt.days...), tt.keepDays)
			if keep != tt.keep || seen != tt.seen {
				t.Fatalf("Select = %d commits over %d days, want %d over %d", keep, seen, tt.keep, tt.seen)
			}
		})
	}
}

func TestReparent(t *testing.T) {
	const message = "Backup 2026-09-15\n\nwith  odd   spacing\n\n\nand blank lines\n"
	base := "tree 1111\nparent aaaa\nparent bbbb\nauthor A <a@a> 1700000000 +0100\ncommitter C <c@c> 1700000001 -0500\nencoding ISO-8859-1\n"
	sig := "gpgsig -----BEGIN PGP SIGNATURE-----\n \n abcd\n -----END PGP SIGNATURE-----\n"
	tag := "mergetag object bbbb\n type commit\n tag v1\n"
	sig256 := "gpgsig-sha256 -----BEGIN SSH SIGNATURE-----\n efgh\n -----END SSH SIGNATURE-----\n"
	raw := []byte(base + tag + sig + sig256 + "\n" + message)

	want := "tree 1111\nparent cccc\nauthor A <a@a> 1700000000 +0100\ncommitter C <c@c> 1700000001 -0500\nencoding ISO-8859-1\n\n" + message
	got, err := Reparent(raw, "cccc")
	if err != nil || string(got) != want {
		t.Fatalf("Reparent onto cccc:\n%q\nwant\n%q (%v)", got, want, err)
	}
	wantRoot := strings.Replace(want, "parent cccc\n", "", 1)
	if got, err := Reparent(raw, ""); err != nil || string(got) != wantRoot {
		t.Fatalf("Reparent as root:\n%q\nwant\n%q (%v)", got, wantRoot, err)
	}
	// Any other header, with its continuation lines, is kept.
	custom := "tree 1111\nparent aaaa\nx-note first\n second\nauthor A <a@a> 1 +0000\n\nmsg\n"
	if got, err := Reparent([]byte(custom), "p"); err != nil || string(got) != "tree 1111\nparent p\nx-note first\n second\nauthor A <a@a> 1 +0000\n\nmsg\n" {
		t.Fatalf("Reparent with a multi-line header = %q, %v", got, err)
	}
	// A header after a signature is kept, and an empty message stays empty.
	plain := "tree 1111\ngpgsig x\n y\nauthor A <a@a> 1 +0000\ncommitter C <c@c> 1 +0000\n\n"
	if got, err := Reparent([]byte(plain), "p"); err != nil || string(got) != "tree 1111\nparent p\nauthor A <a@a> 1 +0000\ncommitter C <c@c> 1 +0000\n\n" {
		t.Fatalf("Reparent of a signed commit with no message = %q, %v", got, err)
	}
	for _, bad := range []string{"", "tree 1111\nauthor A\n", "author A\n\nmsg\n", "\n\nmsg"} {
		if _, err := Reparent([]byte(bad), "p"); err == nil || !strings.Contains(err.Error(), "malformed commit") {
			t.Errorf("Reparent(%q): %v, want malformed", bad, err)
		}
	}
}

// Ten daily backups with one quiet day: pruning keeps the last 5 days with a
// change, copies each kept commit exactly apart from its parent, and cleans
// up once.
func TestRunKeepsTheLastDaysWithAChange(t *testing.T) {
	root := repoDir(t)
	g := newMemGit(root)
	var trees []string
	for d := 1; d <= 10; d++ {
		if d == 8 {
			continue // nothing changed that day, so no commit
		}
		day := fmt.Sprintf("2026-09-%02d", d)
		g.commit(day, fmt.Sprintf("t%02d", d))
		trees = append([]string{fmt.Sprintf("tree t%02d | Backup %s", d, day)}, trees...)
	}
	oldHead := g.head
	res, err := Run(g, root, 5)
	if err != nil {
		t.Fatal(err)
	}
	if *res != (Result{Kept: 5, Days: 5, Dropped: 4}) {
		t.Fatalf("result = %+v", *res)
	}
	got := g.branchTrees(t)
	if strings.Join(got, "\n") != strings.Join(trees[:5], "\n") {
		t.Fatalf("branch after prune:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(trees[:5], "\n"))
	}
	// The kept window spans 6 calendar days, 5 to 10, because the 8th had
	// no commit.
	if log, _ := g.FirstParentLog(""); log[4].Day != "2026-09-05" || log[4].Parents != 0 {
		t.Fatalf("oldest kept = %+v, want the 5th as the new first commit", log[4])
	}
	if g.reclaims != 1 {
		t.Fatalf("ReclaimSpace called %d times", g.reclaims)
	}
	// Each kept commit differs from its original only in its parent line.
	newRaw := g.objects[g.head]
	oldRaw := g.objects[oldHead]
	if !bytes.Equal(bytes.Join(dropParents(newRaw), nil), bytes.Join(dropParents(oldRaw), nil)) {
		t.Fatalf("newest commit changed beyond its parent:\n%s\n%s", newRaw, oldRaw)
	}

	// Running again finds nothing to drop and changes nothing.
	head := g.head
	res, err = Run(g, root, 5)
	if err != nil || *res != (Result{Kept: 5, Days: 5}) || g.head != head || g.reclaims != 1 {
		t.Fatalf("second prune: %+v, %v, head moved %v, reclaims %d", res, err, g.head != head, g.reclaims)
	}
}

func dropParents(raw []byte) [][]byte {
	var out [][]byte
	for _, l := range bytes.SplitAfter(raw, []byte("\n")) {
		if !bytes.HasPrefix(l, []byte("parent ")) {
			out = append(out, l)
		}
	}
	return out
}

func TestRunOneDayKeepsOnlyTheLatestDay(t *testing.T) {
	root := repoDir(t)
	g := newMemGit(root)
	g.commit("2026-09-01", "a")
	g.commit("2026-09-02", "b")
	g.commit("2026-09-02", "c")
	res, err := Run(g, root, 1)
	if err != nil || *res != (Result{Kept: 2, Days: 1, Dropped: 1}) {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if got := g.branchTrees(t); len(got) != 2 || got[0] != "tree c | Backup 2026-09-02" {
		t.Fatalf("branch = %q", got)
	}
}

// A merge commit that is kept is copied onto the line kept, so its second
// parent's history goes; its tree, a full snapshot, stays.
func TestRunFlattensAKeptMerge(t *testing.T) {
	root := repoDir(t)
	g := newMemGit(root)
	g.commit("2026-09-01", "a")
	g.commit("2026-09-02", "b")
	merge := g.commit("2026-09-03", "m", "parent 9999", "mergetag object 9999", " type commit")
	if ps := parents(g.objects[merge]); len(ps) != 2 {
		t.Fatalf("test setup: merge has parents %v", ps)
	}
	if _, err := Run(g, root, 2); err != nil {
		t.Fatal(err)
	}
	raw := g.objects[g.head]
	if ps := parents(raw); len(ps) != 1 || bytes.Contains(raw, []byte("mergetag")) {
		t.Fatalf("kept merge:\n%s", raw)
	}
	if got := g.branchTrees(t); len(got) != 2 || got[0] != "tree m | Backup 2026-09-03" {
		t.Fatalf("branch = %q", got)
	}
}

func TestRunWithNoCommits(t *testing.T) {
	root := repoDir(t)
	g := newMemGit(root)
	res, err := Run(g, root, 5)
	if err != nil || *res != (Result{}) || g.reclaims != 0 {
		t.Fatalf("Run on an empty branch = %+v, %v (reclaims %d)", res, err, g.reclaims)
	}
}

// Refusals happen before anything is written.
func TestRunRefuses(t *testing.T) {
	broken := errors.New("git broke")
	for _, tt := range []struct {
		name  string
		setup func(g *memGit, root string) string // returns the root to prune
		days  int
		want  string
		is    error
	}{
		{"zero days", nil, 0, "must be 1 or more", ErrNotPrunable},
		{"negative days", nil, -1, "must be 1 or more", ErrNotPrunable},
		{"inside another repo", func(g *memGit, root string) string {
			sub := filepath.Join(root, "backup")
			os.Mkdir(sub, 0o755)
			return sub
		}, 5, "not at its top", ErrNotPrunable},
		{"shallow clone", func(g *memGit, root string) string { g.shallow = true; return root }, 5, "fetch --unshallow", ErrNotPrunable},
		{"detached HEAD", func(g *memGit, root string) string { g.branch = ""; return root }, 5, "detached HEAD", ErrNotPrunable},
		{"toplevel fails", func(g *memGit, root string) string { g.fail["Toplevel"] = broken; return root }, 5, "git broke", broken},
		{"toplevel missing", func(g *memGit, root string) string { g.top = filepath.Join(root, "gone"); return root }, 5, "no such file", nil},
		{"root missing", func(g *memGit, root string) string { return filepath.Join(root, "gone") }, 5, "no such file", nil},
		{"shallow check fails", func(g *memGit, root string) string { g.fail["IsShallow"] = broken; return root }, 5, "git broke", broken},
		{"branch fails", func(g *memGit, root string) string { g.fail["CurrentBranch"] = broken; return root }, 5, "git broke", broken},
		{"log fails", func(g *memGit, root string) string { g.fail["FirstParentLog"] = broken; return root }, 5, "git broke", broken},
		{"cat-file fails", func(g *memGit, root string) string { g.fail["CatCommit"] = broken; return root }, 1, "git broke", broken},
		{"malformed commit", func(g *memGit, root string) string { g.corrupt = []byte("garbage"); return root }, 1, "malformed commit", nil},
		{"hash-object fails", func(g *memGit, root string) string { g.fail["HashCommit"] = broken; return root }, 1, "git broke", broken},
		{"branch moved", func(g *memGit, root string) string { g.fail["UpdateRef"] = broken; return root }, 1, "git broke", broken},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := repoDir(t)
			g := newMemGit(root)
			g.commit("2026-09-01", "a")
			g.commit("2026-09-02", "b")
			head := g.head
			target := root
			if tt.setup != nil {
				target = tt.setup(g, root)
			}
			res, err := Run(g, target, tt.days)
			if err == nil || res != nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run = %+v, %v; want an error containing %q", res, err, tt.want)
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Fatalf("Run error %v is not %v", err, tt.is)
			}
			if g.head != head || g.reclaims != 0 {
				t.Fatalf("a refused prune changed the branch (%v) or cleaned up (%d)", g.head != head, g.reclaims)
			}
		})
	}
}

// The root may be reached through a symlink, as on macOS where /var is a
// link to /private/var and git reports the real path.
func TestRunAcceptsASymlinkedRoot(t *testing.T) {
	root := repoDir(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skip("symlink:", err)
	}
	g := newMemGit(root)
	g.commit("2026-09-01", "a")
	g.commit("2026-09-02", "b")
	if res, err := Run(g, link, 1); err != nil || res.Dropped != 1 {
		t.Fatalf("Run through a symlink = %+v, %v", res, err)
	}
}

// A relative root is compared with git's absolute top folder correctly.
func TestRunAcceptsARelativeRoot(t *testing.T) {
	root := repoDir(t)
	t.Chdir(filepath.Dir(root))
	g := newMemGit(root)
	g.commit("2026-09-01", "a")
	g.commit("2026-09-02", "b")
	if res, err := Run(g, filepath.Base(root), 1); err != nil || res.Dropped != 1 {
		t.Fatalf("Run with a relative root = %+v, %v", res, err)
	}
}

// When cleaning up fails the old backups are already gone from the branch,
// so the result is still returned along with the error.
func TestRunReportsAFailedCleanup(t *testing.T) {
	root := repoDir(t)
	g := newMemGit(root)
	g.commit("2026-09-01", "a")
	g.commit("2026-09-02", "b")
	g.fail["ReclaimSpace"] = errors.New("disk full")
	res, err := Run(g, root, 1)
	if !errors.Is(err, ErrCleanup) || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("Run error = %v, want ErrCleanup", err)
	}
	if res == nil || res.Dropped != 1 || len(g.branchTrees(t)) != 1 {
		t.Fatalf("Run result = %+v, branch %q", res, g.branchTrees(t))
	}
}
