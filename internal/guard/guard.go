// Package guard stops salt from running inside itself.
//
// salt starts git, sqlite3 and pg_dump. If any of them fires a hook that runs
// salt again, the nested salt would start more children, and so on until the
// machine runs out of memory. Enter marks the environment on start so every
// child inherits the mark, and refuses to run when the mark is already there.
package guard

import (
	"errors"
	"os"
	"runtime/debug"
)

// EnvActive is set in salt's environment while it runs.
const EnvActive = "SALT_ACTIVE"

// DefaultMemoryLimit is the soft heap limit when GOMEMLIMIT is not set.
const DefaultMemoryLimit = 512 << 20

// ErrNested is returned when salt is started by a process that salt started.
var ErrNested = errors.New("salt is already running in a parent process (" + EnvActive +
	" is set); refusing to start a nested salt")

// Check reports whether starting salt is allowed in the given environment
// lookup. It is separate from Enter so it can be tested without touching the
// process environment.
func Check(lookup func(string) (string, bool)) error {
	if v, ok := lookup(EnvActive); ok && v != "" {
		return ErrNested
	}
	return nil
}

// Enter checks for nesting, marks the environment for children and applies
// the default soft memory limit.
func Enter() error {
	if err := Check(os.LookupEnv); err != nil {
		return err
	}
	if err := os.Setenv(EnvActive, "1"); err != nil {
		return err
	}
	if _, ok := os.LookupEnv("GOMEMLIMIT"); !ok {
		debug.SetMemoryLimit(DefaultMemoryLimit)
	}
	return nil
}
