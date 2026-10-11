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

// ErrInterrupted means the person stopped a command (Ctrl-C or SIGTERM), or
// a question it asked, and the files it had written were removed.
var ErrInterrupted = errors.New("interrupted")

// stopped says, for err from a question the person stopped
// (ErrInterrupted), that the command cmd was interrupted, followed by then,
// such as ": nothing was saved". Any other err is returned as it is.
func stopped(err error, cmd, then string) error {
	if errors.Is(err, ErrInterrupted) {
		return fmt.Errorf("%s %w%s", cmd, ErrInterrupted, then)
	}
	return err
}

// restoreLog records the temporary folders of restores in progress, one
// file for each in dir, so restores running at once never rewrite each
// other's record. A restore that is killed outright (SIGKILL, a power cut)
// cannot clean up, and its folder may hold decrypted files, so doctor
// reports any still recorded. older is the one list an older salt kept for
// them all, which is only read, and removed once it names no folder that
// still exists, or is damaged.
type restoreLog struct{ dir, older string }

func (a *App) restoreLog() restoreLog {
	return restoreLog{dir: filepath.Join(a.CacheDir, "restores"), older: filepath.Join(a.CacheDir, "restores.json")}
}

// recordName is the length of a record's name: 8 bytes of the SHA-256 of
// the folder it records, in hex.
const recordName = 16

// record is the file in l.dir that records the folder tmp.
func (l restoreLog) record(tmp string) string {
	sum := sha256.Sum256([]byte(tmp))
	return filepath.Join(l.dir, hex.EncodeToString(sum[:recordName/2]))
}

// add records tmp. Records of folders already gone are then removed, and so
// is an older salt's list, once it has been read and none of its folders
// exist, or it is damaged, since it can never be read. One that cannot be
// read now, but may be later, is kept. Losing any of that only leaves
// something doctor skips.
func (l restoreLog) add(tmp string) error {
	if err := private.Write(l.record(tmp), []byte(tmp)); err != nil {
		return err
	}
	records, _ := l.records()
	for p, d := range records {
		if !exists(d) {
			os.Remove(p)
		}
	}
	older, err := l.loadOlder()
	if errors.Is(err, errDamagedList) || (err == nil && !slices.ContainsFunc(older, exists)) {
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

// records maps each record in l.dir to the folder it records. Only files
// named as record names them are records, so a temporary file private.Write
// left behind is never read as one.
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
		out[p] = string(b)
	}
	return out, errors.Join(errs...)
}

// isRecordName reports whether name is recordName hex digits, as record
// names a record.
func isRecordName(name string) bool {
	return len(name) == recordName && strings.Trim(name, "0123456789abcdef") == ""
}

// errDamagedList means an older salt's list is not the JSON it wrote.
var errDamagedList = errors.New("it is damaged")

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
		return nil, fmt.Errorf("%s: %w (%v)", l.older, errDamagedList, err)
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
		if exists(d) {
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

// exists reports whether something is at path p, without following a
// symlink there.
func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
