package gitwatch

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Separators in the git log format: a record separator before each commit
// and a unit separator between fields. They can't appear in hashes, numbers
// or (in practice) email addresses.
const (
	recordSep = "\x1e"
	fieldSep  = "\x1f"
)

// logFormat is passed as --format: hash, author time, author email.
const logFormat = "--format=" + recordSep + "%H" + fieldSep + "%at" + fieldSep + "%ae"

// diffStat is one --shortstat summary.
type diffStat struct {
	Files, Insertions, Deletions int
}

// commit is one parsed git log record.
type commit struct {
	Hash        string
	AuthorTime  time.Time
	AuthorEmail string
	Stat        diffStat
}

var shortstatRE = regexp.MustCompile(`(\d+) (files? changed|insertions?\(\+\)|deletions?\(-\))`)

// parseShortstat parses a --shortstat line such as
// " 3 files changed, 10 insertions(+), 2 deletions(-)". Missing parts are 0,
// and empty input (no changes) is all zeros. LC_ALL=C keeps it in English.
func parseShortstat(s string) diffStat {
	var d diffStat
	for _, m := range shortstatRE.FindAllStringSubmatch(s, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue // only on overflow; such a count is meaningless anyway
		}
		switch m[2][0] {
		case 'f':
			d.Files = n
		case 'i':
			d.Insertions = n
		case 'd':
			d.Deletions = n
		}
	}
	return d
}

// parseLog parses `git log <logFormat> --shortstat` output. Commits with no
// changes (or merges, which --shortstat leaves empty) get zero stats.
func parseLog(out []byte) ([]commit, error) {
	var commits []commit
	for rec := range bytes.SplitSeq(out, []byte(recordSep)) {
		if len(bytes.TrimSpace(rec)) == 0 {
			continue
		}
		header, rest, _ := bytes.Cut(rec, []byte("\n"))
		f := strings.Split(string(header), fieldSep)
		if len(f) != 3 || !isHash(f[0]) {
			// The header is not echoed: it holds the author's email.
			return nil, fmt.Errorf("unexpected git log record (%d fields)", len(f))
		}
		sec, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad author time %q: %w", f[1], err)
		}
		commits = append(commits, commit{
			Hash:        f[0],
			AuthorTime:  time.Unix(sec, 0).UTC(),
			AuthorEmail: f[2],
			Stat:        parseShortstat(string(rest)),
		})
	}
	return commits, nil
}

// isHash accepts SHA-1 (40) and SHA-256 (64) hex object names.
func isHash(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// sameEmail compares addresses case-insensitively, ignoring surrounding
// space and angle brackets. Empty never matches.
func sameEmail(a, b string) bool {
	norm := func(s string) string { return strings.ToLower(strings.Trim(strings.TrimSpace(s), "<>")) }
	a, b = norm(a), norm(b)
	return a != "" && a == b
}
