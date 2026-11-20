package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	fmt.Println("Installing Logbook " + version)
	if err := svc.Install(ctx, o); err != nil {
		return err
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return err
	}
	fmt.Printf("\nDone. Logbook is tracking. Status page: http://%s/\n", cfg.LocalAddr)
	fmt.Println("Restart your editors so their WakaTime plugins pick up the new settings.")
	printPathHint(filepath.Dir(svc.BinaryPath()))
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
	userAgent := "logbook/" + version + " (" + runtime.GOOS + "-" + runtime.GOARCH + ")"
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

// printPathHint tells Linux users to add ~/.local/bin to PATH when it isn't,
// so they can type `logbook` later. Autostart and hooks use the full path
// and work either way.
func printPathHint(binDir string) {
	if runtime.GOOS == "windows" {
		return
	}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(d) == binDir {
			return
		}
	}
	fmt.Printf("To run `logbook` from a terminal, add %q to your PATH.\n", binDir)
}
