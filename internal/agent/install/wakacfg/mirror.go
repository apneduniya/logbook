package wakacfg

import (
	"net/url"
	"strings"

	"github.com/apneduniya/logbook/internal/agent/config"
)

// DefaultWakaTimeURL is the api_url wakatime-cli uses when the file sets none.
const DefaultWakaTimeURL = "https://api.wakatime.com/api/v1"

// MirrorOptions are the user's choices that affect mirroring.
type MirrorOptions struct {
	// ServerURL is the Logbook server; a file already pointing there is
	// never mirrored, since that would count the same time twice.
	ServerURL string
	// LocalURL is the daemon's api_url; same rule.
	LocalURL string
	// OverrideURL replaces the original api_url as the mirror target
	// (--mirror-url). The original api_key is still used.
	OverrideURL string
	// Disabled turns mirroring off (--no-mirror). The original URL and key
	// are still saved, so mirroring can be turned on later.
	Disabled bool
}

// MirrorTarget decides where heartbeats are mirrored, from what the user's
// .wakatime.cfg pointed at before Logbook.
//
// The rule is "keep sending to whatever they had", so nothing the user
// relies on breaks: Hackatime users keep their Hackatime hours, WakaTime
// users keep their WakaTime dashboard, and self-hosted (wakapi) users keep
// theirs. A file with no api_url used WakaTime's default, so that is the
// target. Mirroring needs the original api_key; without one there is
// nothing to mirror with.
func MirrorTarget(orig Original, o MirrorOptions) config.Hackatime {
	target := orig.URL
	if !orig.HadURL || target == "" {
		target = DefaultWakaTimeURL
	}
	if o.OverrideURL != "" {
		target = o.OverrideURL
	}
	target = strings.TrimRight(target, "/")
	// The retired origin was Logbook's own server before ADR 0011: mirroring
	// there would count the time twice, and it no longer takes uploads.
	if sameHost(target, o.ServerURL) || sameHost(target, o.LocalURL) || config.IsRetired(target) {
		return config.Hackatime{}
	}
	if orig.APIKey == "" {
		return config.Hackatime{URL: target}
	}
	return config.Hackatime{URL: target, APIKey: orig.APIKey, Mirror: !o.Disabled}
}

// IsHackatime reports whether u is a Hackatime api_url.
func IsHackatime(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	return strings.EqualFold(p.Hostname(), "hackatime.hackclub.com") ||
		strings.Contains(p.Path, "/api/hackatime/")
}

func sameHost(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	pa, errA := url.Parse(a)
	pb, errB := url.Parse(b)
	if errA != nil || errB != nil || pa.Host == "" {
		return false
	}
	return strings.EqualFold(pa.Host, pb.Host)
}
