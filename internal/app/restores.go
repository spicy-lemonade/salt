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
	"time"
)

// ErrInterrupted means the person stopped a restore or a seal (Ctrl-C or
// SIGTERM) and the files it had written were removed.
var ErrInterrupted = errors.New("interrupted")

// restoreLog records the temporary folders of restores in progress, one
// file for each in dir, so restores running at once never rewrite each
// other's record. A restore that is killed outright (SIGKILL, a power cut)
// cannot clean up, and its folder may hold decrypted files, so doctor
// reports any still recorded. older is the one list an older salt kept for
// them all, which is only read, and removed once it names no folder that
// still exists.
type restoreLog struct{ dir, older string }

func (a *App) restoreLog() restoreLog {
	return restoreLog{dir: filepath.Join(a.CacheDir, "restores"), older: filepath.Join(a.CacheDir, "restores.json")}
}

// staleRecord is how old a record cut short must be before add removes it.
// Writing one takes far less.
const staleRecord = time.Minute

// recordName is the length of a record's name: 8 bytes of the SHA-256 of
// the folder it records, in hex.
const recordName = 16

// record is the file in l.dir that records the folder tmp.
func (l restoreLog) record(tmp string) string {
	sum := sha256.Sum256([]byte(tmp))
	return filepath.Join(l.dir, hex.EncodeToString(sum[:recordName/2]))
}

// add records tmp. It is written in place, not through a temporary file as
// private.Write does, so no temporary file is ever left in l.dir. A record
// ends with a newline, so one cut short by a crash names no folder. Records
// of folders already gone are then removed, and so are those cut short once
// they are older than staleRecord, as a younger one may be another salt's
// still being written. So is an older salt's list, once it has been read and
// none of its folders exist. Losing any of that only leaves something doctor
// skips.
func (l restoreLog) add(tmp string) error {
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(l.record(tmp), []byte(tmp+"\n"), 0o600); err != nil {
		return err
	}
	exists := func(d string) bool { _, err := os.Lstat(d); return err == nil }
	records, _ := l.records()
	for p, d := range records {
		if d != "" && !exists(d) {
			os.Remove(p)
		} else if fi, err := os.Stat(p); d == "" && err == nil && time.Since(fi.ModTime()) > staleRecord {
			os.Remove(p)
		}
	}
	if older, err := l.loadOlder(); err == nil && !slices.ContainsFunc(older, exists) {
		os.Remove(l.older)
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

// records maps each record in l.dir to the folder it records, or to "" for
// one cut short. Only files named as add names them are records.
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
		if !isRecordName(e.Name()) {
			continue
		}
		p := filepath.Join(l.dir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		d, complete := strings.CutSuffix(string(b), "\n")
		if !complete {
			d = ""
		}
		out[p] = d
	}
	return out, errors.Join(errs...)
}

// isRecordName reports whether name is recordName hex digits, as record
// names a record.
func isRecordName(name string) bool {
	return len(name) == recordName && strings.Trim(name, "0123456789abcdef") == ""
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
