package gitx

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
)

func TestReadRaw(t *testing.T) {
	a, b, z := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("0", 40)
	entry := func(old, new, status, path string) string {
		return ":100644 100644 " + old + " " + new + " " + status + "\x00" + path + "\x00"
	}
	// Two commits, each starting on a new line. The same blob at two paths
	// is listed at both, and once at each.
	out := "\n" + entry(z, a, "A", "objects/aa/x.age") + entry(z, a, "A", "README.md") +
		"\n" + entry(b, a, "M", "objects/aa/x.age") + entry(z, b, "A", "odd\nname.md")
	got, err := readRaw(strings.NewReader(out))
	want := []Blob{{a, "objects/aa/x.age"}, {a, "README.md"}, {b, "odd\nname.md"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("readRaw = %v, %v", got, err)
	}
	if got, err := readRaw(strings.NewReader("\x00\n\x00")); err != nil || got != nil {
		t.Fatalf("readRaw of nothing = %v, %v", got, err)
	}
	broken := io.MultiReader(strings.NewReader("\n"+entry(z, a, "A", "x")), iotest.ErrReader(errors.New("pipe broke")))
	if _, err := readRaw(broken); err == nil || !strings.Contains(err.Error(), "pipe broke") {
		t.Fatalf("readRaw of a broken read = %v", err)
	}
	for _, bad := range []string{
		"commit " + a + "\x00",
		":100644 100644 " + z + " " + a + "\x00",
		entry(z, a, "A", "x")[:60] + "\x00",
		":100644 100644 " + z + " " + a + " A\x00no-end",
	} {
		if _, err := readRaw(strings.NewReader(bad)); err == nil {
			t.Errorf("readRaw(%.30q) succeeded", bad)
		}
	}
}
