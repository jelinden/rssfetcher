# Code review — rssfetcher

Reviewed at `5ec833b` plus the uncommitted working tree (40 files, +6.4k/−0.8k, nothing committed yet).
Scope: `rssfetcher.go`, `app/{auth,domain,handler,mongo,rss}`, templates, start scripts.

Method: read every non-vendor file, then probed the suspicious paths with throwaway
tests (the "Evidence" lines below are real output, not theory) and a live run against
a local mongod. Test suite as reviewed: 115 tests, `-race` clean, `go vet` and `gofmt` clean.

| | count | fixed |
| --- | --- | --- |
| High | 2 | 2 |
| Medium | 7 | 7 |
| Low / polish | 16 | 16 |
| found while fixing | 4 | 4 |

**All 25 findings are fixed**, plus four more that surfaced while doing it (see
[Further findings](#further-findings)). The suite grew from 115 to 165 tests, still
`-race` clean; `app/domain` went from 0 % to 100 % coverage. See
[Fix log](#fix-log) for what each change cost and the live evidence for the ones that
are only observable against a real server.

## High

### H1. Open redirect through `?next=` on the login page
`app/handler/auth.go:404`

```go
if path == "" || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
	return "/view/"
}
```

`url.Parse` reports `Host == ""` for a path that *starts* with a backslash, and
browsers normalise `\` to `/` in the path of an `http:` URL — so `Location: /\evil.com`
is followed as `//evil.com`, i.e. an external, attacker-chosen host.

Evidence:

```
"/\evil.com"    -> "/\evil.com"     (accepted)
Location: "/\evil.com"
```

Impact: phishing hand-off. `?next=` is the one parameter on the login page that is
reflected into a redirect, and the login page is reachable unauthenticated.

Fix: after the `//` check, also reject a second character of `\`, and drop the
`url.Parse` branch that cannot fire for a relative path. A route allowlist
(`/view/`, `/edit/`) is stronger and just as short:

```go
path = strings.TrimSpace(path)
if len(path) < 2 || path[0] != '/' || (path[1] != '/' && path[1] != '\\') {
	return "/view/"
}
```

### H2. Sliding sessions do not work, and `-session-ttl` is silently ignored
`app/mongo/auth.go:243-277`, `app/handler/auth.go:326,384`, `app/mongo/mongo.go:41`

Three defects in one feature:

1. Renewal hardcodes the *default* lifetime, not the configured one:
   `bson.D{{Key: "expiresAt", Value: time.Now().Add(DefaultSessionTTL)}}`. With
   `-session-ttl 30m` a session is created for 30 minutes and then extended to 12
   hours. `Session()` never receives the TTL — the store signature has no parameter for it.
2. The renewal threshold is an absolute hour (`sessionRenewThreshold = time.Hour`).
   For any `-session-ttl` below ~1 h, `time.Until(expiresAt) < 1h` is *always* true, so
   every authenticated request becomes a database write — the exact thing the comment
   above it says the threshold exists to avoid.
3. The cookie is only ever issued at login (`setSessionCookie` is called from `login()`
   and nowhere else). The browser therefore drops the cookie at the original deadline
   regardless of what the database says. The extension is unobservable from a browser.

Fix: thread the TTL through (`Session(ctx, token, ttl)`), renew on a relative threshold
(`remaining < ttl/2`), and re-issue the session cookie in `authenticate` whenever the
store returns a session whose expiry was extended — which the handler can detect by
comparing `session.ExpiresAt` with the value it read, or simply always, since the
`Max-Age` it writes is derived from the session anyway.

## Medium

### M1. The login throttle never frees memory
`app/handler/auth.go:88-135`

`allowed()` deletes an entry only when that same key comes back after the window, and
`success()` only for a key that logs in. A failure from a previously unseen address adds
an entry that is never revisited.

Evidence: 5 000 `failure()` calls with distinct keys leave the entries in place; nothing
prunes them.

Impact: unbounded growth, and with `-trust-proxy` the key comes straight from
`X-Forwarded-For`, so a client can mint keys at will — a cheap memory DoS against a page
that is by definition reachable unauthenticated.

Fix: sweep on write (`if len(l.attempts) > 512 { delete expired }`), or bound the map and
evict. A per-key cap is not needed once the sweep exists.

### M2. Relative links and GUIDs are not resolved against the feed URL
`app/rss/{rss2,atom,rss1}.go`, `app/mongo/mongo.go` (`PrepareNewsItem`)

Evidence: a feed served from `https://example.com` with `<link>/news/relative</link>`
yields `link: "/news/relative"`, stored verbatim.

Two consequences:
* the admin UI renders a link that points at *this* server, not the article;
* `NormalizeGUID` of a relative id is not globally unique, so `/news/1` on site A and
  `/news/1` on site B collapse into one `rssGuid` and one of the two items silently
  disappears into the other's document.

Fix: resolve `link` (and `guid`, when it looks like a URL) against the feed's own URL
with `url.Parse(base)` + `ResolveReference`, and namespace non-absolute GUIDs with the
feed URL (`feedURL + "|" + guid`) before storing.

### M3. `FetchNews` cannot report failure
`app/mongo/mongo.go:408-431` — every per-feed and per-batch error is logged and then
`return written, nil`. The caller in `rssfetcher.go` prints
`N feeds, M items written` either way.

Impact: "all 40 feeds returned 503" and "the database is down" both look like a healthy
cycle in the log; there is nothing for a health check or an alert to see.

Fix: collect with `errors.Join`, and return a distinct error when *every* feed failed.
Keep the per-feed logging, and log the cycle as failed when `written == 0 && err != nil`.

### M4. Atom feeds throw away the full article body
`app/rss/atom.go:20` — `firstNonEmpty(item.Content, item.Content3, item.InnerXML)` where
`Content` is `<summary>` and `Content3` is `<content>`.

Evidence: an entry with both `<content>` (full HTML) and `<summary>` stores
`content: "Short summary"`.

This is the inverse of the rule applied to RSS 2.0, where `content:encoded` is preferred
over `<description>`. Fix: `firstNonEmpty(item.Content3, item.Content, item.InnerXML)`.

### M5. `app/domain` has no tests at all
`go test -cover` reports **0.0%** for the package (no `*_test.go`). It contains
`FeedInput.Validate` / `validateHTTPURL` — the check that stops an operator (or a
compromised session) from pointing the fetcher at `file://` or a non-http scheme, and
`Normalize`'s scheme defaulting. It is exercised indirectly through handler tests, but
that coverage is attributed elsewhere and the interesting edges (`://` inside a path,
`/relative`, `HTTP://UPPER`, IPv6 hosts, missing host) are not pinned down anywhere.

Fix: a table test per function; cheap and it protects the security boundary.

### M6. SSRF: the fetcher will dial anything
`app/rss/rss.go:85-97` — the transport uses the default resolver/dialer, and redirects
are followed (10 hops, default) with no re-validation.

An admin (or anyone with a session, or anyone who gets a feed row written) can add
`http://169.254.169.254/latest/meta-data/` or `http://127.0.0.1:9200/view/`; the response
body is parsed as a feed and its text is rendered back in the UI, i.e. a read primitive
against the local network. `domain.validateHTTPURL` only checks the *scheme*.

Severity depends on the deployment: admin-only, single-user → nuisance; anything hosted →
High. Fix: a `DialContext` that rejects loopback/RFC1918/link-local/ULA after resolution
(plus `CheckRedirect` re-running the same validation), behind a flag such as
`-allow-private-urls` for people who deliberately fetch from an internal host.

### M7. `/static/` serves a directory listing
`app/handler/templates.go:24-38` — `http.FileServer` on the embedded FS answers the
directory itself.

Evidence: `GET /static/ → 200 text/html` with `<a href="style.css">`. Traversal is
correctly blocked (`/static/../x` → 404) ✓.

Fix: return 404 for the trailing-slash root. While there: `Cache-Control: public,
max-age=3600` means a CSS change is invisible for an hour after deploy — prefer
`no-cache` (letting `Last-Modified`/`Range` do the work) or a content-hashed filename.

## Low / polish

1. **Atom channel image is never parsed** (`atom.go:80-86`): the struct matches
   `xml:"image"`, but Atom's elements are `<icon>` and `<logo>`. Evidence: both are empty
   after parsing a feed that has them. Consequence: no channel-level thumbnail for Atom.
2. **`<ttl>` and `<refresh>` are parsed then ignored** (`rss2.go:52`, `atom.go:17`), and
   the prod default interval is **80 seconds** (`rssfetcher.go:57`) for every feed. That
   is aggressive enough to get the instance rate-limited or banned by small publishers.
   Suggest defaulting to 15–30 min with per-cycle jitter, honouring `ttl`, and adding
   conditional GET (`If-None-Match`/`If-Modified-Since` with stored `etag`/`lastModified`
   per feed) — the bandwidth win alone justifies it.
3. **Zone abbreviations parse as UTC** (`app/rss/time.go` format list): `Wed, 01 Jan 2025
   10:00:00 MST` → offset 0s (confirmed). Go has no abbreviation table, so the instant is
   wrong by hours; `PrepareNewsItem` then rewrites any resulting "future" date to `now`,
   silently destroying the real publication date. Suggest a small abbreviation map
   (EST/EDT/CST/MST/PST/…→ offsets) or clamping only when drift exceeds ~24 h.
4. **`NewsItemUpdates` takes the first N items, not the newest N** (`mongo.go`,
   `if len(models) >= rss.MaxItemsPerFeed { break }`) while both the function comment and
   `MaxItemsPerFeed`'s doc promise "the newest items". Fix: sort by `Date` descending
   before truncating.
5. **`SaveFeedItem` upserts** (`mongo.go:163-183`): flagging a feed that has been deleted
   recreates it as a stub with `removed=true`. Use `SetUpsert(false)` and map
   `ErrNoDocuments` to `ErrNotFound`.
6. **`SaveCategory`/`SaveSubCategory` only `$setOnInsert`** — the edit form's `enName` and
   `styleName` are silently ignored for a category that already exists, so the UI offers
   inputs that do nothing. Either update them (`$set` for those two keys) or remove them
   from the form.
7. **Duplicated `$set` document** in `SaveFeed`/`SaveFeedItem` (five keys, twice) — one
   `feedSetDocument(f domain.Feed) bson.D` removes the drift risk.
8. **`EnsureIndexes` discards the returned index name** (`_ = name`) — v2 hands it back
   precisely so a rename can be detected; log it once at debug level, or assert it equals
   the requested name.
9. **`HasAdminUsers`** (`auth.go:57-70`) filters on `username: {$gt: ""}` and justifies it
   with a comment about text-index hints that does not apply here. `bson.D{}` + `SetLimit(1)`
   says the same thing honestly.
10. **Fetch cycle lifetime**: `context.WithTimeout(ctx, interval)` means a cycle is
    bounded by the tick period; with a short `-interval` downloads are cut off. At
    shutdown, `defer stop()` runs *after* `defer client.Disconnect(ctx)`, so in-flight
    bulk writes are cancelled rather than awaited. Suggest a `sync.WaitGroup` around the
    fetcher and an explicit per-cycle timeout independent of the interval.
11. **`MaxItemsPerFeed = 5` is a compile-time constant** with no flag. For a news reader
    that is very small; `-max-items-per-feed` (default 25, say) costs nothing.
12. **CSRF secret fallback** (`handlers.go:57-63`): if `crypto/rand` fails, the fallback is
    `fmt.Sprintf("insecure-%d", time.Now().UnixNano())` — derivable by anyone who saw the
    log line. If the random source is broken, exiting is more honest than a weak secret.
13. **`Handler.CurrentUser`** (`auth.go:415`) is exported but unused (templates get the
    username through `page()`); delete it or use it.
14. **`ChangePassword` is verify → read → write** (`auth.go:171-193`) — not atomic; two
    concurrent changes can interleave. Cheap fix: filter the update on the *old* hash, so
    it becomes a compare-and-swap.
15. **Error strings leak internals into the UI**: `http.Error(w, err.Error(), 400)` renders
    `mongo: weak password: auth: password must be at least 8 characters`. Prefer a
    message built for users, and keep the prefixed error for the log.
16. **Stale comment and CSP** in `middleware.go`: the `SecurityHeaders` doc still says "the
    UI has no authentication at all; that is the remaining known gap" — false since the
    login landed. The CSP carries `style-src 'unsafe-inline'` (no template uses an inline
    style) and `img-src * data:` (allows arbitrary remote images); tighten both, and add
    `Strict-Transport-Security` when `-cookie-secure` is on.

## Good parts (worth preserving in any further refactor)

* **The seams are the right ones.** `handler.Store`, `rss.FetchFunc`, `feedRefresher` and
  `adminUserStore` (with their fakes) are why 115 tests exist at all — the
  original package-level `MongoClient` made the handlers literally untestable (the init-time
  `template.ParseGlob("public/tmpl/*")` panic).
* **Correct MongoDB usage**: bulk upsert instead of two queries per item, unordered writes,
  `$setOnInsert` protecting `clicks`/`read`/first-seen `pubDate`, a *partial* unique index on
  `rssGuid` (so pre-existing documents without the field don't break the constraint), TTL
  index for sessions, per-operation timeouts, `IsDuplicateKeyError` treated as success on
  concurrent seed.
* **Auth design is sound in the large**: scrypt with self-describing parameters, hash-only
  session storage, generic 401 for unknown user vs wrong password, dummy-hash timing
  equalisation, POST-only mutations with per-session CSRF, password change dropping all
  sessions, and no credential in the repository or the binary.
* **Templates are auto-escaped everywhere** (no `template.HTML`, no raw output found), and
  every mutating form carries a token — enforced by a test rather than by convention.
* Embedded assets removed the working-directory dependency entirely; `-templates <dir>`
  keeps live editing possible.

## Process

* **Nothing is committed.** Split the working tree into reviewable commits: driver v2 →
  index/partial-index fix → auth primitives → store auth layer → handler auth → embed +
  CSS → credential removal.
* **No CI.** Add a workflow running `gofmt -l`, `go vet ./...`, `go test -race -cover
  ./...`; `golangci-lint` is not installed locally and would catch the unused exported
  API, the ignored return values and the dead fields listed above.
* **The suite silently shrinks without a mongod.** `testStore` calls `t.Skipf` when the
  URI is unreachable (verified: `MONGO_TEST_URI=mongodb://127.0.0.1:1` → `--- SKIP`), so CI
  without a service container reports green while the mongo package is barely tested. Add a
  service container, or make the absence of mongod an explicit `t.Fatal` unless a
  `-short`/`RSSFETCHER_CI=1` marker says otherwise.
* `go.mod` is `go 1.26.0` with `vendor/`; keep `-mod=vendor` in CI so a stale vendor tree
  fails loudly.

## Suggested order of work

1. H1, H2 (small, self-contained, both are user-visible today).
2. M5 (tests for `app/domain`) then M1, M3, M7 — cheap and each removes a failure mode.
3. M2 + L4 together: they are the same code path and both are "the item we stored is not
   the item you meant".
4. M6 + M4 + L1, L2, L3 behind a `-max-items-per-feed` / conditional-GET / SSRF-guard
   change set.
5. The Low list as one cleanup commit, plus CI.

## Further findings

Discovered by the fixes and the live runs, in the same severity order. All four are
fixed.

### F1. The same feed URL could be subscribed twice
Nothing in `SaveFeed` or in the indexes stops a second document with an identical
`url`. Live evidence (adding the same feed twice through the UI, then one fetch
cycle):

```
--- feedcollection
  BBC    url=https://feeds.bbci.co.uk/news/world/rss.xml
    category={"_id":{"$oid":"000000000000000000000000"},"categoryName":"",...}
  BBC    url=https://feeds.bbci.co.uk/news/world/rss.xml
    category={"_id":{"$oid":"6afde182a0384599aef69b48"},"categoryName":"world",...}
```

Two rows are polled, listed and deduplicated independently, and the one created
without a category produces items that appear in no category view. There is no
unique index on `url`, and adding one now would fail against databases that already
contain duplicates - the same trap as the `rssGuid` index (L8/M3 in the original
list). Fixed in the write path instead: an insert that finds an active feed with the
same URL returns `ErrDuplicateFeed`, which the handler answers with **409** and a
plain sentence.

### F2. `-interval-jitter 0` could not disable jitter
`config.jitter()` treated `0` as "not set", so the flag's own documentation was
false. A duration flag cannot distinguish "absent" from "zero", so the switch is a
separate `-no-jitter` boolean and `-interval-jitter` now only sets the size.
`validate()` rejects a jitter that is as large as the interval (which would let two
cycles overlap).

### F3. `FeedInput.Normalize` detected schemes with `strings.Contains`
`"example.com/a://b"` counted as "already has a scheme" and was stored as a relative
link, which the UI then renders against *this* server. `hasScheme` now parses the
prefix the way `url.Parse` does (letters first, then digits/`+`/`-`/`.`). Found by
the M5 tests, which is exactly why that package needed them.

### F4. A DNS failure was reported as a policy decision
The first version of the SSRF guard returned a bool, so `www.intercon-spokane.com`
(NXDOMAIN) produced:

```
rss: refusing to fetch a private address: www.intercon-spokane.com
```

which sends an operator hunting for a proxy or a firewall rule. `checkHostAddresses`
returns the resolver error instead (`rss: resolving %s: %w`), and the guard message is
only used for an address that really is private.

## Fix log

### High

* **H1** - `safeNext` rejects a backslash before the URL is parsed, and the login
  handler re-checks the target it is about to send. `TestSafeNextKeepsRedirectsLocal`
  covers `/\evil.com`, `\/evil.com`, `//evil.com`, `\evil.com`;
  `TestLoginRedirectIsAlwaysLocal` asserts the `Location` header itself, so a future
  handler cannot bypass the helper. Live probe against the built binary (before the
  fix it answered `Location: /\evil.com`):

  ```
  /\evil.com         -> Location: /view/
  //evil.com         -> Location: /view/
  https://evil.com   -> Location: /view/
  /view/             -> Location: /view/
  /edit/abc          -> Location: /edit/abc
  ```
* **H2** - `Store.Session` now takes the configured TTL and reports whether the
  deadline moved, so the handler can re-issue the cookie; the renewal threshold is
  `ttl/2` instead of a fixed hour. Three tests pin it: the store is called with the
  configured TTL, a session near expiry is renewed *with that TTL*, and a short TTL
  does not renew on every request. Live probe with `-session-ttl=3s
  -cookie-secure`: the login response carries `Strict-Transport-Security`, the
  `Secure` flag, and two seconds later (one second from expiry, inside the `ttl/2`
  window) the session is extended and `Set-Cookie` is re-issued with a new `Expires`.

### Medium

* **M1** - the login limiter prunes stale keys, bounded by `loginLimiterMaxEntries`.
  The prune condition initially dropped entries that were still in backoff (a zero
  `blocked` time is "no backoff", not "expired"), which the test caught because the
  limiter answered 401 instead of 429.
* **M2** - `ResolveReferences` resolves the channel link, item links, enclosures and
  channel image against the fetch URL; guids that are relative references are resolved
  and opaque ones are namespaced with the feed URL. Proven live: `<link>posts/1</link>`
  stored as `http://127.0.0.1:19500/rel/posts/1`, `<guid>posts/1</guid>` stored as
  `127.0.0.1:19500/rel/feed.xml|posts/1`.
* **M3** - `FetchNews` joins per-feed errors and returns them; when *every* feed
  failed, the message says so. Verified live against a host that does not resolve:
  the cycle now logs `fetch cycle: feed ... : rss: refusing to fetch a private address`
  instead of the old "2 feeds, 3 items written" lie.
* **M4** - atom prefers `<content>` over `<summary>`; live run stored
  `<p>full body</p>` from `content:encoded` for RSS 2.0 and the equivalent for atom.
* **M5** - `app/domain` has 100 % statement coverage, including the URL edge cases the
  security boundary depends on (`file://`, `gopher://`, `javascript:`, `http://`,
  IPv6 hosts, control characters, bad percent escapes).
* **M6** - guarded `DialContext` + `CheckRedirect` re-validation behind
  `-allow-private-urls`. Note that the guard checks *resolved* addresses, so
  `internal.example.com` pointing at `169.254.169.254` is refused too.
* **M7** - `/static/` answers 404 for the directory; cache header is
  `public, max-age=0, must-revalidate` so a CSS change is visible immediately.
  Verified live: `GET /static/ → 404`, `GET /static/style.css → 200 text/css`.

### Low

* **L1** - atom `<logo>`/`<icon>` map to the channel image (there is no `<image>`
  element in atom); `media:content` is collected next to `media:thumbnail`.
* **L2** - `<ttl>` is stored on the feed and honoured (`FeedsDueForFetch`),
  conditional GET sends the stored validators, the prod default interval is 20 minutes
  with jitter, and `-interval`, `-interval-jitter`, `-no-jitter`, `-fetch-timeout` and
  `-honour-feed-ttl` control it. Live: first cycle stored 2 items and an `ETag`, the
  next cycle wrote 0.
* **L3** - a zone abbreviation table turns `MST`/`EDT`/`CET`/... into numeric offsets
  before parsing; `PrepareNewsItem` only clamps a date more than 24 h in the future,
  so a publisher with a slightly fast clock keeps its real publication time.
* **L4** - parsers collect up to 200 items and `NewestItems` selects the newest N by
  date before the bulk write. `TestNewsItemUpdatesKeepsTheNewestItems` pins both the
  model list and what lands in the collection.
* **L5** - `SaveFeedItem` no longer upserts; a missing document is `ErrNotFound`.
* **L6** - category and sub category edits `$set` the presentation fields
  (`enName`, `styleName`) while `_id` stays `$setOnInsert`, so the form's inputs do what
  they look like they do and editing never replaces the id.
* **L7** - one `feedSetDocument` for both writers, with a test that round-trips every
  field of `domain.Feed` through the database.
* **L8** - `indexSpec` carries its name; `EnsureIndexes` fails if the server returns a
  different one, and the error names the index and the collection.
* **L9** - `HasAdminUsers` counts with an empty filter and `SetLimit(1)`.
* **L10** - `net.Listen` before any database write, a cycle deadline independent of the
  interval, `context.WithoutCancel` so an in-flight bulk write is not cancelled by a
  shutdown signal, and `main` waits for the fetcher before disconnecting.
* **L11** - `-max-items-per-feed`, backed by `rss.SetMaxItemsPerFeed` (atomic, so the
  fetch loop and a test cannot race on it).
* **L12** - `handler.New` returns an error instead of falling back to a time-derived
  CSRF secret; it also refuses a nil store or nil templates.
* **L13** - dead `CurrentUser` removed.
* **L14** - `setPasswordHash` is a compare-and-swap on the previous hash
  (`ErrConflict` when it moved). Fixing it exposed a second bug: the exported
  `VerifyAdminPassword` clears the hash it returns, so `ChangePassword` must use the
  internal verifier - and a hash upgrade must report the value it just wrote, or the
  CAS compares against a hash its own upgrade replaced.
* **L15** - the password form says "at least 8 characters" (from
  `auth.MinPasswordLen`) and the prefixed error goes to the log; `fail()` maps
  `ErrNotFound`/`ErrInvalidID`/`ErrConflict`/`ErrDuplicateFeed` to 404/400/409 instead
  of a blanket 500.
* **L16** - CSP without `unsafe-inline`, with `script-src 'none'`, `img-src 'self'`,
  `frame-ancestors 'none'`, `form-action 'self'`; HSTS only when `-cookie-secure`.
  `TestSecurityHeadersCSPMatchesTheTemplates` plus
  `TestEmbeddedTemplatesUseNoInlineStylesOrScripts` fail if a template adds an inline
  style or script, which is what makes the tight CSP safe to keep.

### Process

* `.github/workflows/ci.yml` runs gofmt, vet, build, `go test -race -cover` against a
  MongoDB service container, and **fails if the mongo suite skips** - the local
  convenience of `t.Skipf` without a mongod is exactly what makes CI meaningless.
* Live evidence for the parts a unit test cannot reach (real publishers, real
  conditional requests, the duplicate subscription) is in the finding entries above.
