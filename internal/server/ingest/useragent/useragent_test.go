package useragent

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		ua   string
		want Client
	}{
		{"", Client{}},
		{
			"wakatime/v1.102.1 (linux-6.8.0-49-generic-x86_64) go1.23.4 vscode/1.95.3 vscode-wakatime/24.9.1",
			Client{Editor: "vscode", OS: "linux", Plugin: "vscode-wakatime/24.9.1"},
		},
		{
			"wakatime/v1.102.1 (darwin-23.4.0-arm64) go1.23.4 neovim/0.10.2 vim-wakatime/11.2.0",
			Client{Editor: "neovim", OS: "macos", Plugin: "vim-wakatime/11.2.0"},
		},
		{
			"wakatime/v1.102.1 (windows-10.0.22631-x86_64) go1.23.4 Cursor/0.42.3 vscode-wakatime/24.9.1",
			Client{Editor: "cursor", OS: "windows", Plugin: "vscode-wakatime/24.9.1"},
		},
		{
			"wakatime/v1.102.1 (linux-5.15.153.1-microsoft-standard-wsl2-x86_64) go1.23.4 vscode/1.95.3 vscode-wakatime/24.9.1",
			Client{Editor: "vscode", OS: "wsl", Plugin: "vscode-wakatime/24.9.1"},
		},
		{
			"wakatime/v1.102.1 (darwin-23.4.0-arm64) go1.23.4 PyCharm/2024.2.1 PyCharm-wakatime/15.0.3",
			Client{Editor: "pycharm", OS: "macos", Plugin: "pycharm-wakatime/15.0.3"},
		},
		{
			"wakatime/v1.102.1 (linux-6.8.0-x86_64) go1.23.4 IntelliJ IDEA/2024.2 jetbrains-wakatime/15.0.3",
			Client{Editor: "intellijidea", OS: "linux", Plugin: "jetbrains-wakatime/15.0.3"},
		},
		{
			"wakatime/v1.102.1 (linux-6.8.0-x86_64) go1.23.4 Visual Studio Code - Insiders/1.96.0 vscode-wakatime/24.9.1",
			Client{Editor: "vscode", OS: "linux", Plugin: "vscode-wakatime/24.9.1"},
		},
		{
			"wakatime/v1.102.1 (linux-6.8.0-x86_64) go1.23.4 sublime_text/4180 sublime-wakatime/12.0.0",
			Client{Editor: "sublime_text", OS: "linux", Plugin: "sublime-wakatime/12.0.0"},
		},
		{
			"wakatime/v1.102.1 (linux-6.8.0-x86_64) go1.23.4 claude/1.0.0 claude-code-wakatime/1.2.0",
			Client{Editor: "claude-code", OS: "linux", Plugin: "claude-code-wakatime/1.2.0"},
		},
		{
			"wakatime/v1.102.1 (linux-6.8.0-x86_64) go1.23.4 emacs-wakatime/1.0.2",
			Client{Editor: "emacs", OS: "linux", Plugin: "emacs-wakatime/1.0.2"},
		},
		{
			// wakatime-cli's fallback when the plugin passes nothing.
			"wakatime/v1.102.1 (linux-unknown-unknown) go1.23.4 Unknown/0",
			Client{OS: "linux"},
		},
		{
			"wakatime/v1.102.1 (linux-6.8.0-x86_64) go1.23.4 node/v20.11.0 zed/0.160.0 zed-wakatime/0.1.9",
			Client{Editor: "zed", OS: "linux", Plugin: "zed-wakatime/0.1.9"},
		},
		{
			// wakatime-cli's own test fixture sends a bare CLI version.
			"wakatime/13.0.7",
			Client{},
		},
		{
			"vscode/1.95.3 vscode-wakatime/24.9.1",
			Client{Editor: "vscode", Plugin: "vscode-wakatime/24.9.1"},
		},
		{
			"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/131.0 Safari/537.36",
			Client{},
		},
		{
			"wakatime/v1.0.0 (vscode) go1.22 code-server/4.0",
			Client{Editor: "code-server"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.ua, func(t *testing.T) {
			if got := Parse(tt.ua); got != tt.want {
				t.Fatalf("Parse(%q) = %+v, want %+v", tt.ua, got, tt.want)
			}
		})
	}
}
