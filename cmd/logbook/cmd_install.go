package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/apneduniya/logbook/internal/agent/version"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/apneduniya/logbook/internal/agent/config"
	"github.com/apneduniya/logbook/internal/agent/install"
	"github.com/apneduniya/logbook/internal/agent/install/autostart"
	"github.com/apneduniya/logbook/internal/agent/install/keycheck"
	"github.com/apneduniya/logbook/internal/agent/install/selfinstall"
	"github.com/apneduniya/logbook/internal/agent/install/wakacfg"
)

// keyEnv lets scripts pass the key without putting it on a command line,
// where other local users could see it in the process list.
const keyEnv = "LOGBOOK_API_KEY"

// daemonCommand is the subcommand every autostart entry runs.
const daemonCommand = "run"

// errNoDaemon stops install in a build without the daemon command.
var errNoDaemon = errors.New("this build has no `logbook run` daemon, so there is nothing to start at login; install a release build")

// hasCommand reports whether cmds has a command called name.
func hasCommand(cmds []Command, name string) bool {
	for _, c := range cmds {
		if c.Name == name {
			return true
		}
	}
	return false
}

func installCommand() Command {
	return Command{
		Name:  "install",
		Usage: "set up Logbook: --key <key> [--server <url>] [--no-mirror] [--mirror-url <url>] [--harness all|none|claude-code,codex,...]",
		Run:   runInstall,
	}
}

func uninstallCommand() Command {
	return Command{
		Name:  "uninstall",
		Usage: "remove Logbook and restore .wakatime.cfg [--purge --yes deletes local data]",
		Run:   runUninstall,
	}
}

// harnessInstallers lists the AI harness hook installers. The harness
// module provides them through harness.All(); this list is replaced with
// that call when the modules are merged, and until then install and
// uninstall simply have no harnesses to visit.
func harnessInstallers() []install.HarnessInstaller {
	return nil
}

func runInstall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	var o install.Options
	fs.StringVar(&o.Key, "key", "", "your Logbook API key (or set "+keyEnv+")")
	fs.StringVar(&o.ServerURL, "server", "", "Logbook server URL (default "+config.DefaultServerURL+")")
	fs.BoolVar(&o.NoMirror, "no-mirror", false, "don't also send heartbeats to where .wakatime.cfg pointed before")
	fs.StringVar(&o.MirrorURL, "mirror-url", "", "mirror heartbeats to this WakaTime-compatible API URL instead")
	fs.StringVar(&o.Harness, "harness", "all", "AI tools to hook: all, none, or a comma-separated list")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if o.Key == "" {
		o.Key = os.Getenv(keyEnv)
	}
	if o.Key == "" {
		return errors.New("--key is required; copy your API key from Logbook's settings page")
	}
	if !hasCommand(commands(), daemonCommand) {
		// Autostart would point at a command this binary lacks, and
		// systemd/launchd would restart the failing daemon forever.
		return errNoDaemon
	}
	src, err := executable()
	if err != nil {
		return err
	}
	o.SourceBinary = src

	dir, err := config.Dir()
	if err != nil {
		return err
	}
	svc, err := newInstallService(dir, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Println("Installing Logbook " + version.String())
	if err := svc.Install(ctx, o); err != nil {
		return err
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return err
	}
	fmt.Printf("\nDone. Logbook is tracking. Status page: http://%s/\n", cfg.LocalAddr)
	fmt.Println("Restart your editors so their WakaTime plugins pick up the new settings.")
	fmt.Print(pathHint(runtime.GOOS, svc.BinaryPath(), os.Getenv("PATH")))
	return nil
}

func runUninstall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	var o install.UninstallOptions
	fs.BoolVar(&o.Purge, "purge", false, "also delete the data directory (config, local database, logs)")
	fs.BoolVar(&o.Yes, "yes", false, "confirm --purge")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	svc, err := newInstallService(dir, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Println("Uninstalling Logbook")
	if err := svc.Uninstall(ctx, o); err != nil {
		return err
	}
	fmt.Println("\nDone. Restart your editors so their WakaTime plugins pick up the restored settings.")
	return nil
}

// installService wraps install.Service with the binary path for messages.
type installService struct {
	*install.Service
	bin string
}

func (s installService) BinaryPath() string { return s.bin }

// newInstallService builds the real install steps for this machine.
func newInstallService(dir string, out io.Writer) (installService, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return installService{}, fmt.Errorf("finding home directory: %w", err)
	}
	configHome, err := os.UserConfigDir()
	if err != nil {
		return installService{}, fmt.Errorf("finding config directory: %w", err)
	}
	wakaPath, err := wakacfg.Locate(wakacfg.OSEnv())
	if err != nil {
		return installService{}, err
	}
	target, err := selfinstall.Target(runtime.GOOS, home, os.Getenv("LOCALAPPDATA"))
	if err != nil {
		return installService{}, err
	}
	client := &http.Client{Timeout: 20 * time.Second}
	userAgent := version.UserAgent()
	svc := install.New(install.Deps{
		DataDir:   dir,
		Binary:    selfinstall.New(target),
		Waka:      wakacfg.New(wakaPath, dir, os.Getenv("WAKATIME_API_KEY")),
		Autostart: autostart.New(autostart.Options{Home: home, ConfigHome: configHome, DataDir: dir}),
		Harnesses: harnessInstallers(),
		Keys:      keycheck.New(client, userAgent),
		Out:       out,
	})
	return installService{Service: svc, bin: target}, nil
}

// executable returns the running binary's real path.
func executable() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("finding this program: %w", err)
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p, nil
}

// pathHint tells the user how to run `logbook` later. On macOS and Linux
// it suggests adding the binary's folder to PATH when it isn't there. On
// Windows the folder is never on PATH (changing it would mean editing the
// user's environment), so it prints the full path and the uninstall
// command. Autostart and hooks use the full path and work either way.
func pathHint(goos, bin, pathEnv string) string {
	if goos == "windows" {
		return fmt.Sprintf("Logbook is installed at %s\nTo remove it later, run in PowerShell: & \"%s\" uninstall\n", bin, bin)
	}
	dir := filepath.Dir(bin)
	for _, d := range filepath.SplitList(pathEnv) {
		if filepath.Clean(d) == dir {
			return ""
		}
	}
	return fmt.Sprintf("To run `logbook` from a terminal, add %q to your PATH.\n", dir)
}
