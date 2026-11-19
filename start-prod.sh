#!/bin/bash
# Production run: builds a stripped binary, then starts it on 0.0.0.0:9200.
#
#   MONGO_URI="mongodb://user:pass@192.168.0.1:27017/news" ./start-prod.sh
#
# Required:
#   MONGO_URI                    connection string, including the database
#   RSSFETCHER_ADMIN_USER        admin username to create on the first start
#   RSSFETCHER_ADMIN_PASSWORD    password for it
#   RSSFETCHER_AUTH_SECRET       secret signing CSRF tokens
#
# Optional:
#   RSSFETCHER_LISTEN            listen address              (default :9200)
#   RSSFETCHER_INTERVAL          fetch interval              (default 80s, as it
#                                always was in this deployment - a news reader
#                                that polls every six hours looks broken)
#   RSSFETCHER_JITTER            random +/- spread on the interval (the binary
#                                derives one from -env when this is unset)
#   RSSFETCHER_MAX_FEED_TTL      ceiling on a feed's own <ttl> (default 15m; 0
#                                honours the publisher exactly)
#   RSSFETCHER_FETCH_TIMEOUT     deadline of one cycle       (default 2m)
#   RSSFETCHER_MAX_ITEMS         items stored per feed       (default 25)
#   RSSFETCHER_COOKIE_SECURE     set "true" when serving over HTTPS
#   RSSFETCHER_IGNORE_FEED_TTL   set "true" to poll every feed on every cycle,
#                                ignoring the <ttl> a feed publishes altogether
#   RSSFETCHER_BACKGROUND        set "true" to detach from the terminal
#
# By default the server stays in the foreground, which is what systemd
# (Type=simple) and containers need: the supervisor is then the thing that owns
# the process, restarts it and collects its stdout. Start with
# RSSFETCHER_BACKGROUND=true when you are logging in and starting it by hand -
# it then runs under nohup, its pid goes to run/rssfetcher.pid and its output to
# rssfetcher.log.
#
# Credentials are read from the environment; a .env file next to this script is
# the usual place to keep them (it is git-ignored, and should be chmod 600):
#
#   set -a; . ./.env; set +a; ./start-prod.sh
set -euo pipefail

cd "$(dirname "$0")"

if [ -z "${MONGO_URI:-}" ]; then
	: "${MONGO_USER:?set MONGO_USER (or MONGO_URI)}"
	: "${MONGO_PASSWORD:?set MONGO_PASSWORD (or MONGO_URI)}"
	MONGO_URI="mongodb://${MONGO_USER}:${MONGO_PASSWORD}@192.168.0.1:27017/news"
fi

: "${RSSFETCHER_ADMIN_USER:?refusing to start without RSSFETCHER_ADMIN_USER}"
: "${RSSFETCHER_ADMIN_PASSWORD:?refusing to start without RSSFETCHER_ADMIN_PASSWORD}"
: "${RSSFETCHER_AUTH_SECRET:?refusing to start without RSSFETCHER_AUTH_SECRET}"

# The binary reads all four from the environment (see envOr in rssfetcher.go),
# and that is how they are passed: argv is visible to every local user through
# ps(1) and /proc/<pid>/cmdline, so credentials must never be flags.
export MONGO_URI RSSFETCHER_ADMIN_USER RSSFETCHER_ADMIN_PASSWORD RSSFETCHER_AUTH_SECRET

go build -mod=vendor -trimpath -ldflags "-s -w" -o rssfetcher rssfetcher.go

cookie_secure="${RSSFETCHER_COOKIE_SECURE:-false}"

# Only passed when asked for: the binary clamps its own default to the interval,
# and a hardcoded jitter would turn a short RSSFETCHER_INTERVAL into a startup
# error.
if [ -n "${RSSFETCHER_JITTER:-}" ]; then
	args+=(-interval-jitter "$RSSFETCHER_JITTER")
fi

args=(
	-env prod
	-max-feed-ttl "${RSSFETCHER_MAX_FEED_TTL:-15m}"
	-listen "${RSSFETCHER_LISTEN:-0.0.0.0:9200}"
	-interval "${RSSFETCHER_INTERVAL:-80s}"
	-fetch-timeout "${RSSFETCHER_FETCH_TIMEOUT:-2m}"
	-max-items-per-feed "${RSSFETCHER_MAX_ITEMS:-25}"
	-honour-feed-ttl="$([ "${RSSFETCHER_IGNORE_FEED_TTL:-false}" = "true" ] && echo false || echo true)"
	-cookie-secure="$cookie_secure"
)

if [ "${RSSFETCHER_BACKGROUND:-false}" != "true" ]; then
	# exec, so that Ctrl-C, systemd and docker all talk to the server itself
	# rather than to a wrapper shell that would have to forward the signal.
	exec ./rssfetcher "${args[@]}"
fi

mkdir -p run
pidfile=run/rssfetcher.pid
if [ -f "$pidfile" ] && kill -0 "$(cat "$pidfile")" 2>/dev/null; then
	echo "refusing to start: rssfetcher is already running (pid $(cat "$pidfile"))" >&2
	echo "stop it first: kill $(cat "$pidfile")" >&2
	exit 1
fi

# The detached process keeps the pid nohup was given (nohup execs), so the
# pidfile is the server's own pid.
nohup ./rssfetcher "${args[@]}" >> rssfetcher.log 2>&1 &
pid=$!
echo "$pid" > "$pidfile"
echo "rssfetcher started in the background (pid $pid), listening on ${RSSFETCHER_LISTEN:-0.0.0.0:9200}"
echo "  logs: tail -f rssfetcher.log"
echo "  stop: kill $(cat "$pidfile")"
