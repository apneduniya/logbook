#!/bin/bash
# Signs in to the local Logbook server as the demo user "orpheus" with dev
# sign-in, creates an API key, and exports it to later workflow steps as
# LOGBOOK_API_KEY (masked in logs). Usage: bootstrap.sh <server-url>
set -euo pipefail

server=$1
jar="$RUNNER_TEMP/cookies.txt"

for _ in $(seq 1 60); do
	curl -fsS "$server/healthz" >/dev/null 2>&1 && break
	sleep 2
done
curl -fsS "$server/healthz" >/dev/null

session=$(curl -fsS -c "$jar" -H 'Content-Type: application/json' \
	-d '{"username":"orpheus"}' "$server/auth/dev-login")
csrf=$(jq -r .csrf_token <<<"$session")
echo "signed in as $(jq -r .user.username <<<"$session")"

key=$(curl -fsS -b "$jar" -H "X-CSRF-Token: $csrf" -H 'Content-Type: application/json' \
	-d '{"name":"laptop"}' "$server/api/v1/api_keys" | jq -r .key)
echo "::add-mask::$key"
[ -n "$key" ] && [ "$key" != null ]
echo "created API key with prefix ${key:0:8}"
echo "LOGBOOK_API_KEY=$key" >>"$GITHUB_ENV"
rm -f "$jar"
