package wakacfg

import (
	"testing"

	"github.com/apneduniya/logbook/internal/agent/config"
)

func TestMirrorTarget(t *testing.T) {
	const (
		hackatime = "https://hackatime.hackclub.com/api/hackatime/v1"
		key       = "11111111-1111-4111-8111-111111111111"
		server    = "https://api.logbook.boogles.dev"
		retired   = "https://logbook.boogles.dev/api/v1"
		local     = "http://127.0.0.1:47231/api/v1"
	)
	base := MirrorOptions{ServerURL: server, LocalURL: local}
	tests := []struct {
		name string
		orig Original
		opts MirrorOptions
		want config.Hackatime
	}{
		{"hackatime", Original{URL: hackatime, HadURL: true, APIKey: key, HadKey: true}, base,
			config.Hackatime{URL: hackatime, APIKey: key, Mirror: true}},
		{"trailing slash trimmed", Original{URL: hackatime + "/", HadURL: true, APIKey: key, HadKey: true}, base,
			config.Hackatime{URL: hackatime, APIKey: key, Mirror: true}},
		{"wakatime default when no url", Original{APIKey: key, HadKey: true}, base,
			config.Hackatime{URL: DefaultWakaTimeURL, APIKey: key, Mirror: true}},
		{"self-hosted kept", Original{URL: "https://wakapi.example/api", HadURL: true, APIKey: key, HadKey: true}, base,
			config.Hackatime{URL: "https://wakapi.example/api", APIKey: key, Mirror: true}},
		{"no-mirror keeps values", Original{URL: hackatime, HadURL: true, APIKey: key, HadKey: true},
			MirrorOptions{ServerURL: server, LocalURL: local, Disabled: true},
			config.Hackatime{URL: hackatime, APIKey: key}},
		{"override url", Original{APIKey: key, HadKey: true},
			MirrorOptions{ServerURL: server, LocalURL: local, OverrideURL: hackatime},
			config.Hackatime{URL: hackatime, APIKey: key, Mirror: true}},
		{"no key, no mirror", Original{}, base, config.Hackatime{URL: DefaultWakaTimeURL}},
		{"already logbook server", Original{URL: server + "/api/v1", HadURL: true, APIKey: key, HadKey: true}, base, config.Hackatime{}},
		{"retired logbook origin", Original{URL: retired, HadURL: true, APIKey: key, HadKey: true}, base, config.Hackatime{}},
		{"already local daemon", Original{URL: local, HadURL: true, APIKey: key, HadKey: true}, base, config.Hackatime{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MirrorTarget(tt.orig, tt.opts); got != tt.want {
				t.Fatalf("MirrorTarget() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestIsHackatime(t *testing.T) {
	tests := map[string]bool{
		"https://hackatime.hackclub.com/api/hackatime/v1": true,
		"https://HACKATIME.hackclub.com/api/v1":           true,
		"https://self.example/api/hackatime/v1":           true,
		"https://api.wakatime.com/api/v1":                 false,
		"::bad":                                           false,
	}
	for in, want := range tests {
		if got := IsHackatime(in); got != want {
			t.Errorf("IsHackatime(%q) = %v, want %v", in, got, want)
		}
	}
}
