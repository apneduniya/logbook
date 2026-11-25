#!/bin/bash
# Takes the macOS docs screenshots on a GitHub-hosted runner: runs the real
# one-line installer and the agent's commands in Terminal.app, opens the
# status page in Safari, and captures each window with screencapture.
#
# Needs a Logbook server on LOGBOOK_SERVER and a key in LOGBOOK_API_KEY
# (set by the workflow). The key goes into a file each Terminal window
# sources before the screen is cleared, so it never appears on screen.
# Writes macos-<step>.png and macos-<step>.txt to the directory in $1.
set -euo pipefail

out=$1
mkdir -p "$out"
work="$RUNNER_TEMP/shots-work"
mkdir -p "$work"
log() { echo "[shots] $*"; }
# On failure, keep a picture of the whole screen to see what was in the way.
trap 'rc=$?; if [ $rc -ne 0 ]; then screencapture -x "$out/debug-failure-screen.png" || true; fi' EXIT

: "${LOGBOOK_API_KEY:?}" "${LOGBOOK_SERVER:?}"
bindir="$HOME/Library/Application Support/Logbook/bin"

# What each window sources before its command. Install runs without the
# binary's folder on PATH, as for a new user; later steps add it, as the
# installer's PATH hint says to.
umask 077
cat >"$work/env-install.sh" <<EOF
export LOGBOOK_API_KEY='$LOGBOOK_API_KEY'
export LOGBOOK_SERVER='$LOGBOOK_SERVER'
PROMPT='%1~ %% '
EOF
cp "$work/env-install.sh" "$work/env-after.sh"
printf 'export PATH="%s:$PATH"\n' "$bindir" >>"$work/env-after.sh"
# For a second install if the first fails: puts a launchctl shim first on
# PATH (see below).
cp "$work/env-install.sh" "$work/env-shim.sh"
printf 'export PATH="%s:$PATH"\n' "$work/shim" >>"$work/env-shim.sh"
umask 022

# v0.2.0's install runs "launchctl kickstart -k" right after loading the
# job with RunAtLoad, and on these runners that call takes 25-30 s (the
# job's ThrottleInterval), past install's 15 s limit, so install fails and
# rolls back. The shim runs kickstart in the background instead, so the
# rest of install, and the later shots, can go ahead.
mkdir -p "$work/shim"
cat >"$work/shim/launchctl" <<'EOF'
#!/bin/sh
if [ "$1" = kickstart ]; then
	(/bin/launchctl "$@" >/dev/null 2>&1 &)
	exit 0
fi
exec /bin/launchctl "$@"
EOF
chmod +x "$work/shim/launchctl"

# Let the runner's scripts drive Terminal over Apple Events. The image
# grants osascript and bash access to System Events, Finder and Safari but
# not Terminal, so the first "tell Terminal" would wait on a consent dialog.
now=$(date +%s)
for client in /usr/bin/osascript /bin/bash /bin/zsh /opt/hca/hosted-compute-agent /usr/local/opt/runner/provisioner/provisioner; do
	for target in com.apple.Terminal com.apple.Safari; do
		row="'kTCCServiceAppleEvents','$client',1,2,0,1,NULL,NULL,0,'$target',NULL,NULL,$now,NULL,NULL,'UNUSED',$now"
		sqlite3 "$HOME/Library/Application Support/com.apple.TCC/TCC.db" "INSERT OR REPLACE INTO access VALUES($row);" ||
			log "user TCC insert failed for $client -> $target"
		sudo sqlite3 "/Library/Application Support/com.apple.TCC/TCC.db" "INSERT OR REPLACE INTO access VALUES($row);" ||
			log "system TCC insert failed for $client -> $target"
	done
done

# New windows run zsh, the macOS default shell (the runner's user has bash).
defaults write com.apple.Terminal Shell -string /bin/zsh

# A Terminal profile setting: large Menlo, light background, ~1000x600.
osascript <<'EOF'
tell application "Terminal"
	activate
	delay 1
	set s to default settings
	set font name of s to "Menlo"
	set font size of s to 17
	set number of columns of s to 92
	set number of rows of s to 26
	set background color of s to {65535, 65535, 65535}
	set normal text color of s to {8000, 8000, 8000}
	set cursor color of s to {30000, 30000, 30000}
	repeat with w in windows
		close w
	end repeat
end tell
EOF
# zsh prints a one-time note when the shell has no rc file.
touch "$HOME/.zshrc"

wait_idle() { # seconds
	local i
	for ((i = 0; i < $1; i++)); do
		sleep 1
		if [ "$(osascript -e 'tell application "Terminal" to busy of selected tab of front window')" = false ]; then
			return 0
		fi
	done
	return 1
}

