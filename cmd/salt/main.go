// Command salt encrypts AI-agent memory backups before they leave the machine.
package main

import (
	"fmt"
	"os"

	"github.com/spicy-lemonade/salt/internal/guard"
)

var version = "dev"

const usage = `salt encrypts your agent's memory backups before they are pushed.

Usage:
  salt version

More commands are coming; see docs/design.md.
`

func main() {
	if err := guard.Enter(); err != nil {
		fmt.Fprintln(os.Stderr, "salt:", err)
		os.Exit(3)
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version", "--version":
		fmt.Println("salt", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "salt: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
