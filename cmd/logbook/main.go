// Command logbook is the Logbook agent and CLI (ADR 0009). It only parses
// arguments and dispatches to internal/agent packages.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"
)

// Command is one CLI subcommand. Each lives in its own cmd_<name>.go file.
type Command struct {
	Name  string
	Usage string // one line, shown by `logbook help`
	Run   func(ctx context.Context, args []string) error
}

// commands lists every subcommand. Add new ones here.
func commands() []Command {
	return []Command{}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string) int {
	cmds := commands()
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(cmds)
		return 0
	}
	for _, c := range cmds {
		if c.Name == args[0] {
			if err := c.Run(ctx, args[1:]); err != nil {
				fmt.Fprintf(os.Stderr, "logbook %s: %v\n", c.Name, err)
				return 1
			}
			return 0
		}
	}
	fmt.Fprintf(os.Stderr, "logbook: unknown command %q\n\n", args[0])
	usage(cmds)
	return 2
}

func usage(cmds []Command) {
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Name < cmds[j].Name })
	fmt.Fprintln(os.Stderr, "Usage: logbook <command> [flags]\n\nCommands:")
	for _, c := range cmds {
		fmt.Fprintf(os.Stderr, "  %-10s %s\n", c.Name, c.Usage)
	}
}
