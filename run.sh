#!/usr/bin/env bash
# Start the adapter. Without flags it runs in the foreground.
#   ./run.sh -d          run in background, log to var/adapter.log, pid to var/adapter.pid
#   ./run.sh --stop      stop a background instance
#   ./run.sh --status    show whether one is running
#
# Configuration lives in .env (gitignored, see .env.example). Every value can
# also come from the environment, which wins over the file:
#   VLLM_UPSTREAM  vLLM base URL          (default http://localhost:8000)
#   PORT           port to listen on      (default 8080)
set -euo pipefail

cd "$(dirname "$0")"
DIR="$PWD"
VAR="$DIR/var"
LOG="$VAR/adapter.log"
PIDFILE="$VAR/adapter.pid"

running() {
	[[ -f "$PIDFILE" ]] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null
}

case "${1:-}" in
--stop)
	if running; then
		kill "$(cat "$PIDFILE")"
		echo "stopped pid $(cat "$PIDFILE")"
		rm -f "$PIDFILE"
	else
		echo "not running"
	fi
	exit 0
	;;
--status)
	if running; then
		echo "running pid $(cat "$PIDFILE") -> ${VLLM_UPSTREAM:-see .env}"
	else
		echo "stopped"
	fi
	exit 0
	;;
esac

if [[ ! -d "$DIR/dist" || ! -f "$DIR/dist/index.html" ]]; then
	echo "error: dist/ is missing, run ./build.sh first" >&2
	exit 1
fi

if [[ "${1:-}" == "-d" ]]; then
	mkdir -p "$VAR"
	if running; then
		echo "already running pid $(cat "$PIDFILE")"
		exit 0
	fi
	# setsid detaches from the caller so the server survives the shell that started it
	setsid nohup node "$DIR/server/adapter.mjs" >"$LOG" 2>&1 < /dev/null &
	echo $! > "$PIDFILE"
	sleep 2
	if running; then
		echo "started pid $(cat "$PIDFILE"), log $LOG"
	else
		echo "failed to start, see $LOG" >&2
		exit 1
	fi
else
	exec node "$DIR/server/adapter.mjs"
fi
