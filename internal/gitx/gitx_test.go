package gitx

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Hooks, compression and the delta search are off for every git command.
func TestArgsDisablesHooksAndCompression(t *testing.T) {
	got := Args("/repo", "commit", "-m", "x")
	want := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.looseCompression=0",
		"-c", "pack.compression=0",
		"-c", "pack.window=0",
		"-C", "/repo", "commit", "-m", "x",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Args() = %q, want %q", got, want)
	}
}

func TestBlobSpec(t *testing.T) {
	for _, tt := range []struct{ rev, path, want string }{
		{"", "x", ":0:x"},
		{"", "0:x", ":0:0:x"}, // must not read as stage 0 of "x"
		{"", "2:a/b", ":0:2:a/b"},
		{"HEAD", "0:x", "HEAD:0:x"},
	} {
		if got := BlobSpec(tt.rev, tt.path); got != tt.want {
			t.Errorf("BlobSpec(%q, %q) = %q, want %q", tt.rev, tt.path, got, tt.want)
		}
	}
}

func TestSplitZ(t *testing.T) {
	if got := splitZ([]byte("a\x00b c\x00line\nbreak\x00")); !slices.Equal(got, []string{"a", "b c", "line\nbreak"}) {
		t.Fatalf("splitZ = %q", got)
	}
	if got := splitZ(nil); len(got) != 0 {
		t.Fatalf("splitZ(nil) = %q", got)
	}
}

func TestReadCheckAttr(t *testing.T) {
	out := "index.age\x00text\x00unset\x00odd\nname.age\x00eol\x00crlf\x00"
	var got []Attr
	err := ReadCheckAttr(strings.NewReader(out), func(a Attr) error {
		got = append(got, a)
		return nil
	})
	want := []Attr{{"index.age", "text", "unset"}, {"odd\nname.age", "eol", "crlf"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("ReadCheckAttr = %v, %v; want %v", got, err, want)
	}
	if err := ReadCheckAttr(strings.NewReader(""), func(Attr) error { return nil }); err != nil {
		t.Fatalf("empty output: %v", err)
	}
	for _, cut := range []string{"index.age", "index.age\x00", "index.age\x00text\x00", "index.age\x00text\x00unset"} {
		if err := ReadCheckAttr(strings.NewReader(cut), func(Attr) error { return nil }); err == nil {
			t.Errorf("truncated output %q accepted", cut)
		}
	}
	stop := errors.New("stop")
	if err := ReadCheckAttr(strings.NewReader(out), func(Attr) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("callback error = %v", err)
	}
}

func TestParseLog(t *testing.T) {
	out := "aaaa\x00bbbb\x002026-09-15T23:30:00-05:00\n" +
		"bbbb\x00cccc dddd\x002026-09-15T06:00:00+01:00\n" +
		"cccc\x00\x002026-09-14T00:10:00+13:00\n"
	got, err := ParseLog(out)
	// Days are in each committer's own time zone: 23:30 at -05:00 is the 16th
	// in UTC, but the committer's day is the 15th.
	want := []Commit{{"aaaa", 1, "2026-09-15"}, {"bbbb", 2, "2026-09-15"}, {"cccc", 0, "2026-09-14"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("ParseLog = %v, %v; want %v", got, err, want)
	}
	if got, err := ParseLog(""); err != nil || len(got) != 0 {
		t.Fatalf("ParseLog of nothing = %v, %v", got, err)
	}
	for _, bad := range []string{
		"aaaa\x00bbbb",
		"\x00bbbb\x002026-09-15T06:00:00Z",
		"aaaa\x00bbbb\x00yesterday",
		"aaaa\x00bbbb\x002026-09-15T06:00:00Z\x00extra",
	} {
		if _, err := ParseLog(bad); err == nil {
			t.Errorf("ParseLog(%q) accepted", bad)
		}
	}
}

// readBatch gives each blob's first bytes, however large the blob, and
// reports a missing object or one that is not a blob as not a file. It reads
// the contents of every object git prints, so a commit, as a staged
// submodule names, is never read as the lines about the objects after it,
// even one whose message looks like one. It refuses output it cannot read,
// such as one that ends part way.
func TestReadBatch(t *testing.T) {
	big := "age-encryption.org/v1\n" + strings.Repeat("x", 100<<10)
	commit := "tree 1234\n\nx blob 22\nage-encryption.org/v1\n"
	out := "1111 blob 3\nabc\n" +
		":0:gone missing\n" +
		":0:two words missing\n" +
		"abcd ambiguous\n" +
		"1111111111111111111111111111111111111111 submodule\n" +
		"2222 commit " + fmt.Sprint(len(commit)) + "\n" + commit + "\n" +
		"3333 blob " + fmt.Sprint(len(big)) + "\n" + big + "\n" +
		"4444 blob 0\n\n" +
		"5555 blob 3\nxyz\n"
	type got struct {
		name, head string
		ok         bool
	}
	var seen []got
	names := []string{"a", "gone", "two words", "short", "far sub", "sub", "big", "empty", "after"}
	err := readBatch(strings.NewReader(out), names, 8, func(name string, head []byte, ok bool) {
		seen = append(seen, got{name, string(head), ok})
	})
	want := []got{{"a", "abc", true}, {"gone", "", false}, {"two words", "", false}, {"short", "", false}, {"far sub", "", false},
		{"sub", "", false}, {"big", "age-encr", true}, {"empty", "", true}, {"after", "xyz", true}}
	if err != nil || !slices.Equal(seen, want) {
		t.Fatalf("readBatch = %v, %+v; want %+v", err, seen, want)
	}

	for name, out := range map[string]string{
		"no header":          "",
		"bad size":           "1111 blob many\n",
		"negative size":      "1111 blob -1\n",
		"too many fields":    "1111 blob 3 4\n",
		"ends in the blob":   "1111 blob 10\nabc",
		"ends in the commit": "2222 commit 10\n",
	} {
		err := readBatch(strings.NewReader(out), []string{"a"}, 8, func(string, []byte, bool) {})
		if err == nil || !strings.HasPrefix(err.Error(), "git cat-file: ") {
			t.Errorf("%s: readBatch = %v, want an error from reading git cat-file", name, err)
		}
	}
}
