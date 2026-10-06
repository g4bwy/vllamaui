#!/usr/bin/env bash
# Restart the mock Strata server used by the adapter tests.
#   ./tests/restart-mock.sh [port]
set -euo pipefail
PORT="${1:-8121}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"
PIDFILE="$DIR/var/mock-$PORT.pid"
mkdir -p "$DIR/var"
if [[ -f "$PIDFILE" ]] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
	kill "$(cat "$PIDFILE")"
	sleep 1
fi
setsid node "$DIR/tests/mock-strata.mjs" "$PORT" < /dev/null > "$DIR/var/mock-$PORT.log" 2>&1 &
echo $! > "$PIDFILE"
sleep 1.5
head -1 "$DIR/var/mock-$PORT.log"
