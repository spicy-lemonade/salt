package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spicy-lemonade/salt/internal/private"
)

// ErrInterrupted means the person stopped a restore or a seal (Ctrl-C or
// SIGTERM) and the files it had written were removed.
var ErrInterrupted = errors.New("interrupted")

// restoreLog records the temporary folders of restores in progress, one
// file for each in dir, so restores running at once never rewrite each
// other's record. A restore that is killed outright (SIGKILL, a power cut)
// cannot clean up, and its folder may hold decrypted files, so doctor
// reports any still recorded. older is the one list an older salt kept for
// them all, which is still read.
type restoreLog struct{ dir, older string }

func (a *App) restoreLog() restoreLog {
	return restoreLog{dir: filepath.Join(a.CacheDir, "restores"), older: filepath.Join(a.CacheDir, "restores.json")}
}

// record is the file in l.dir that records the folder tmp.
func (l restoreLog) record(tmp string) string {
	sum := sha256.Sum256([]byte(tmp))
	return filepath.Join(l.dir, hex.EncodeToString(sum[:8]))
}

// add records tmp. Records whose folder is already gone are removed, and
// the folders in an older salt's list that still exist are recorded one by
// one instead, since it only helps doctor. Losing either only leaves a
// record doctor skips.
func (l restoreLog) add(tmp string) error {
	if err := private.Write(l.record(tmp), []byte(tmp)); err != nil {
		return err
	}
	if older, err := l.loadOlder(); err == nil {
		for _, d := range older {
			if _, err := os.Lstat(d); err == nil {
				private.Write(l.record(d), []byte(d))
			}
		}
	}
	os.Remove(l.older)
	records, _ := l.records()
	for p, d := range records {
		if _, err := os.Lstat(d); errors.Is(err, fs.ErrNotExist) {
			os.Remove(p)
		}
	}
	return nil
}

// remove forgets tmp.
func (l restoreLog) remove(tmp string) error {
	err := os.Remove(l.record(tmp))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// records maps each record in l.dir to the folder it records.
func (l restoreLog) records() (map[string]string, error) {
	entries, err := os.ReadDir(l.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	var errs []error
	for _, e := range entries {
		// One being written by private.Write is not a record yet.
		if strings.Contains(e.Name(), ".tmp-") {
			continue
		}
		p := filepath.Join(l.dir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out[p] = string(b)
	}
	return out, errors.Join(errs...)
}

// loadOlder reads an older salt's list, or none when there is none.
func (l restoreLog) loadOlder() ([]string, error) {
	b, err := os.ReadFile(l.older)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var dirs []string
	if err := json.Unmarshal(b, &dirs); err != nil {
		return nil, fmt.Errorf("%s: %w", l.older, err)
	}
	return dirs, nil
}

// leftovers are the recorded folders that still exist, in order. It changes
// nothing, so doctor can call it. A record or list that cannot be read is
// reported, along with the folders the others name.
func (l restoreLog) leftovers() ([]string, error) {
	records, err := l.records()
	older, olderErr := l.loadOlder()
	var out []string
	for _, d := range slices.Concat(slices.Collect(maps.Values(records)), older) {
		if _, err := os.Lstat(d); err == nil {
			out = append(out, d)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), errors.Join(err, olderErr)
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
