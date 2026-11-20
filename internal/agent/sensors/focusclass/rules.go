package focusclass

// group is one row of the classification table: names that mean kind on os
// (osAny for every OS). Names are matched case-insensitively against the
// normalized process name or macOS bundle id/app name; a trailing "*" makes a
// prefix rule. Exact names always beat prefixes.
type group struct {
	os    string
	kind  Kind
	names []string
}

// rules is the classification table. Sources: each app's installed binary or
// Info.plist (CFBundleIdentifier), cross-checked against public app-matching
// lists (e.g. Talon's community app definitions). Windows names drop ".exe";
// Linux names are /proc/<pid>/comm values.
//
// Order matters only for duplicates: the first group that lists a name wins.
func rules() []group {
	return []group{
		// Exceptions that must win over the prefix rules below.
		{osDarwin, Other, []string{"com.jetbrains.toolbox"}},

		// ---- Editors and IDEs ----
		{osAny, Editor, []string{
			// VS Code family.
			"code", "code - insiders", "code-insiders", "code-oss", "vscodium", "codium",
			"cursor", "windsurf", "trae", "kiro", "void", "positron",
			// JetBrains (Windows *64.exe and Linux native launchers).
			"idea", "idea64", "pycharm", "pycharm64", "webstorm", "webstorm64", "goland", "goland64",
			"clion", "clion64", "rider", "rider64", "phpstorm", "phpstorm64", "rubymine", "rubymine64",
			"datagrip", "datagrip64", "rustrover", "rustrover64", "dataspell", "dataspell64",
			"fleet", "android studio", "studio64",
			// Other editors.
			"zed", "zed-editor", "zeditor", "sublime_text", "sublime text",
			"gvim", "macvim", "nvim-qt", "neovide", "goneovim", "emacs", "lapce",
			"kate", "kwrite", "gedit", "gnome-text-editor", "geany", "eclipse",
			"notepad++", "devenv", "rstudio", "spyder", "thonny", "bluej", "greenfoot",
			"arduino ide", "arduino-ide", "kicad", "pcbnew", "eeschema",
			"godot", "unity", "processing",
		}},
		{osLinux, Editor, []string{"studio"}}, // Android Studio's Linux launcher
		{osDarwin, Editor, []string{
			// Bundle identifiers.
			"com.microsoft.vscode", "com.microsoft.vscodeinsiders", "com.visualstudio.code.oss",
			"com.vscodium", "com.todesktop.230313mzl4w4u92", "com.anysphere.cursor",
			"com.exafunction.windsurf", "dev.zed.zed", "dev.zed.zed-preview",
			"com.sublimetext.4", "com.sublimetext.3", "org.vim.macvim", "com.neovide.neovide",
			"org.gnu.emacs", "com.apple.dt.xcode", "com.google.android.studio", "cc.arduino.ide2",
			"org.kicad-pcb.kicad", "com.panic.nova", "com.barebones.bbedit",
			"com.macromates.textmate", "com.coteditor.coteditor", "org.godotengine.godot",
			"com.jetbrains.*",
			// Localized app names, used when the bundle id is unavailable.
			"visual studio code", "xcode", "intellij idea", "intellij idea ce", "pycharm ce",
			"nova", "bbedit", "textmate", "coteditor",
		}},

		// ---- Terminals ----
		{osAny, Terminal, []string{
			"wezterm", "wezterm-gui", "alacritty", "kitty", "ghostty", "hyper", "tabby",
			"warp", "rio", "contour", "iterm2",
		}},
		{osWindows, Terminal, []string{
			"windowsterminal", "wt", "openconsole", "conhost", "cmd", "powershell",
			"powershell_ise", "pwsh", "mintty", "conemu", "conemu64", "cmder", "wsl",
		}},
		{osLinux, Terminal, []string{
			"gnome-terminal", "gnome-terminal-server", "konsole", "xterm", "uxterm", "urxvt",
			"rxvt", "tilix", "terminator", "xfce4-terminal", "mate-terminal", "lxterminal",
			"qterminal", "kgx", "ptyxis", "foot", "footclient", "st", "terminology",
			"guake", "tilda", "yakuake", "cool-retro-term", "deepin-terminal", "warp-terminal",
		}},
		{osDarwin, Terminal, []string{
			"com.apple.terminal", "com.googlecode.iterm2", "com.mitchellh.ghostty", "org.alacritty",
			"net.kovidgoyal.kitty", "com.github.wez.wezterm", "dev.warp.warp-stable",
			"co.zeit.hyper", "org.tabby", "com.raphaelamorim.rio",
			"terminal",
		}},

		// ---- Browsers ----
		{osAny, Browser, []string{
			"chrome", "google chrome", "msedge", "microsoft edge", "firefox", "firefox-bin",
			"firefox-esr", "brave", "brave-browser", "brave browser", "opera", "vivaldi",
			"vivaldi-bin", "chromium", "chromium-browser", "arc", "zen", "zen-bin", "zen browser",
			"librewolf", "waterfox", "floorp", "thorium", "dia", "comet", "orion",
		}},
		{osWindows, Browser, []string{"iexplore"}},
		{osLinux, Browser, []string{"epiphany", "falkon", "google-chrome", "microsoft-edge"}},
		{osDarwin, Browser, []string{
			"com.apple.safari", "com.apple.safaritechnologypreview", "com.google.chrome",
			"com.google.chrome.beta", "com.google.chrome.canary", "org.mozilla.firefox",
			"org.mozilla.firefoxdeveloperedition", "org.mozilla.nightly", "com.microsoft.edgemac",
			"com.brave.browser", "company.thebrowser.browser", "company.thebrowser.dia",
			"com.operasoftware.opera", "com.vivaldi.vivaldi", "org.chromium.chromium",
			"app.zen-browser.zen", "io.gitlab.librewolf-community", "com.kagi.kagimacos",
			"ai.perplexity.comet", "com.duckduckgo.macos.browser",
			"safari",
		}},

		// ---- AI desktop apps ----
		{osAny, AI, []string{
			"claude", "chatgpt", "chatgpt classic", "codex", "perplexity", "lm studio", "jan", "msty",
		}},
		{osDarwin, AI, []string{
			"com.anthropic.claudefordesktop", "com.openai.chat", "com.openai.codex",
			"ai.perplexity.mac", "ai.elementlabs.lmstudio",
		}},
	}
}
