package daemon

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/crediting"
	"github.com/apneduniya/logbook/internal/agent/devicekey"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/serverapi"
	"github.com/apneduniya/logbook/internal/agent/upload"
	"github.com/apneduniya/logbook/pkg/struggle"
)

// newCrediting wires the crediting pipeline: the device identity, the
// server client and the Runner, all reading the live config.
func newCrediting(o Options, db *localdb.DB, client *http.Client, set *settings, repos RepoSource) *crediting.Runner {
	api := serverapi.New(client, o.UserAgent)
	name := o.DeviceName
	if name == "" {
		if h, err := os.Hostname(); err == nil {
			name = h
		}
	}
	identity := devicekey.NewIdentity(devicekey.Options{
		KeyPath: config.DeviceKeyPath(o.DataDir), Store: db, Registrar: api,
		Name: name, OS: runtime.GOOS, Logger: o.Logger.With("task", "device"),
	})
	return crediting.New(crediting.Options{
		Store: db, Server: api, Identity: identity,
		Target:      func() crediting.Target { return creditingTarget(set.Get()) },
		ActiveTimes: activeTimes(db),
		Repos:       engineRepos{repos},
		Logger:      o.Logger.With("task", "crediting"),
		Interval:    o.creditingInterval,
	})
}

// activeTimes returns the local database as the active-time source when it
// offers one (Claude Code OpenTelemetry datapoints), else nil: crediting
// works without it.
func activeTimes(db *localdb.DB) crediting.ActiveTimeSource {
	if s, ok := any(db).(crediting.ActiveTimeSource); ok {
		return s
	}
	return nil
}

// creditingTarget is the Logbook server and key from cfg, disabled for the
// same reasons as the heartbeat uploader.
func creditingTarget(cfg config.Config) crediting.Target {
	return crediting.Target{
		Target:   serverapi.Target{BaseURL: strings.TrimRight(cfg.ServerURL, "/"), APIKey: cfg.APIKey},
		Disabled: upload.LogbookTarget(cfg).Disabled,
	}
}

// engineRepos adapts the active-repo list to the engine's Repo type.
type engineRepos struct{ r RepoSource }

func (e engineRepos) ActiveRepos(ctx context.Context) ([]struggle.Repo, error) {
	rs, err := e.r.ActiveRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing active repos: %w", err)
	}
	out := make([]struggle.Repo, len(rs))
	for i, x := range rs {
		out[i] = struggle.Repo{Path: x.Path, Project: x.Project}
	}
	return out, nil
}
