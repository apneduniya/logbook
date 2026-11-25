package admin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"time"
)

// Usage is the help text of `logbook-server admin`.
const Usage = `usage:
  logbook-server admin grant <username>             make a user an admin
  logbook-server admin revoke <username>            take the role away and revoke all their admin keys
  logbook-server admin key create <username> <name> make an admin key (printed once)
  logbook-server admin key list                     list every admin key
  logbook-server admin key revoke <prefix>          revoke one admin key`

// ErrUsage means the arguments don't name a command; Usage says which exist.
var ErrUsage = errors.New("unknown admin command")

// Command runs one `logbook-server admin …` command (args are what follows
// "admin") and prints its result to out. Each command is audited.
func Command(ctx context.Context, k *Keys, args []string, out io.Writer) error {
	switch {
	case len(args) == 2 && args[0] == "grant":
		changed, err := k.Grant(ctx, args[1])
		if err != nil {
			return err
		}
		if !changed {
			return printf(out, "%s is already an admin\n", args[1])
		}
		return printf(out, "%s is now an admin\n", args[1])
	case len(args) == 2 && args[0] == "revoke":
		n, err := k.Revoke(ctx, args[1])
		if err != nil {
			return err
		}
		return printf(out, "%s is no longer an admin; %d admin key(s) revoked\n", args[1], n)
	case len(args) == 4 && args[0] == "key" && args[1] == "create":
		nk, err := k.CreateKey(ctx, args[2], args[3])
		if err != nil {
			return err
		}
		return printf(out, "admin key for %s (prefix %s); it is shown only this once:\n%s\n", args[2], nk.Prefix, nk.Key)
	case len(args) == 2 && args[0] == "key" && args[1] == "list":
		keys, err := k.ListKeys(ctx)
		if err != nil {
			return err
		}
		return writeKeys(out, keys)
	case len(args) == 3 && args[0] == "key" && args[1] == "revoke":
		if err := k.RevokeKey(ctx, args[2]); err != nil {
			return err
		}
		return printf(out, "revoked %s\n", args[2])
	default:
		return ErrUsage
	}
}

func printf(out io.Writer, format string, args ...any) error {
	if _, err := fmt.Fprintf(out, format, args...); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}

// writeKeys prints keys as a table.
func writeKeys(out io.Writer, keys []KeyInfo) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PREFIX\tUSER\tNAME\tCREATED\tLAST USED\tSTATUS") // tabwriter keeps errors for Flush
	for _, key := range keys {
		status := "live"
		switch {
		case key.RevokedAt != nil:
			status = "revoked " + key.RevokedAt.UTC().Format(time.RFC3339)
		case key.UserRole != RoleAdmin:
			status = "inactive (user isn't an admin)"
		}
		used := "never"
		if key.LastUsedAt != nil {
			used = key.LastUsedAt.UTC().Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", key.Prefix, key.Username, key.Name,
			key.CreatedAt.UTC().Format(time.RFC3339), used, status)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}
