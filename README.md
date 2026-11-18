rssFetcher
==========

RSS/Atom fetcher with a small admin UI. Feeds, categories and news items are
stored in MongoDB; a background loop refreshes the feeds and upserts the newest
items.

## Run

```sh
go build -o rssfetcher rssfetcher.go

MONGO_URI=mongodb://127.0.0.1:27017/news ./start-local.sh   # dev
MONGO_URI=mongodb://... ./start-prod.sh                     # prod restart
```

Or directly:

```sh
./rssfetcher -address mongodb://127.0.0.1:27017 -database news -listen :9200
```

| flag                 | default                     | meaning                                                            |
| -------------------- | --------------------------- | ------------------------------------------------------------------ |
| `-address`           | `mongodb://localhost:27017` | MongoDB connection URI (`mongodb://` or `mongodb+srv://`)           |
| `-database`          | `news`                      | database name (the database in the URI wins)                        |
| `-listen`            | `:9200`                     | HTTP listen address                                                 |
| `-env`               | `dev`                       | `dev` refreshes every 2m, `prod` every 20m                          |
| `-interval`          | *(derived from `-env`)*     | explicit refresh interval, e.g. `30s`                               |
| `-interval-jitter`   | `10s` / `2m`                | random ± spread on the interval, so a restart does not hit every publisher at once |
| `-no-jitter`         | `false`                     | poll on an exact interval (a duration flag cannot tell "0" from "unset", so this is a boolean) |
| `-fetch-timeout`     | `2m`                        | deadline of one fetch cycle, independent of `-interval`              |
| `-honour-feed-ttl`   | `true`                      | skip a feed until the `<ttl>` it published has passed                |
| `-max-items-per-feed`| `25`                        | newest items stored per feed                                        |
| `-allow-private-urls`| `false`                     | let feeds resolve to loopback/RFC1918 addresses (SSRF guard off)     |
| `-templates`         | *(embedded)*                | read templates from disk instead                                    |
| `-fetch-on-start`    | `true`                      | refresh once while booting                                          |

## Fetching

The loop polls every active feed with a bounded concurrency (8 at a time), and
each cycle is a single pass:

* **Conditional GET.** The `ETag` / `Last-Modified` of the last successful fetch
  are stored on the feed and sent back as `If-None-Match` / `If-Modified-Since`.
  A `304` costs one small response and writes nothing - which also means raising
  `-max-items-per-feed` only takes effect on the next fetch that actually returns
  a body, and clearing `etag`/`lastModified`/`fetchedAt` on a feed document is how
  you force one.
* **Per feed `<ttl>`.** A publisher that says "refresh hourly" is skipped until
  that window has passed, even when `-interval` is shorter (`-honour-feed-ttl
  false` to ignore it).
* **Newest N.** Parsers collect up to 200 items and the newest
  `-max-items-per-feed` (by publication date) are upserted, so a feed that lists
  its oldest entries first is not the one that gets stored.
* **Relative references** are resolved against the feed URL, both links and
  identifiers — a bare `/news/1` guid would otherwise collide with `/news/1` of
  every other publisher. An opaque guid (`tag:…`, `urn:…`, a bare number) is kept
  verbatim or namespaced with the feed URL, respectively.
* **One subscription per URL.** Adding a feed whose URL is already subscribed is a
  `409` with a plain message, not a second row polled alongside the first.
* **SSRF guard.** Hosts are resolved before dialling and loopback, RFC1918,
  link-local (including `169.254.169.254`) and unique-local addresses are
  refused; redirects are re-validated. Point the fetcher at an internal service
  on purpose with `-allow-private-urls`.
* **Errors are reported.** A cycle in which feeds failed returns an error and
  logs it, instead of printing a healthy summary either way.

## Login

The admin UI requires a session cookie. **Neither the username nor the password
is stored in this repository or compiled into the binary** - the account is
configured through the environment, and the password is kept only as a **scrypt**
verifier (`scrypt$N$r$p$salt$hash`, N=32768, r=8, p=1, 32 byte key) in the
`usercollection` collection:

| | default | source |
| --- | --- | --- |
| username | *none* | `-admin-user` / `RSSFETCHER_ADMIN_USER` |
| password | *none* | `-admin-password` / `RSSFETCHER_ADMIN_PASSWORD` |

So the first start looks like this:

