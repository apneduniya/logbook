package doctor

import (
	"bufio"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxCfgBytes bounds how much of .wakatime.cfg is read.
const maxCfgBytes = 1 << 20

// WakatimeCfgPath returns where wakatime-cli reads its config:
// $WAKATIME_HOME/.wakatime.cfg, else ~/.wakatime.cfg.
func WakatimeCfgPath() (string, error) {
	if h := os.Getenv("WAKATIME_HOME"); h != "" {
		return filepath.Join(h, ".wakatime.cfg"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding home dir: %w", err)
	}
	return filepath.Join(home, ".wakatime.cfg"), nil
}

// ReadWakatimeCfg reads api_url from the [settings] section of the config
// at path, and compares its api_key with wantKey without returning it.
func ReadWakatimeCfg(path, wantKey string) WakatimeCfg {
	w := WakatimeCfg{Path: path}
	f, err := os.Open(path) //nolint:gosec // the user's own wakatime config
	if err != nil {
		w.Err = err
		return w
	}
	defer func() { _ = f.Close() }()
	settings, err := parseSettings(io.LimitReader(f, maxCfgBytes))
	if err != nil {
		w.Err = err
		return w
	}
	w.APIURL = settings["api_url"]
	key := settings["api_key"]
	w.KeyMatches = wantKey != "" && subtle.ConstantTimeCompare([]byte(key), []byte(wantKey)) == 1
	return w
}

// parseSettings returns the keys of the INI [settings] section. It follows
// wakatime-cli's format: "key = value" lines, # and ; comments.
func parseSettings(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	section := ""
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxCfgBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
			continue
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		if section != "settings" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, errors.New("a line is too long")
		}
		return nil, fmt.Errorf("reading wakatime config: %w", err)
	}
	return out, nil
}
