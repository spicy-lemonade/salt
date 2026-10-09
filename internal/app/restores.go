package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/spicy-lemonade/salt/internal/seal"
)

// ErrInterrupted means the person stopped a restore or a seal (Ctrl-C or
// SIGTERM) and the files it had written were removed.
var ErrInterrupted = errors.New("interrupted")

// restoreLog lists the temporary folders of restores in progress. A restore
// that is killed outright (SIGKILL, a power cut) cannot clean up, and its
// folder may hold decrypted files, so doctor reports any still listed.
type restoreLog struct{ path string }

func (a *App) restoreLog() restoreLog {
	return restoreLog{filepath.Join(a.CacheDir, "restores.json")}
}

func (l restoreLog) load() ([]string, error) {
	b, err := os.ReadFile(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var dirs []string
	if err := json.Unmarshal(b, &dirs); err != nil {
		return nil, fmt.Errorf("%s: %w", l.path, err)
	}
	return dirs, nil
}

func (l restoreLog) save(dirs []string) error {
	b, err := json.Marshal(dirs)
	if err != nil {
		return err
	}
	return seal.WritePrivate(l.path, b)
}

// add records dir. Entries whose folder is already gone are dropped, and an
// unreadable list is started afresh, since it only helps doctor.
func (l restoreLog) add(dir string) error {
	dirs, _ := l.load()
	kept := []string{dir}
	for _, d := range dirs {
		if _, err := os.Lstat(d); err == nil && d != dir {
			kept = append(kept, d)
		}
	}
	return l.save(kept)
}

func (l restoreLog) remove(dir string) error {
	dirs, err := l.load()
	if err != nil || !slices.Contains(dirs, dir) {
		return err
	}
	return l.save(slices.DeleteFunc(dirs, func(d string) bool { return d == dir }))
}

// leftovers are listed folders that still exist.
func (l restoreLog) leftovers() ([]string, error) {
	dirs, err := l.load()
	var out []string
	for _, d := range dirs {
		if _, err := os.Lstat(d); err == nil {
			out = append(out, d)
		}
	}
	return out, err
}

// trackRestore records a restore's temporary folder until it is gone.
func (a *App) trackRestore(tmp string) func() {
	log := a.restoreLog()
	if err := log.add(tmp); err != nil {
		a.UI.Printf("salt: could not record the restore in progress (%v); if it is interrupted, delete %s yourself\n", err, a.short(tmp))
		return func() {}
	}
	return func() { log.remove(tmp) }
}

// warnLeftoverRestores points out folders an earlier, unfinished restore left
// next to dest.
func (a *App) warnLeftoverRestores(dest string) {
	abs, err := filepath.Abs(dest)
	if err != nil {
		return
	}
	found, _ := filepath.Glob(filepath.Join(filepath.Dir(abs), ".salt-restore-*"))
	for _, d := range found {
		a.UI.Printf("salt: %s was left by a restore that did not finish and may hold decrypted files; delete it once you have checked it\n", a.short(d))
	}
}

func (a *App) doctorRestores(r *report) {
	dirs, err := a.restoreLog().leftovers()
	if err != nil {
		r.add(warn, "could not read the list of restores in progress: %v", err)
	}
	for _, d := range dirs {
		r.add(warn, "%s was left by a restore that did not finish and may hold decrypted files; delete it once you have checked it", a.short(d))
	}
}
