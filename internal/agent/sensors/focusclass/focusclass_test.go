package focusclass

import (
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		goos string
		app  string
		want Kind
	}{
		// Windows: case-insensitive, ".exe" and paths stripped.
		{"windows", "Code.exe", Editor},
		{"windows", `C:\Users\a\AppData\Local\Programs\cursor\Cursor.exe`, Editor},
		{"windows", "Windsurf.exe", Editor},
		{"windows", "idea64.exe", Editor},
		{"windows", "studio64.exe", Editor},
		{"windows", "Arduino IDE.exe", Editor},
		{"windows", "devenv.exe", Editor},
		{"windows", "WindowsTerminal.exe", Terminal},
		{"windows", "cmd.exe", Terminal},
		{"windows", "pwsh.exe", Terminal},
		{"windows", "mintty.exe", Terminal},
		{"windows", "chrome.exe", Browser},
		{"windows", "msedge.exe", Browser},
		{"windows", "Claude.exe", AI},
		{"windows", "ChatGPT.exe", AI},
		{"windows", "explorer.exe", Other},
		{"windows", "Discord.exe", Other},
		{"windows", "", Unknown},
		{"windows", "   ", Unknown},
		{"windows", "st.exe", Other}, // "st" is a terminal only on Linux

		// macOS: bundle ids and localized names.
		{"darwin", "com.microsoft.VSCode", Editor},
		{"darwin", "com.todesktop.230313mzl4w4u92", Editor},
		{"darwin", "com.exafunction.windsurf", Editor},
		{"darwin", "com.jetbrains.goland", Editor},
		{"darwin", "com.jetbrains.intellij.ce", Editor},
		{"darwin", "com.jetbrains.toolbox", Other},
		{"darwin", "com.apple.dt.Xcode", Editor},
		{"darwin", "dev.zed.Zed", Editor},
		{"darwin", "com.googlecode.iterm2", Terminal},
		{"darwin", "com.mitchellh.ghostty", Terminal},
		{"darwin", "com.apple.Terminal", Terminal},
		{"darwin", "Terminal", Terminal},
		{"darwin", "com.apple.Safari", Browser},
		{"darwin", "company.thebrowser.Browser", Browser},
		{"darwin", "com.anthropic.claudefordesktop", AI},
		{"darwin", "com.openai.codex", AI},
		{"darwin", "com.openai.chat", AI},
		{"darwin", "com.apple.finder", Other},
		{"darwin", "Visual Studio Code", Editor},

		// Linux: /proc/<pid>/comm names, including 15-byte truncation.
		{"linux", "code", Editor},
		{"linux", "cursor", Editor},
		{"linux", "idea", Editor},
		{"linux", "studio", Editor},
		{"linux", "gnome-terminal-", Terminal},
		{"linux", "gnome-text-edit", Editor},
		{"linux", "chromium-browse", Browser},
		{"linux", "konsole", Terminal},
		{"linux", "kitty", Terminal},
		{"linux", "st", Terminal},
		{"linux", "firefox-bin", Browser},
		{"linux", "nautilus", Other},
		{"linux", "cmd", Other}, // "cmd" is a terminal only on Windows
		{"linux", "com.jetbrains.goland", Other},
	}
	classifiers := map[string]*Classifier{}
	for _, tt := range tests {
		t.Run(tt.goos+"/"+tt.app, func(t *testing.T) {
			c, ok := classifiers[tt.goos]
			if !ok {
				c = New(tt.goos)
				classifiers[tt.goos] = c
			}
			if got := c.Classify(tt.app); got != tt.want {
				t.Fatalf("Classify(%q) on %s = %q, want %q", tt.app, tt.goos, got, tt.want)
			}
		})
	}
}

// TestRulesWellFormed guards the table against typos that would silently
// never match.
func TestRulesWellFormed(t *testing.T) {
	valid := map[Kind]bool{Editor: true, Terminal: true, Browser: true, AI: true, Other: true}
	seen := map[string]Kind{}
	for _, g := range rules() {
		if !valid[g.kind] {
			t.Errorf("group has invalid kind %q", g.kind)
		}
		switch g.os {
		case osAny, osWindows, osDarwin, osLinux:
		default:
			t.Errorf("group has invalid os %q", g.os)
		}
		for _, n := range g.names {
			if n == "" || n != strings.TrimSpace(n) || strings.HasSuffix(n, ".exe") {
				t.Errorf("bad rule name %q", n)
			}
			key := g.os + "|" + strings.ToLower(n)
			if k, dup := seen[key]; dup && k != g.kind {
				t.Errorf("rule %q listed as both %q and %q", key, k, g.kind)
			}
			seen[key] = g.kind
		}
	}
}

func TestKindsMatchWireValues(t *testing.T) {
	for _, k := range []Kind{Editor, Terminal, Browser, AI, Other, Unknown} {
		s := wire.ActivitySample{Time: time.Unix(1_800_000_000, 0), FocusKind: string(k)}
		if err := s.Validate(); err != nil {
			t.Fatalf("kind %q rejected by wire: %v", k, err)
		}
	}
}