capture_window() { # app png
	local id bounds
	id=$(osascript -e "tell application \"$1\" to id of front window")
	log "capturing $1 window $id to $2"
	if ! screencapture -o -x -l"$id" "$2"; then
		bounds=$(osascript -e "tell application \"$1\" to get bounds of front window" | tr -d ' ')
		IFS=, read -r x1 y1 x2 y2 <<<"$bounds"
		screencapture -x -R"$x1,$y1,$((x2 - x1)),$((y2 - y1))" "$2"
	fi
	screencapture -x "$out/debug-$(basename "$2" .png)-screen.png" || true
}

terminal_shot() { # name envfile command
	local name=$1 envfile=$2 cmd=$3
	osascript - "$envfile" <<'EOF'
on run argv
	tell application "Terminal"
		activate
		do script "source " & quoted form of (item 1 of argv) & "; clear"
		delay 1
		set position of front window to {40, 40}
	end tell
end run
EOF
	wait_idle 30 || true
	sleep 1
	osascript - "$cmd" <<'EOF'
on run argv
	tell application "Terminal" to do script (item 1 of argv) in selected tab of front window
end run
EOF
	sleep 2
	if ! wait_idle 300; then
		log "$name still busy after 300 s"
	fi
	sleep 2
	capture_window Terminal "$out/macos-$name.png"
	osascript -e 'tell application "Terminal" to get contents of selected tab of front window' |
		sed -e 's/[[:space:]]*$//' | awk 'NF {n = NR} {l[NR] = $0} END {for (i = 1; i <= n; i++) print l[i]}' >"$out/macos-$name.txt"
	sed 's/^/  | /' "$out/macos-$name.txt"
	osascript -e 'tell application "Terminal" to close front window saving no' || true
	sleep 1
}

installer='curl -fsSL https://raw.githubusercontent.com/apneduniya/logbook/main/scripts/install.sh | sh'
datadir="$HOME/Library/Application Support/logbook"
svc="gui/$(id -u)/dev.boogles.logbook"
terminal_shot install "$work/env-install.sh" "$installer"
if [ ! -f "$datadir/config.json" ]; then
	# Keep the real failure for the report, then install with the shim.
	log "install failed; keeping it as macos-install-failure"
	mv "$out/macos-install.png" "$out/macos-install-failure.png"
	mv "$out/macos-install.txt" "$out/macos-install-failure.txt"
	/usr/bin/log show --last 3m --style compact --predicate 'eventMessage CONTAINS "dev.boogles.logbook"' 2>/dev/null | tail -20 || true
	terminal_shot install "$work/env-shim.sh" "$installer"
	echo "installed with the background kickstart shim" >"$out/macos-install-workaround.txt"
	[ -f "$datadir/config.json" ] || { log "install failed even with the shim"; exit 1; }
	sleep 40 # launchd throttles the restart

	# How long does kickstart -k take on a job that has run a while, and
	# on one that has just started (as during install)?
	launchctl print "$svc" 2>&1 | grep -E 'state =|pid =|runs =' || true
	for when in "on a job running for 40 s" "right after a restart"; do
		start=$(date +%s)
		perl -e 'alarm 90; exec @ARGV' /bin/launchctl kickstart -k "$svc"
		log "kickstart -k $when: exit $?, took $(($(date +%s) - start)) s"
	done
	sleep 40
	start=$(date +%s)
	perl -e 'alarm 90; exec @ARGV' /bin/launchctl kickstart -k "$svc"
	log "kickstart -k after 40 s more: exit $?, took $(($(date +%s) - start)) s"
	/usr/bin/log show --last 4m --style compact --predicate 'subsystem == "com.apple.xpc.launchd"' 2>/dev/null |
		grep -i -E 'boogles' | grep -v -i 'BTM' | tail -30 || true
	sleep 35
	launchctl print "$svc" 2>&1 | grep -E 'state =|pid =|runs =' || true
fi

sleep 15 # let the daemon register the device
terminal_shot status "$work/env-after.sh" 'logbook status'
terminal_shot doctor "$work/env-after.sh" 'logbook doctor'

# The status page in Safari.
local_addr=$(plutil -extract local_addr raw -o - "$HOME/Library/Application Support/logbook/config.json" 2>/dev/null || echo 127.0.0.1:47231)
url="http://$local_addr/"
log "opening $url in Safari"
open -a Safari "$url"
sleep 8
osascript <<'EOF' || true
tell application "Safari"
	activate
	set bounds of front window to {20, 30, 1004, 750}
end tell
EOF
sleep 3
capture_window Safari "$out/macos-status-page.png"
osascript -e 'tell application "Safari" to quit' || true

terminal_shot uninstall "$work/env-after.sh" 'logbook uninstall'
rm -f "$work"/env-*.sh
log done
