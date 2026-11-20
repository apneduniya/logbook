package wakacfg

import (
	"bytes"
	"errors"
	"strings"
)

// doc is a .wakatime.cfg file held as its original lines, so that editing a
// key changes that one line and leaves every other byte alone: comments,
// blank lines, other sections, key order, quoting, the BOM and the line
// endings. It understands the subset of INI that wakatime-cli (go-ini with
// AllowPythonMultilineValues) reads:
//   - "[section]" headers, optionally followed by "; comment";
//   - "key = value" and "key: value" lines (the first '=' or ':' splits);
//   - '#' and ';' comment lines;
//   - indented continuation lines after a key (Python-style multiline).
//
// Section and key names match case-insensitively, as viper (which
// wakatime-cli reads the file through) lowercases them. A key outside any
// section is in the default section, named "".
type doc struct {
	bom     bool
	newline string
	lines   []line
	// trailingNewline records whether the file ended with a line break.
	trailingNewline bool
}

type lineKind int

const (
	lineOther   lineKind = iota // blank, comment or unrecognised
	lineSection                 // [name]
	lineKey                     // key = value
	lineCont                    // indented continuation of the key above
)

type line struct {
	text    string
	kind    lineKind
	section string // lowercased section this line belongs to
	key     string // lowercased key name, for lineKey and lineCont
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// parseDoc splits b into lines and classifies them. It never fails: lines it
// does not understand are kept verbatim, as wakatime-cli skips them too.
func parseDoc(b []byte) *doc {
	d := &doc{newline: "\n"}
	if bytes.HasPrefix(b, utf8BOM) {
		d.bom = true
		b = b[len(utf8BOM):]
	}
	s := string(b)
	if strings.Contains(s, "\r\n") {
		d.newline = "\r\n"
	}
	if s == "" {
		return d
	}
	d.trailingNewline = strings.HasSuffix(s, "\n")
	s = strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r")
	section, lastKey := "", ""
	for _, raw := range strings.Split(s, "\n") {
		text := strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimSpace(text)
		l := line{text: text, section: section}
		switch {
		case trimmed == "" || trimmed[0] == '#' || trimmed[0] == ';':
			// Blank lines end a multiline value; comment lines inside one are
			// skipped by go-ini, so they do not end it.
			if trimmed == "" {
				lastKey = ""
			}
		case lastKey != "" && (text[0] == ' ' || text[0] == '\t'):
			l.kind, l.key = lineCont, lastKey
		case trimmed[0] == '[':
			if name, ok := sectionName(trimmed); ok {
				section = strings.ToLower(name)
				l.kind, l.section = lineSection, section
			}
			lastKey = ""
		default:
			if name, _, ok := splitKey(text); ok {
				l.kind, l.key = lineKey, strings.ToLower(name)
				lastKey = l.key
			}
		}
		d.lines = append(d.lines, l)
	}
	return d
}

// sectionName returns the name inside "[name]". The name ends at the last
// ']' (go-ini's rule), and anything after it must be a comment.
func sectionName(trimmed string) (string, bool) {
	end := strings.LastIndexByte(trimmed, ']')
	if end < 1 {
		return "", false
	}
	return strings.TrimSpace(trimmed[1:end]), true
}

// splitKey splits a key line at its first '=' or ':'. It returns the key
// name and the index just past the delimiter.
func splitKey(text string) (string, int, bool) {
	i := strings.IndexAny(text, "=:")
	if i < 0 {
		return "", 0, false
	}
	name := strings.TrimSpace(text[:i])
	if name == "" {
		return "", 0, false
	}
	return name, i + 1, true
}

// get returns the value of key in section. When a key appears more than
// once, the last one wins, as in go-ini.
func (d *doc) get(section, key string) (string, bool) {
	section, key = strings.ToLower(section), strings.ToLower(key)
	val, found := "", false
	for i, l := range d.lines {
		if l.kind != lineKey || l.section != section || l.key != key {
			continue
		}
		_, at, _ := splitKey(l.text)
		parts := []string{plainValue(l.text[at:])}
		for _, c := range d.lines[i+1:] {
			if c.kind != lineCont {
				if c.kind == lineOther && strings.TrimSpace(c.text) != "" {
					continue // comment inside a multiline value
				}
				break
			}
			parts = append(parts, strings.TrimSpace(c.text))
		}
		val, found = strings.TrimSpace(strings.Join(parts, "\n")), true
	}
	return val, found
}

// plainValue applies go-ini's rules for a one-line value: cut an inline
// comment, then strip one pair of surrounding quotes. A quoted value keeps
// comment symbols inside the quotes.
func plainValue(v string) string {
	v = strings.TrimSpace(v)
	for _, q := range []string{`"`, `'`, "`"} {
		if strings.HasPrefix(v, q) {
			if end := strings.Index(v[1:], q); end >= 0 {
				rest := strings.TrimSpace(v[end+2:])
				if rest == "" || rest[0] == '#' || rest[0] == ';' {
					return v[1 : end+1]
				}
			}
		}
	}
	if i := strings.IndexAny(v, "#;"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// errBadValue means a value would not survive a round trip through go-ini.
var errBadValue = errors.New("value contains a line break or comment character")

// set gives key the value in every place it appears in section, keeping
// each line's key spelling and delimiter. Continuation lines of the old
// value are dropped. A missing key is added at the end of the last block of
// that section, creating the section at the end of the file if needed.
func (d *doc) set(section, key, value string) error { //nolint:unparam // a general INI editor; only [settings] is edited today
	if strings.ContainsAny(value, "\r\n#;") {
		return errBadValue
	}
	section, key = strings.ToLower(section), strings.ToLower(key)
	found := false
	out := d.lines[:0:0]
	for i := 0; i < len(d.lines); i++ {
		l := d.lines[i]
		if l.kind == lineCont && l.section == section && l.key == key {
			continue
		}
		if l.kind == lineKey && l.section == section && l.key == key {
			_, at, _ := splitKey(l.text)
			rest := l.text[at:]
			ws := rest[:len(rest)-len(strings.TrimLeft(rest, " \t"))]
			if ws == rest || (ws == "" && strings.HasSuffix(l.text[:at-1], " ")) {
				ws = " " // "k =" or "k =v": keep "k = v" symmetric
			}
			l.text = l.text[:at] + ws + value
			found = true
		}
		out = append(out, l)
	}
	d.lines = out
	if found {
		return nil
	}
	newLine := line{text: key + " = " + value, kind: lineKey, section: section, key: key}
	if at := d.insertPoint(section); at >= 0 {
		d.lines = append(d.lines[:at], append([]line{newLine}, d.lines[at:]...)...)
		return nil
	}
	if section == "" {
		d.lines = append([]line{newLine}, d.lines...)
		return nil
	}
	if n := len(d.lines); n > 0 && strings.TrimSpace(d.lines[n-1].text) != "" {
		d.lines = append(d.lines, line{section: d.lines[n-1].section})
	}
	d.lines = append(d.lines,
		line{text: "[" + section + "]", kind: lineSection, section: section},
		newLine)
	d.trailingNewline = true
	return nil
}

// insertPoint returns the index just after the last key (or the header) of
// the last block of section, or -1 if the section does not exist. Blank
// lines and comments that follow stay after the new key.
func (d *doc) insertPoint(section string) int {
	at := -1
	for i, l := range d.lines {
		if l.section != section {
			continue
		}
		switch l.kind {
		case lineSection, lineKey, lineCont:
			at = i + 1
		case lineOther:
		}
	}
	if at < 0 && section == "" {
		return -1
	}
	return at
}

// remove deletes every occurrence of key in section, with its
// continuation lines. It reports whether anything was removed.
func (d *doc) remove(section, key string) bool {
	section, key = strings.ToLower(section), strings.ToLower(key)
	out := d.lines[:0:0]
	removed := false
	for _, l := range d.lines {
		if (l.kind == lineKey || l.kind == lineCont) && l.section == section && l.key == key {
			removed = true
			continue
		}
		out = append(out, l)
	}
	d.lines = out
	return removed
}

// bytes renders the document with its original BOM and line endings.
func (d *doc) bytes() []byte {
	var b bytes.Buffer
	if d.bom {
		b.Write(utf8BOM)
	}
	for i, l := range d.lines {
		b.WriteString(l.text)
		if i < len(d.lines)-1 || d.trailingNewline {
			b.WriteString(d.newline)
		}
	}
	return b.Bytes()
}
