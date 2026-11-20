package cmdline

import (
	"path"
	"strings"
)

// Shell is the interpreter a harness hands a command string to.
type Shell int

// Shells harnesses use for string-form hook commands.
const (
	// POSIX is sh/bash/zsh: `sh -c`, `bash -c` or `$SHELL -lc`.
	POSIX Shell = iota
	// Cmd is Windows cmd.exe (`cmd /C`).
	Cmd
	// PowerShell is Windows PowerShell 5.1 or pwsh (`-Command`).
	PowerShell
)

// HookArgs returns the arguments after the binary for a hook command.
func HookArgs(harness, event string) []string {
	return []string{"hook", harness, event}
}

// Hook returns the full argv of a hook command.
func Hook(binPath, harness, event string) []string {
	return append([]string{binPath}, HookArgs(harness, event)...)
}

// Quote joins argv into one command string for sh. Arguments that need it
// are quoted; plain ones are left bare so the result stays readable.
func Quote(sh Shell, argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = quoteArg(sh, a)
	}
	s := strings.Join(parts, " ")
	// PowerShell treats a quoted first token as a string expression, not a
	// command, so it needs the call operator to run it.
	if sh == PowerShell && len(argv) > 0 && parts[0] != argv[0] {
		s = "& " + s
	}
	return s
}

func quoteArg(sh Shell, a string) string {
	if a != "" && !strings.ContainsFunc(a, needsQuote(sh)) {
		return a
	}
	switch sh {
	case Cmd:
		// cmd.exe has no escape for '"' inside quotes; paths can't contain
		// one on Windows, so plain double quotes are enough.
		return `"` + a + `"`
	case PowerShell:
		return "'" + strings.ReplaceAll(a, "'", "''") + "'"
	default:
		return "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
}

func needsQuote(sh Shell) func(rune) bool {
	return func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return false
		}
		switch r {
		case '-', '_', '.', '/', ',', '+', '=', '@':
			return false
		case ':', '\\':
			return sh == POSIX // Windows paths are fine bare in cmd and PowerShell
		}
		return true
	}
}

// Split breaks a command string into words, undoing the quoting Quote adds
// for any of the shells: single quotes (including PowerShell's doubled quote
// and the POSIX quote-backslash-quote escape), double quotes, and a leading
// PowerShell "&". It isn't a full shell parser; it only
// needs to read back Logbook's own command lines and not mistake anyone
// else's for one.
func Split(s string) []string {
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '\'':
			inWord = true
			for i++; i < len(s); i++ {
				if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' { // PowerShell ''
						cur.WriteByte('\'')
						i++
						continue
					}
					break
				}
				cur.WriteByte(s[i])
			}
		case c == '"':
			inWord = true
			for i++; i < len(s) && s[i] != '"'; i++ {
				cur.WriteByte(s[i])
			}
		case c == '\\' && i+1 < len(s) && s[i+1] == '\'': // POSIX '\''
			inWord = true
			cur.WriteByte('\'')
			i++
		default:
			inWord = true
			cur.WriteByte(c)
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	if len(words) > 0 && words[0] == "&" {
		words = words[1:]
	}
	return words
}

// IsHook reports whether argv is a Logbook hook command for harness: a binary
// whose name starts with "logbook", then "hook <harness> <event>". The binary
// path itself may differ (the user may have moved Logbook since install), so
// only its base name is checked.
func IsHook(argv []string, harness string) bool {
	if len(argv) != 4 || argv[1] != "hook" || argv[2] != harness || argv[3] == "" {
		return false
	}
	return strings.HasPrefix(strings.ToLower(baseName(argv[0])), "logbook")
}

// baseName returns the last element of a path written with either kind of
// separator, whatever OS this runs on.
func baseName(p string) string {
	return path.Base(strings.ReplaceAll(p, `\`, "/"))
}
