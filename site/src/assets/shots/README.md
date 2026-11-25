# Screenshot provenance

Web screenshots use fictional accounts on the local demo stack. See [the capture guide](../../../../scripts/docs-shots/README.md) for regeneration.

Windows and macOS screenshots come from GitHub-hosted Windows Server 2025 and macOS 15 runners, using the published v0.2.0 agent against a throwaway local server. They are real application windows; the Windows desktop may differ from a personal Windows laptop.

On macOS the unmodified installer failed at `launchctl kickstart -k` and rolled back. `macos-install-failure.png` and its transcript record that failure. The capture script then used a test-only `launchctl` shim that backgrounds kickstart to capture status, doctor and uninstall. `macos-install.png` records that modified environment and must not be presented as proof the unmodified installer works. The user guides disclose the workaround wherever those later screenshots appear. No workaround is installed on a reader's computer.

The capture scripts are in `.github/docs-shots/`, driven by `.github/workflows/docs-shots.yml`. Terminal transcripts are kept beside the images. No real account keys or personal project data should appear in screenshots.
