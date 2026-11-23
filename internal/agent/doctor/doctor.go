package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/apneduniya/logbook/internal/agent/account"
	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/status"
	"github.com/apneduniya/logbook/internal/agent/upload"
)

// Level is a finding's severity.
type Level string

// Severities.
const (
	OK   Level = "ok"
	Warn Level = "warn"
	Fail Level = "fail"
)

// Thresholds for warnings.
const (
	MaxClockSkew   = 2 * time.Minute
	StaleQueue     = time.Hour
	SpoolWarnBytes = 1 << 20
)

// Finding is one check's result.
type Finding struct {
	Check  string `json:"check"`
	Level  Level  `json:"level"`
	Detail string `json:"detail"`
	// Fix tells the user what to do; empty when Level is OK.
	Fix string `json:"fix,omitempty"`
}

// WakatimeCfg is what was read from ~/.wakatime.cfg.
type WakatimeCfg struct {
	Path   string
	APIURL string
	// KeyMatches is whether api_key equals the agent's Logbook key.
	KeyMatches bool
	// Err is set if the file couldn't be read (fs.ErrNotExist if missing).
	Err error
}

// Facts is everything Diagnose looks at.
type Facts struct {
	Now       time.Time
	Config    config.Config
	ConfigErr error
	// VerifyErr is the result of checking the key against the server.
	VerifyErr  error
	ServerTime time.Time
	// DaemonErr is set when the daemon didn't answer.
	DaemonErr error
	// Status comes from the daemon, or the database when it's down.
	Status    status.Snapshot
	StatusErr error
	Wakatime  WakatimeCfg
}

// Diagnose returns one finding per check, in a stable order.
func Diagnose(f Facts) []Finding {
	if f.ConfigErr != nil {
		fix := "run `logbook install`, or `logbook login --key <key>`"
		if !errors.Is(f.ConfigErr, config.ErrNotConfigured) {
			fix = "fix or delete config.json in the logbook data folder, then run `logbook login --key <key>`"
		}
		return []Finding{{Check: "config", Level: Fail, Detail: f.ConfigErr.Error(), Fix: fix}}
	}
	out := []Finding{{Check: "config", Level: OK, Detail: "server " + f.Config.ServerURL}}
	out = append(out, keyFinding(f), clockFinding(f), daemonFinding(f), wakatimeFinding(f))
	out = append(out, queueFindings(f)...)
	return out
}

func keyFinding(f Facts) Finding {
	switch {
	case f.Config.APIKey == "":
		return Finding{Check: "api key", Level: Fail, Detail: "not signed in", Fix: "run `logbook login --key <key>`"}
	case errors.Is(f.VerifyErr, account.ErrKeyRejected):
		return Finding{Check: "api key", Level: Fail, Detail: "the server rejected the saved key",
			Fix: "create a new key on the Logbook dashboard and run `logbook login --key <key>`"}
	case errors.Is(f.VerifyErr, account.ErrNotAPI):
		return Finding{Check: "api key", Level: Fail, Detail: "the server address isn't the Logbook API: " + f.VerifyErr.Error(),
			Fix: "run `logbook login --server <API address> --key <key>` with the API's address, not the website's"}
	case f.VerifyErr != nil:
		return Finding{Check: "api key", Level: Warn, Detail: "could not check the key: " + f.VerifyErr.Error(),
			Fix: "check your internet connection or proxy (HTTPS_PROXY); heartbeats wait in the queue meanwhile"}
	}
	return Finding{Check: "api key", Level: OK, Detail: "accepted by the server"}
}

func clockFinding(f Facts) Finding {
	if f.ServerTime.IsZero() {
		return Finding{Check: "clock", Level: OK, Detail: "not checked (needs a server answer)"}
	}
	skew := f.Now.Sub(f.ServerTime)
	if skew < 0 {
		skew = -skew
	}
	if skew > MaxClockSkew {
		return Finding{Check: "clock", Level: Warn, Detail: fmt.Sprintf("this computer's clock is off by %s", skew.Round(time.Second)),
			Fix: "turn on automatic date and time in your system settings; heartbeats with wrong times can be rejected"}
	}
	return Finding{Check: "clock", Level: OK, Detail: "in sync with the server"}
}

func daemonFinding(f Facts) Finding {
	if f.DaemonErr != nil {
		return Finding{Check: "agent", Level: Fail, Detail: "not running at " + f.Config.LocalAddr,
			Fix: "start it with `logbook run` (or `logbook install` to start it at login); editors keep heartbeats offline meanwhile"}
	}
	return Finding{Check: "agent", Level: OK, Detail: "running at " + f.Config.LocalAddr}
}

