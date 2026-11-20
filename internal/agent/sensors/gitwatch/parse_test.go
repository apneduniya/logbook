package gitwatch

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseShortstat(t *testing.T) {
	tests := []struct {
		in   string
		want diffStat
	}{
		{"", diffStat{}},
		{" 1 file changed, 1 insertion(+)\n", diffStat{1, 1, 0}},
		{" 1 file changed, 2 deletions(-)", diffStat{1, 0, 2}},
		{" 3 files changed, 10 insertions(+), 2 deletions(-)", diffStat{3, 10, 2}},
		{" 2 files changed, 0 insertions(+), 0 deletions(-)", diffStat{2, 0, 0}}, // binary or mode-only
		{" 1 file changed, 1 insertion(+), 1 deletion(-)", diffStat{1, 1, 1}},
	}
	for _, tt := range tests {
		if got := parseShortstat(tt.in); got != tt.want {
			t.Errorf("parseShortstat(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestParseLog(t *testing.T) {
	h1 := strings.Repeat("a", 40)
	h2 := strings.Repeat("b", 64) // SHA-256 repository
	out := recordSep + h1 + fieldSep + "1795000000" + fieldSep + "me@example.com\n\n" +
		" 2 files changed, 5 insertions(+), 1 deletion(-)\n" +
		recordSep + h2 + fieldSep + "1794999000" + fieldSep + "Other@Example.com\n" // empty commit: no stat line
	got, err := parseLog([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	want := []commit{
		{h1, time.Unix(1795000000, 0).UTC(), "me@example.com", diffStat{2, 5, 1}},
		{h2, time.Unix(1794999000, 0).UTC(), "Other@Example.com", diffStat{}},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("parseLog = %+v\nwant %+v", got, want)
	}

	for _, bad := range []string{
		recordSep + "nothex" + fieldSep + "1" + fieldSep + "x\n",
		recordSep + h1 + fieldSep + "notatime" + fieldSep + "x\n",
		recordSep + h1 + fieldSep + "1\n",
	} {
		if _, err := parseLog([]byte(bad)); err == nil {
			t.Errorf("parseLog(%q) = nil error", bad)
		}
	}
	if got, err := parseLog(nil); err != nil || len(got) != 0 {
		t.Errorf("parseLog(empty) = %v, %v", got, err)
	}
}

func TestSameEmail(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"me@example.com", "ME@Example.com", true},
		{" <me@example.com> ", "me@example.com", true},
		{"me@example.com", "you@example.com", false},
		{"", "", false},
	}
	for _, tt := range tests {
		if got := sameEmail(tt.a, tt.b); got != tt.want {
			t.Errorf("sameEmail(%q, %q) = %v", tt.a, tt.b, got)
		}
	}
}

func TestGitEnv(t *testing.T) {
	env := gitEnv([]string{
		"PATH=/usr/bin", "HOME=/home/u", "GIT_DIR=/elsewhere", "LC_ALL=fr_FR.UTF-8",
		"GIT_EXTERNAL_DIFF=evil", "GIT_SSH_COMMAND=evil", "GIT_OPTIONAL_LOCKS=1",
	})
	has := func(kv string) bool { return slices.Contains(env, kv) }
	for _, want := range []string{"PATH=/usr/bin", "HOME=/home/u", "LC_ALL=C", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0"} {
		if !has(want) {
			t.Errorf("env missing %q: %v", want, env)
		}
	}
	for _, bad := range []string{"GIT_DIR=/elsewhere", "LC_ALL=fr_FR.UTF-8", "GIT_EXTERNAL_DIFF=evil", "GIT_SSH_COMMAND=evil", "GIT_OPTIONAL_LOCKS=1"} {
		if has(bad) {
			t.Errorf("env kept %q", bad)
		}
	}
}

func TestCapBuffer(t *testing.T) {
	b := &capBuffer{max: 4}
	if n, err := b.Write([]byte("abcdef")); n != 6 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if b.String() != "abcd" || !b.overflow {
		t.Fatalf("buffer = %q overflow=%v", b.String(), b.overflow)
	}
}
