package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/apneduniya/logbook/internal/agent/account"
	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/daemon"
	"github.com/apneduniya/logbook/internal/agent/doctor"
	"github.com/apneduniya/logbook/internal/agent/status"
	"github.com/apneduniya/logbook/internal/agent/version"
)

func doctorCommand() Command {
	return Command{
		Name:  "doctor",
		Usage: "check the setup and explain how to fix problems [--json]",
		Run: func(ctx context.Context, args []string) error {
			return runDoctor(ctx, args, os.Stdout)
		},
	}
}

// errProblemsFound makes `logbook doctor` exit non-zero when a check fails.
var errProblemsFound = errors.New("problems found")

func runDoctor(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	asJSON := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	findings := doctor.Diagnose(gatherFacts(ctx))
	var b strings.Builder
	if *asJSON {
		enc := json.NewEncoder(&b)
		enc.SetIndent("", "  ")
		if err := enc.Encode(findings); err != nil {
			return fmt.Errorf("encoding findings: %w", err)
		}
	} else {
		for _, f := range findings {
			fmt.Fprintf(&b, "[%-4s] %-13s %s\n", strings.ToUpper(string(f.Level)), f.Check, f.Detail)
			if f.Fix != "" {
				fmt.Fprintf(&b, "       %-13s fix: %s\n", "", f.Fix)
			}
		}
	}
	if _, err := io.WriteString(stdout, b.String()); err != nil {
		return fmt.Errorf("writing findings: %w", err)
	}
	for _, f := range findings {
		if f.Level == doctor.Fail {
			return errProblemsFound
		}
	}
	return nil
}

func gatherFacts(ctx context.Context) doctor.Facts {
	f := doctor.Facts{Now: time.Now()}
	dir, err := config.Dir()
	if err != nil {
		f.ConfigErr = err
		return f
	}
	if f.Config, f.ConfigErr = config.Load(dir); f.ConfigErr != nil {
		return f
	}
	if f.Config.APIKey != "" {
		v, err := account.Verify(ctx, daemon.NewHTTPClient(), f.Config.ServerURL, f.Config.APIKey, version.UserAgent())
		f.VerifyErr, f.ServerTime = err, v.ServerTime
	}
	local := &http.Client{Timeout: 3 * time.Second}
	if s, err := status.Fetch(ctx, local, f.Config.LocalAddr); err == nil {
		f.Status = s
	} else {
		f.DaemonErr = err
		f.Status, f.StatusErr = offlineStatus(ctx, dir, f.Config)
	}
	if path, err := doctor.WakatimeCfgPath(); err != nil {
		f.Wakatime = doctor.WakatimeCfg{Err: err}
	} else {
		f.Wakatime = doctor.ReadWakatimeCfg(path, f.Config.APIKey)
	}
	return f
}
