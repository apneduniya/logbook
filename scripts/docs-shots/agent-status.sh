#!/usr/bin/env bash
# Takes agent-status-page.png: runs the real agent in the foreground against
# the demo stack (stack.sh start), isolated in a temp dir, sends it a few
# editor heartbeats and screenshots its local status page.
#
# Nothing is installed: no autostart, no editor or AI-tool hooks, no change
# to the real ~/.wakatime.cfg. LOGBOOK_HOME, WAKATIME_HOME and HOME all point
# into the temp dir, which is deleted at the end with the agent stopped.
# The agent signs in as a separate demo user ("demo-agent"), so the
# screenshots of Orpheus and Nova don't change; its key and device are
# revoked afterwards.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
api=http://localhost:18480
go=${GO:-go}
tmp=$(mktemp -d)
pid=
cleanup() {
	[[ -n "$pid" ]] && kill "$pid" 2>/dev/null && wait "$pid" 2>/dev/null
	rm -rf "$tmp"
}
trap cleanup EXIT

mkdir -p "$tmp/home" "$tmp/logbook" "$tmp/wakatime"
(cd "$root" && "$go" build -ldflags "-X github.com/apneduniya/logbook/internal/agent/version.tag=v0.2.0" -o "$tmp/logbook-agent" ./cmd/logbook)
# The agent, seeing only the temp dir.
agent() { HOME="$tmp/home" LOGBOOK_HOME="$tmp/logbook" WAKATIME_HOME="$tmp/wakatime" "$tmp/logbook-agent" "$@"; }

# A key for the demo user, made as the web UI makes it.
jar="$tmp/cookies"
csrf=$(curl -fs -c "$jar" -H 'Content-Type: application/json' -d '{"username":"demo-agent"}' \
	"$api/auth/dev-login" | sed -E 's/.*"csrf_token":"([^"]+)".*/\1/')
key=$(curl -fs -b "$jar" -H 'Content-Type: application/json' -H "X-CSRF-Token: $csrf" \
	-d '{"name":"Demo computer"}' "$api/api/v1/api_keys" | sed -E 's/.*"key":"([^"]+)".*/\1/')

echo "signing the agent in"
printf '%s\n' "$key" | agent login --key - --server "$api" >/dev/null
echo "starting the agent"
HOME="$tmp/home" LOGBOOK_HOME="$tmp/logbook" WAKATIME_HOME="$tmp/wakatime" \
	"$tmp/logbook-agent" run --quiet </dev/null >/dev/null 2>&1 &
pid=$!
port=47231
for _ in $(seq 50); do curl -fs -m 2 -o /dev/null "http://127.0.0.1:$port/" && break; sleep 0.2; done
echo "agent up; sending heartbeats"

# A few heartbeats, as wakatime-cli sends them to the agent.
now=$(date +%s)
body="["
for i in 0 1 2 3 4 5; do
	t=$((now - 300 + i * 50))
	[[ $i -gt 0 ]] && body+=","
	body+="{\"entity\":\"/home/demo/code/pixel-garden/src/lib/garden.ts\",\"type\":\"file\",\"category\":\"coding\",\"time\":$t,\"project\":\"pixel-garden\",\"language\":\"TypeScript\",\"is_write\":$([[ $((i % 2)) -eq 0 ]] && echo true || echo false)}"
done
body+="]"
# wakatime-cli's auth: Basic base64(key), from a file so the key isn't in argv.
(umask 077 && printf 'Authorization: Basic %s\n' "$(printf '%s' "$key" | base64 -w0)" >"$tmp/auth")
curl -fsS -m 10 -o /dev/null -w 'heartbeats: %{http_code}\n' -H @"$tmp/auth" -H 'Content-Type: application/json' \
	-H 'User-Agent: wakatime/v1.130.0 (darwin-25.1.0-arm64) go1.25.0 vscode/1.106.1 vscode-wakatime/25.4.0' \
	-d "$body" "http://127.0.0.1:$port/api/v1/users/current/heartbeats.bulk"
# Let the uploader and the crediting run once.
sleep 15

echo "capturing the status page"
AGENT_STATUS_URL="http://127.0.0.1:$port/" timeout 120 node "$root/scripts/docs-shots/capture.mjs" agent-status-page

kill "$pid"
wait "$pid" 2>/dev/null || true
pid=
# Revoke the demo key and disconnect its device.
for dev in $(curl -fs -b "$jar" "$api/api/v1/me/devices" | grep -oE '"id":"[0-9a-f-]{36}"' | cut -d'"' -f4); do
	curl -fs -o /dev/null -X DELETE -b "$jar" -H "X-CSRF-Token: $csrf" "$api/api/v1/me/devices/$dev" || true
done
for id in $(curl -fs -b "$jar" "$api/api/v1/api_keys" | grep -oE '"id":"[0-9a-f-]{36}"' | cut -d'"' -f4); do
	curl -fs -o /dev/null -X DELETE -b "$jar" -H "X-CSRF-Token: $csrf" "$api/api/v1/api_keys/$id" || true
done
echo "done; the demo-agent key and device are revoked"
