package cmdline

import (
	"reflect"
	"testing"
)

func TestQuote(t *testing.T) {
	tests := []struct {
		name string
		sh   Shell
		bin  string
		want string
	}{
		{"posix plain", POSIX, "/usr/local/bin/logbook", "/usr/local/bin/logbook hook codex Stop"},
		{"posix space", POSIX, "/Users/Teen Dev/bin/logbook", "'/Users/Teen Dev/bin/logbook' hook codex Stop"},
		{"posix quote", POSIX, "/home/o'brien/logbook", `'/home/o'\''brien/logbook' hook codex Stop`},
		{"posix windows path", POSIX, `C:\Logbook\logbook.exe`, `'C:\Logbook\logbook.exe' hook codex Stop`},
		{"cmd plain", Cmd, `C:\Logbook\logbook.exe`, `C:\Logbook\logbook.exe hook codex Stop`},
		{"cmd space", Cmd, `C:\Users\Teen Dev\logbook.exe`, `"C:\Users\Teen Dev\logbook.exe" hook codex Stop`},
		{"cmd ampersand", Cmd, `C:\R&D\logbook.exe`, `"C:\R&D\logbook.exe" hook codex Stop`},
		{"pwsh plain", PowerShell, `C:\Logbook\logbook.exe`, `C:\Logbook\logbook.exe hook codex Stop`},
		{"pwsh space", PowerShell, `C:\Users\Teen Dev\logbook.exe`, `& 'C:\Users\Teen Dev\logbook.exe' hook codex Stop`},
		{"pwsh quote", PowerShell, `C:\Users\O'Brien\logbook.exe`, `& 'C:\Users\O''Brien\logbook.exe' hook codex Stop`},
		{"pwsh dollar", PowerShell, `C:\$work\logbook.exe`, `& 'C:\$work\logbook.exe' hook codex Stop`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			argv := Hook(tc.bin, "codex", "Stop")
			got := Quote(tc.sh, argv)
			if got != tc.want {
				t.Fatalf("Quote = %s\n want %s", got, tc.want)
			}
			if back := Split(got); !reflect.DeepEqual(back, argv) {
				t.Fatalf("Split(Quote) = %q, want %q", back, argv)
			}
			if !IsHook(Split(got), "codex") {
				t.Fatal("quoted command not recognised as a Logbook hook")
			}
		})
	}
}

func TestIsHook(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"/opt/logbook hook codex Stop", true},
		{`C:\x\Logbook.EXE hook codex Stop`, true},
		{"/opt/logbook-dev hook codex Stop", true},
		{"/opt/logbook hook claude-code Stop", false},
		{"/opt/logbook hook codex", false},
		{"/opt/logbook hook codex Stop --extra", false},
		{"/opt/logbook run", false},
		{"python3 logbook.py hook codex Stop", false},
		{"/usr/bin/notify-send hook codex Stop", false},
		{"/opt/logbook hook codex Stop && rm -rf ~", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := IsHook(Split(tc.cmd), "codex"); got != tc.want {
			t.Errorf("IsHook(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

func TestSplit(t *testing.T) {
	tests := map[string][]string{
		`a  b	c`:             {"a", "b", "c"},
		`"a b" c`:            {"a b", "c"},
		`'it''s'`:            {"it's"},
		`'it'\''s'`:          {"it's"},
		`& 'x y' z`:          {"x y", "z"},
		`a""b`:               {"ab"},
		`''`:                 {""},
		`echo "unterminated`: {"echo", "unterminated"},
	}
	for in, want := range tests {
		if got := Split(in); !reflect.DeepEqual(got, want) {
			t.Errorf("Split(%q) = %q, want %q", in, got, want)
		}
	}
}
