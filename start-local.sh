#!/bin/bash
# Local development run: builds the binary, then starts it on 127.0.0.1:9200.
#
#   MONGO_URI=mongodb://127.0.0.1:27017 ./start-local.sh
#
# Without MONGO_URI the mongo credentials in MONGO_USER/MONGO_PASSWORD are used.
#
# The admin login is not configured here, and there is no default anywhere:
#
#   RSSFETCHER_ADMIN_USER      username to create on the first start
#   RSSFETCHER_ADMIN_PASSWORD  password for it; generated and printed once if
#                              you leave it out
#
# Existing accounts are never overwritten, so both are only needed the first
# time. Any extra arguments are forwarded to the binary, e.g.
#
#   ./start-local.sh -fetch-on-start=false -templates app/handler/tmpl
set -euo pipefail

cd "$(dirname "$0")"

if [ -z "${MONGO_URI:-}" ]; then
	: "${MONGO_USER:?set MONGO_USER (or MONGO_URI)}"
	: "${MONGO_PASSWORD:?set MONGO_PASSWORD (or MONGO_URI)}"
	MONGO_URI="mongodb://${MONGO_USER}:${MONGO_PASSWORD}@192.168.0.1:27017/news"
fi

go build -mod=vendor -o rssfetcher rssfetcher.go

args=(
	-env dev
	-address "$MONGO_URI"
	-listen 127.0.0.1:9200
	-interval 60s
	# Local runs pull every feed on every cycle; production respects <ttl>.
	-honour-feed-ttl=false
)
# Only pass what is actually configured: an empty -admin-user would otherwise
# look like an intentional choice to seed nothing.
if [ -n "${RSSFETCHER_ADMIN_USER:-}" ]; then
	args+=(-admin-user "$RSSFETCHER_ADMIN_USER")
fi
if [ -n "${RSSFETCHER_ADMIN_PASSWORD:-}" ]; then
	args+=(-admin-password "$RSSFETCHER_ADMIN_PASSWORD")
fi

# Templates and the stylesheet are compiled into the binary, so the working
# directory does not matter. Pass -templates <dir> to edit the markup live.
exec ./rssfetcher "${args[@]}" "$@"
