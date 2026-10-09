package seal

import (
	"os"
	"slices"
	"testing"
)

// CachedPaths lists every file the last seal sealed, and nothing when there
// is no cache or it is damaged.
func TestCachedPaths(t *testing.T) {
	f := newFixture(t, true)
	if got := CachedPaths(f.cache, f.root); got != nil {
		t.Fatalf("before any seal: %v", got)
	}
	f.seal(false)
	want := []string{"NOTES.md", "data/store.db", "notes/facts.md", "notes/profile.md", "projects/tax-return-2026/plan.md"}
	if got := CachedPaths(f.cache, f.root); !slices.Equal(got, want) {
		t.Fatalf("CachedPaths = %v, want %v", got, want)
	}
	_, p := f.loadCache()
	os.WriteFile(p, []byte("{not json"), 0o600)
	if got := CachedPaths(f.cache, f.root); got != nil {
		t.Fatalf("damaged cache: %v", got)
	}
}
