package useragent

import (
	"regexp"
	"strings"
)

// Client is what a User-Agent says about the sending client. Empty fields
// mean the User-Agent didn't say.
type Client struct {
	// Editor is a lowercase editor name such as "vscode" or "neovim".
	Editor string
	// OS is a lowercase operating system name such as "linux" or "macos".
	OS string
	// Plugin is the editor plugin token, such as "vscode-wakatime/24.6.0".
	Plugin string
}

var (
	cliPattern     = regexp.MustCompile(`(?i)^wakatime/\S*\s+\(([^)]*)\)\s*(.*)$`)
	pluginPattern  = regexp.MustCompile(`(?i)[-_](?:waka|hacka)time$`)
	runtimePattern = regexp.MustCompile(`(?i)^(?:node/v?\d|python/v?\d{1,2}(?:\.|$))`)
	bareRuntime    = regexp.MustCompile(`(?i)^(?:go|java|node|python)(?:\d|$)`)
	decorator      = regexp.MustCompile(`(?i)^(?:v?\d+(?:[._-]\d+)*[a-z]?|[+_.\\-])$`)
	osSplit        = regexp.MustCompile(`[\s)_-]`)
)

// Multi-word product names, rewritten before the chain is split on spaces.
// Longer names come first so "visual studio code - insiders" wins over
// "visual studio code".
var productAliases = []struct{ from, to string }{
	{"visual studio code - insiders", "vscode"},
	{"visual studio code", "vscode"},
	{"code - oss", "code-oss"},
	{"intellij idea", "intellijidea"},
	{"vs code", "vscode"},
}

// Products that wrap the real client and never name the editor.
var middlewares = map[string]bool{
	"wakatime": true, "wakatime-cli": true, "exec-wakatime": true, "mcp-wakatime": true,
	"vscode-hackatime-tracker": true, "waka-relay": true, "wakatime-ls": true,
	"in": true, "x": true, "unknown": true,
}

// Plugin names that don't follow the "<editor>-wakatime" pattern.
var pluginEditorAliases = map[string]string{
	"python-wakatime":       "pycharm",
	"wakatime-zsh-plugin":   "zsh",
	"oh-my-pi-wakatime":     "pi",
	"wakatime.nvim":         "neovim",
	"jetbrains-wakatime":    "",
	"visualstudio-wakatime": "visualstudio",
}

var osProducts = map[string]bool{
	"linux": true, "windows": true, "macos": true, "darwin": true, "win": true, "mac": true, "wsl": true,
}

// Parse extracts what it can from a User-Agent header or heartbeat field.
func Parse(ua string) Client {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return Client{}
	}
	if m := cliPattern.FindStringSubmatch(ua); m != nil {
		editor, plugin := fromProducts(m[2])
		c := Client{Editor: editor, OS: normalizeOS(m[1], ua), Plugin: plugin}
		if pe := editorFromPlatform(m[1]); pe != "" {
			// Some plugins put their own name where the OS belongs.
			c.OS = ""
			if c.Editor == "" {
				c.Editor = pe
			}
		}
		return c
	}
	if strings.HasPrefix(strings.ToLower(ua), "mozilla/") {
		// Browser User-Agents name the browser, not an editor.
		return Client{}
	}
	// A bare plugin chain sent without wakatime-cli, e.g. "vscode/1.95.0 vscode-wakatime/24.6.0".
	editor, plugin := fromProducts(ua)
	return Client{Editor: editor, Plugin: plugin}
}

// fromProducts finds the editor and plugin in a space-separated product chain.
func fromProducts(chain string) (editor, plugin string) {
	chain = strings.ToLower(strings.Trim(strings.TrimSpace(chain), `"\`))
	for _, a := range productAliases {
		chain = strings.ReplaceAll(chain, a.from, a.to)
	}
	tokens := strings.Fields(chain)

	for _, tok := range tokens {
		if isPlugin(product(tok)) {
			plugin = tok
			break
		}
	}
	for _, tok := range tokens {
		p := product(tok)
		switch {
		case p == "" || strings.HasPrefix(p, "("), middlewares[p], isRuntime(tok), decorator.MatchString(p):
			continue
		case isPlugin(p):
			base := pluginBase(p)
			if base == "" || osProducts[base] {
				continue
			}
			return normalizeEditor(base), plugin
		default:
			return normalizeEditor(p), plugin
		}
	}
	return "", plugin
}

// product returns the name part of a "name/version" token.
func product(tok string) string {
	name, _, _ := strings.Cut(tok, "/")
	return name
}

func isPlugin(p string) bool {
	_, aliased := pluginEditorAliases[p]
	return aliased || pluginPattern.MatchString(p)
}

func pluginBase(p string) string {
	if e, ok := pluginEditorAliases[p]; ok {
		return e
	}
	return pluginPattern.ReplaceAllString(p, "")
}

func isRuntime(tok string) bool {
	if runtimePattern.MatchString(tok) {
		return true
	}
	return !strings.Contains(tok, "/") && bareRuntime.MatchString(tok)
}

func normalizeEditor(e string) string {
	switch e {
	case "claude", "claudecode":
		return "claude-code"
	case "github-copilot-cli":
		return "copilot-cli"
	case "idea", "intellijideacommunityedition", "intellijideaultimateedition":
		return "intellijidea"
	case "ktexteditor":
		return "kate"
	case "vs-code", "vs_code", "vscode-insiders":
		return "vscode"
	case "godotengine":
		return "godot"
	}
	return e
}

func editorFromPlatform(platform string) string {
	p := strings.ToLower(strings.TrimSpace(platform))
	switch {
	case p == "vscode" || p == "vs code" || strings.HasPrefix(p, "vscode/") || strings.HasPrefix(p, "vscode,"):
		return "vscode"
	case p == "termux-wakatime":
		return "termux"
	case p == "webstorm":
		return "webstorm"
	}
	return ""
}

func normalizeOS(platform, ua string) string {
	p := strings.ToLower(strings.TrimSpace(platform))
	switch {
	case strings.Contains(strings.ToLower(ua), "-wsl2-"):
		return "wsl"
	case strings.Contains(p, "android"):
		return "android"
	case p == "chrome os":
		return "chromeos"
	case p == "win32":
		return "windows"
	case strings.HasPrefix(p, "iphone"), strings.HasPrefix(p, "ipad"):
		return "ios"
	}
	switch first := osSplit.Split(p, 2)[0]; first {
	case "win", "windows":
		return "windows"
	case "darwin", "mac", "macos", "macintosh;", "macintosh":
		return "macos"
	case "none", "unknown":
		return ""
	default:
		return first
	}
}