```sh
RSSFETCHER_ADMIN_USER=ops RSSFETCHER_ADMIN_PASSWORD='choose-one' MONGO_URI=mongodb://127.0.0.1:27017 ./start-local.sh
```

Leave the password out and the first start that has to create the account
generates a random 26 character one and prints it **once**:

```
rssfetcher: created admin user "ops"; only its scrypt hash is stored, never the password
rssfetcher:   password: 4KQZHTSY7MVB3DGNPXUWCEJRKF
rssfetcher: shown once and not recoverable from the database; change it in the admin UI, or set -admin-password / RSSFETCHER_ADMIN_PASSWORD
```

Copy it, log in, and set your own via the "Change password" panel. Later starts
print nothing, because the account already exists.

With no username configured nothing is seeded, which is not an error: the feed
fetcher needs no login at all, and an install whose accounts already exist in
the database runs fine without the variable. If the database holds **no**
account either, the start-up log says so explicitly, since the UI would
otherwise just be unreachable.

* Seeding is idempotent: an existing account is **never** overwritten, so
  rotating the password from the "Change password" panel survives restarts.
* Changing the password drops every session of that account (TTL'd
  `sessioncollection` documents; only the SHA-256 of the cookie value is stored).
* Sessions: `HttpOnly`, `SameSite=Lax`, `Path=/`, 12h lifetime (`-session-ttl`).
  An active session is renewed once half of that lifetime remains — the threshold
  is relative, so a short TTL still gets one renewal instead of one write per page
  view. When the store extends the deadline the cookie is re-issued with the same
  `-session-ttl` (it used to keep the expiry and the cookie disagreed). Add
  `-cookie-secure` behind TLS. Expired sessions are removed by MongoDB through a
  TTL index.
* Every state change is POST-only and carries a per-session CSRF token (HMAC of
  the session id, signed with `-auth-secret` / `RSSFETCHER_AUTH_SECRET`; a random
  one is generated per start when unset, which invalidates open forms on restart).
* Failed logins are throttled per client (5 per 5 min by default, then an
  exponential backoff). `-trust-proxy` makes the throttle use `X-Forwarded-For` —
  only enable it behind a proxy that sets that header.
* Unknown usernames and wrong passwords return the identical 401 response.

**Rotating a password you already seeded.** Seeding never overwrites, so change
it from the "Change password" panel after logging in. If you cannot (or would
rather not) log in with the old credential, replace the account instead; `<user>`
is the username currently stored in `usercollection`:

```sh
# rename the account, keeping its password hash, then rotate via the UI
mongosh --quiet news --eval 'db.usercollection.updateOne({username: "<user>"}, {$set: {username: "ops"}})'

# ...or drop it; the next start with RSSFETCHER_ADMIN_USER set creates it fresh
mongosh --quiet news --eval 'db.usercollection.deleteMany({username: "<user>"})'
```

`start-prod.sh` requires `RSSFETCHER_ADMIN_USER`, `RSSFETCHER_ADMIN_PASSWORD` and
`RSSFETCHER_AUTH_SECRET` in the environment and refuses to start without them;
`start-local.sh` forwards both variables when set and prints a random password
when the account is created.

## Assets and templates

`public/tmpl` is gone: the HTML (`app/handler/tmpl/*.html`) and the stylesheet
(`app/handler/static/style.css`) are embedded in the binary with `go:embed` and
served from `/static/style.css`. Pass `-templates <dir>` to render on-disk
templates instead (handy for tweaking the markup without a rebuild).

## Upgrading from the first version

Two stored fields changed shape, and both are self-healing on the next fetch:

* `feedcollection` gained `etag`, `lastModified`, `fetchedAt` and `ttl`. Feeds stored
  before this have none of them, so they are fetched unconditionally once and the
  validators are recorded from then on.
* `newscollection.rssGuid` for feeds that published **relative** identifiers changes
  from `/news/1` to `<feed url>|/news/1`. The old document is not deleted: the item
  simply appears once more under its namespaced guid. To clean up afterwards:

  ```sh
  # guids that are still a bare path (the old, colliding shape)
  mongosh --quiet news --eval 'db.newscollection.deleteMany({rssGuid: {$regex: "^/"}})'
  ```

  Feeds whose guids were already absolute URLs are unaffected.

## Test

```sh
go test ./...          # mongo dependent tests skip when no mongod is reachable
go test -race ./...
MONGO_TEST_URI=mongodb://127.0.0.1:27017 go test ./app/mongo/   # throwaway db
```
