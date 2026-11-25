#!/usr/bin/env bash
# Starts or stops the local demo stack the docs screenshots are taken from:
# the API on 127.0.0.1:18480 (database logbook_demo, dev sign-in on) and
# the web UI, built against it, on localhost:18481. See README.md here.
#
#   scripts/docs-shots/stack.sh start   # build and start both, wait until they answer
#   scripts/docs-shots/stack.sh stop    # stop both
#
# Logs and pids go to $DEMO_DIR (default /tmp/logbook-demo).
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
dir=${DEMO_DIR:-/tmp/logbook-demo}
api_port=18480
web_port=18481
go=${GO:-go}
# The demo seed's default key (public, demo data only): the server must use
# the same key to read the review links the seed stored.
key=$(printf '%s' 'logbook-demo-key-not-a-secret!!!' | base64)

stop() {
	for name in api web; do
		if [[ -f "$dir/$name.pid" ]]; then
			pid=$(cat "$dir/$name.pid")
			kill "$pid" 2>/dev/null || true
			for _ in $(seq 50); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
			rm -f "$dir/$name.pid"
		fi
	done
}

wait_for() {
	for _ in $(seq 100); do
		curl -fs -o /dev/null "$1" && return 0
		sleep 0.2
	done
	echo "$1 didn't answer; see $dir/*.log" >&2
	return 1
}

case "${1:-}" in
start)
	mkdir -p "$dir"
	stop
	(cd "$root" && "$go" build -o "$dir/logbook-server" ./cmd/logbook-server)
	(cd "$root/web" && bun install --frozen-lockfile >/dev/null &&
		LOGBOOK_API_URL="http://localhost:$api_port" bun run build >"$dir/build.log" 2>&1) ||
		{ echo "web build failed; see $dir/build.log" >&2; exit 1; }
	# TZ=UTC: the API writes timestamps in the process's zone, as in production.
	TZ=UTC DATABASE_URL="postgres://logbook:logbook@127.0.0.1:54329/logbook_demo?sslmode=disable" \
		LOGBOOK_ADDR="127.0.0.1:$api_port" LOGBOOK_BASE_URL="http://localhost:$api_port" \
		LOGBOOK_WEB_URL="http://localhost:$web_port" LOGBOOK_DEV_LOGIN=true LOGBOOK_DEV_INSECURE_COOKIE=true \
		LOGBOOK_ENCRYPTION_KEY="$key" LOGBOOK_LOG_LEVEL=warn \
		nohup "$dir/logbook-server" >"$dir/api.log" 2>&1 &
	echo $! >"$dir/api.pid"
	# vite itself, not a wrapper, so the pid is the server's and stop stops it.
	cd "$root/web"
	nohup ./node_modules/.bin/vite preview --port "$web_port" --strictPort --host localhost >"$dir/web.log" 2>&1 &
	echo $! >"$dir/web.pid"
	cd "$root"
	wait_for "http://localhost:$api_port/healthz"
	wait_for "http://localhost:$web_port/"
	echo "API http://localhost:$api_port, web http://localhost:$web_port"
	;;
stop) stop ;;
*)
	echo "usage: $0 start|stop" >&2
	exit 2
	;;
esac
