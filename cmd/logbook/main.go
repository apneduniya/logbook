// Command logbook is the Logbook agent and CLI. It runs in the background,
// passes editor heartbeats on, senses activity and keeps evidence locally.
// Built in step 2 of docs/ARCHITECTURE.md.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "logbook agent: not implemented yet (see docs/ARCHITECTURE.md, step 2)")
	os.Exit(2)
}
