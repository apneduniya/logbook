// Package focusclass classifies the focused application into a coarse kind
// (editor, terminal, browser, ai, other or unknown) from its process name or
// macOS bundle identifier (ADR 0007). It is a pure, data-driven lookup: it
// does no I/O and never sees window titles.
package focusclass