func wakatimeFinding(f Facts) Finding {
	w := f.Wakatime
	switch {
	case w.Err != nil:
		cause := w.Err
		var pe *fs.PathError
		if errors.As(cause, &pe) {
			cause = pe.Err // the path is already in the message
		}
		return Finding{Check: "wakatime.cfg", Level: Fail, Detail: "can't read " + w.Path + ": " + cause.Error(),
			Fix: "run `logbook install` to point editor plugins at the agent"}
	case !PointsAtAgent(w.APIURL, f.Config.LocalAddr):
		detail := "api_url is not set"
		if w.APIURL != "" {
			detail = "api_url is " + redactURL(w.APIURL) + ", not the agent's API"
		}
		return Finding{Check: "wakatime.cfg", Level: Fail, Detail: detail,
			Fix: "run `logbook install` so editor heartbeats go through the agent (api_url = " +
				AgentAPIURL(f.Config.LocalAddr) + ")"}
	case !w.KeyMatches:
		return Finding{Check: "wakatime.cfg", Level: Fail, Detail: "api_key differs from the agent's Logbook key",
			Fix: "run `logbook install` again after `logbook login`"}
	}
	return Finding{Check: "wakatime.cfg", Level: OK, Detail: "editors send heartbeats to the agent"}
}

// PointsAtAgent reports whether apiURL targets the agent's loopback address
// and one of the API prefixes it serves. wakatime-cli appends
// /users/current/heartbeats.bulk to api_url, after stripping a trailing
// slash and a legacy endpoint suffix (normalizeURL in its pkg/params), so
// http://127.0.0.1:<port> alone would 404.
func PointsAtAgent(apiURL, localAddr string) bool {
	u, err := url.Parse(normalizeAPIURL(apiURL))
	if err != nil || u.Scheme != "http" {
		return false
	}
	_, wantPort, err := net.SplitHostPort(localAddr)
	if err != nil {
		return false
	}
	if u.Path != agentAPIPath && u.Path != agentHackatimePath {
		return false
	}
	return (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost") && u.Port() == wantPort
}

// The API prefixes the agent's local API serves.
const (
	agentAPIPath       = "/api/v1"
	agentHackatimePath = "/api/hackatime/v1"
)

// AgentAPIURL is the api_url editors should use for the agent at localAddr.
func AgentAPIURL(localAddr string) string { return "http://" + localAddr + agentAPIPath }

// normalizeAPIURL strips what wakatime-cli strips before using api_url.
func normalizeAPIURL(s string) string {
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".bulk")
	s = strings.TrimSuffix(s, "/users/current/heartbeats")
	s = strings.TrimSuffix(s, "/heartbeats")
	return strings.TrimSuffix(s, "/heartbeat")
}

// redactURL drops any user info and query from a URL before printing it.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "an unparsable URL"
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

func queueFindings(f Facts) []Finding {
	if f.StatusErr != nil {
		return []Finding{{Check: "queues", Level: Warn, Detail: "could not read the local database: " + f.StatusErr.Error(),
			Fix: "stop the agent, then run `logbook doctor` again; if it persists, reinstall"}}
	}
	out := []Finding{
		uploaderFinding("uploads", f.Now, f.Status.Logbook),
		uploaderFinding("mirror", f.Now, f.Status.Hackatime),
	}
	if f.Status.SpoolBytes > SpoolWarnBytes {
		out = append(out, Finding{Check: "hook spool", Level: Warn,
			Detail: fmt.Sprintf("%d bytes of AI hook events are waiting", f.Status.SpoolBytes),
			Fix:    "start the agent with `logbook run`; it imports them within a minute"})
	}
	return out
}

func uploaderFinding(name string, now time.Time, u status.Uploader) Finding {
	q, w := u.Queue, u.Worker
	switch {
	case w.Paused == upload.ReasonReLogin:
		return Finding{Check: name, Level: Fail, Detail: fmt.Sprintf("paused: the server rejected the key (%d waiting)", q.Pending),
			Fix: reloginFix(name)}
	case q.Dropped > 0:
		return Finding{Check: name, Level: Warn, Detail: fmt.Sprintf("%d heartbeats were dropped because the queue was full", q.Dropped),
			Fix: "keep the agent online so the queue drains"}
	case q.Pending > 0 && !q.Oldest.IsZero() && now.Sub(q.Oldest) > StaleQueue:
		detail := fmt.Sprintf("%d heartbeats waiting since %s", q.Pending, q.Oldest.Local().Format(time.DateTime))
		if w.LastError != "" {
			detail += "; last error: " + w.LastError
		}
		return Finding{Check: name, Level: Warn, Detail: detail,
			Fix: "check your connection; the agent retries automatically"}
	case w.Paused != "":
		return Finding{Check: name, Level: OK, Detail: w.Paused}
	}
	return Finding{Check: name, Level: OK, Detail: fmt.Sprintf("%d waiting", q.Pending)}
}

func reloginFix(name string) string {
	if name == "mirror" {
		return "your Hackatime key was rejected; run `logbook install` again to re-read it from your Hackatime setup"
	}
	return "run `logbook login --key <key>` with a current key"
}
