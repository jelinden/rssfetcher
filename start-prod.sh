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
#   RSSFETCHER_INTERVAL          fetch interval              (default 6h)
#   RSSFETCHER_FETCH_TIMEOUT     deadline of one cycle       (default 2m)
#   RSSFETCHER_MAX_ITEMS         items stored per feed       (default 25)
#   RSSFETCHER_COOKIE_SECURE     set "true" when serving over HTTPS
#   RSSFETCHER_IGNORE_FEED_TTL   set "true" to poll every feed on every cycle,
#                                ignoring the <ttl> a feed publishes
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

go build -mod=vendor -trimpath -ldflags "-s -w" -o rssfetcher rssfetcher.go

cookie_secure="${RSSFETCHER_COOKIE_SECURE:-false}"

exec ./rssfetcher \
	-env prod \
	-address "$MONGO_URI" \
	-listen "${RSSFETCHER_LISTEN:-0.0.0.0:9200}" \
	-interval "${RSSFETCHER_INTERVAL:-6h}" \
	-fetch-timeout "${RSSFETCHER_FETCH_TIMEOUT:-2m}" \
	-max-items-per-feed "${RSSFETCHER_MAX_ITEMS:-25}" \
	-honour-feed-ttl="$([ "${RSSFETCHER_IGNORE_FEED_TTL:-false}" = "true" ] && echo false || echo true)" \
	-admin-user "$RSSFETCHER_ADMIN_USER" \
	-admin-password "$RSSFETCHER_ADMIN_PASSWORD" \
	-auth-secret "$RSSFETCHER_AUTH_SECRET" \
	-cookie-secure="$cookie_secure"
