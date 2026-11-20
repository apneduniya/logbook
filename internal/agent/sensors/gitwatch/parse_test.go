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
	p1, p2 := strings.Repeat("1", 40), strings.Repeat("2", 40)
	out := recordSep + h1 + fieldSep + "1795000000" + fieldSep + "me@example.com" + fieldSep + p1 + " " + p2 + "\n\n" +
		" 2 files changed, 5 insertions(+), 1 deletion(-)\n" +
		recordSep + h2 + fieldSep + "1794999000" + fieldSep + "Other@Example.com" + fieldSep + "\n" // root, empty: no stat line
	got, err := parseLog([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	want := []commit{
		{h1, time.Unix(1795000000, 0).UTC(), "me@example.com", false, diffStat{2, 5, 1}},
		{h2, time.Unix(1794999000, 0).UTC(), "Other@Example.com", true, diffStat{}},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("parseLog = %+v\nwant %+v", got, want)
	}

	for _, bad := range []string{
		recordSep + "nothex" + fieldSep + "1" + fieldSep + "x" + fieldSep + "\n",
		recordSep + h1 + fieldSep + "notatime" + fieldSep + "x" + fieldSep + "\n",
		recordSep + h1 + fieldSep + "1" + fieldSep + "x\n", // old three-field format
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

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		email   string
		filters []string
		bad     bool
	}{
		{"empty", "", "", nil, false},
		{"email only", "user.email\nme@example.com\x00", "me@example.com", nil, false},
		{"last email wins", "user.email\nold@x.org\x00user.email\nme@x.org\x00", "me@x.org", nil, false},
		{"lfs and a custom filter, values ignored", "user.email\nme@x.org\x00filter.lfs.clean\ngit-lfs clean -- %f\x00" +
			"filter.lfs.process\ngit-lfs filter-process\x00filter.lfs.required\ntrue\x00filter.My.Tool.clean\nevil\nmultiline\x00",
			"me@x.org", []string{"lfs", "My.Tool"}, false},
		{"filter variable without driver", "filter.clean\nx\x00", "", nil, false},
		{"name that can't be overridden", "filter.a=b.clean\nx\x00", "", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseConfig([]byte(tt.in))
			if (err != nil) != tt.bad {
				t.Fatalf("err = %v", err)
			}
			if got.email != tt.email || !slices.Equal(got.filters, tt.filters) {
				t.Fatalf("parseConfig = %+v", got)
			}
		})
	}
	cfg := repoConfigOut{filters: []string{"lfs"}}
	want := []string{"-c", "filter.lfs.clean=", "-c", "filter.lfs.smudge=", "-c", "filter.lfs.process=", "-c", "filter.lfs.required=false"}
	if got := cfg.filterOverrides(); !slices.Equal(got, want) {
		t.Fatalf("filterOverrides = %v", got)
	}
}

func TestSubcommand(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"log", "HEAD"}, "log"},
		{[]string{"-c", "filter.x.clean=", "diff", "--shortstat"}, "diff"},
		{[]string{"--no-pager", "status"}, "status"},
		{[]string{"-c"}, "?"},
	}
	for _, tt := range tests {
		if got := subcommand(tt.args); got != tt.want {
			t.Errorf("subcommand(%v) = %q, want %q", tt.args, got, tt.want)
		}
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
