package app

import (
	"path/filepath"
	"strings"
)

// ShortPath shows path as "~/…" when it is inside home, for messages only. It
// returns path unchanged when home is empty, the root folder or not a parent
// of path. It must never be used for a path inside a command salt suggests: a
// quoted "~" is not expanded by the shell, so the pasted command would fail.
func ShortPath(path, home string) string {
	if home == "" || path == "" || !filepath.IsAbs(path) || !filepath.IsAbs(home) ||
		filepath.Dir(home) == home {
		return path
	}
	rel, err := filepath.Rel(home, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	if rel == "." {
		return "~"
	}
	return "~" + string(filepath.Separator) + rel
}

// short is ShortPath with this machine's home folder.
func (a *App) short(path string) string { return ShortPath(path, a.Home) }
