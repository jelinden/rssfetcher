package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jelinden/rssfetcher/app/auth"
	"github.com/jelinden/rssfetcher/app/domain"
	"github.com/jelinden/rssfetcher/app/handler"
	"github.com/jelinden/rssfetcher/app/mongo"
	"github.com/jelinden/rssfetcher/app/rss"
)

// The admin login has no default username and no default password: both come
// from RSSFETCHER_ADMIN_USER / RSSFETCHER_ADMIN_PASSWORD (or the -admin-user /
// -admin-password flags), so nothing that grants access is stored in this
// repository or compiled into the binary. With no password configured, a first
// start generates a random one and prints it once; change it later from the
// admin UI ("Change password").

type config struct {
	mongoURI         string
	database         string
	env              string
	addr             string
	templateDir      string
	fetchEvery       time.Duration
	fetchJitter      time.Duration
	noJitter         bool
	fetchTimeout     time.Duration
	checkTimeout     time.Duration // budget of one "check now" request in the admin UI
	fetchOnStart     bool
	maxItems         int
	allowPrivateURLs bool
	maxFeedTTL       time.Duration // ceiling on what a publisher's <ttl> may demand
	keepFeedTTL      bool
	adminUser        string
	adminPassword    string
	authSecret       string
	sessionTTL       time.Duration
	cookieSecure     bool
	trustProxy       bool
}

// Default refresh period. Eighty seconds (what the first version used in prod)
// is aggressive enough to get a small instance rate limited or banned by the
// publishers it polls; a news reader does not need sub-minute freshness, and
// per feed <ttl> (see -honour-feed-ttl) refines it further.
const (
	devFetchInterval  = 2 * time.Minute
	prodFetchInterval = 20 * time.Minute

	defaultFetchTimeout = 2 * time.Minute
	defaultFetchJitter  = 2 * time.Minute
	devFetchJitter      = 10 * time.Second
)

// interval is the feed refresh period. Zero means "depends on -env".
func (c config) interval() time.Duration {
	if c.fetchEvery > 0 {
		return c.fetchEvery
	}
	if c.env == "dev" {
		return devFetchInterval
	}
	return prodFetchInterval
}

// jitter spreads the start of a cycle so a restart of many instances (or one
// instance with forty feeds) does not hit every publisher at the same second.
//
// Turning jitter off is a separate boolean rather than "-interval-jitter 0",
// because a zero flag default makes 0 indistinguishable from "not set" - the
// documented way of disabling it would silently have done nothing.
// jitter is the spread applied to the interval. A configured value is used as
// given (validate rejects nonsense); the value derived from -env is capped at a
// quarter of the interval, because a default must never contradict a schedule the
// operator asked for.
func (c config) jitter() time.Duration {
	if c.noJitter {
		return 0
	}
	if c.fetchJitter > 0 {
		return c.fetchJitter
	}
	spread := devFetchJitter
	if c.env != "dev" {
		spread = defaultFetchJitter
	}
	// The default must fit the interval it is applied to: a two minute spread on a
	// forty second schedule is not a spread, it is a startup error.
	if max := c.interval() / 4; spread > max {
		spread = max
	}
	if spread < 0 {
		return 0
	}
	return spread
}

// cycleTimeout bounds one fetch cycle. It is deliberately not the interval: a
// short interval used to cut downloads off mid flight, and a long one let a
// wedged cycle run for twenty minutes.
func (c config) cycleTimeout() time.Duration {
	if c.fetchTimeout > 0 {
		return c.fetchTimeout
	}
	return defaultFetchTimeout
}

// csrfSecretWarning explains what happens when no CSRF secret is configured. It
// is a function rather than an inline log call so the wording is testable, and it
// is worth a line at startup: signing keys that change every boot are a
// deliberate choice, not something the process should decide quietly.
func (c config) csrfSecretWarning() string {
	if strings.TrimSpace(c.authSecret) != "" {
		return ""
	}
	return "no -auth-secret / RSSFETCHER_AUTH_SECRET is set: CSRF tokens are signed " +
		"with a key generated for this process, so forms left open across a restart " +
		"will be rejected - set one deliberately (for example: openssl rand -hex 32)"
}

// validate rejects flag combinations that would silently do the wrong thing.
func (c config) validate() error {
	// Zero means "not configured": applyFetchPolicy turns it into the package
	// default. Only a negative limit is nonsense.
	if c.maxItems < 0 {
		return fmt.Errorf("-max-items-per-feed must not be negative, got %d", c.maxItems)
	}
	if c.fetchJitter < 0 {
		return fmt.Errorf("-interval-jitter must not be negative, got %s", c.fetchJitter)
	}
	// Only an explicitly configured jitter is argued about; the one derived from
	// -env is clamped to fit, so lowering -interval cannot fail to start.
	if c.fetchJitter > 0 && c.interval() <= c.fetchJitter {
		return fmt.Errorf("-interval-jitter %s must be smaller than -interval %s", c.fetchJitter, c.interval())
	}
	if c.maxFeedTTL < 0 {
		return fmt.Errorf("-max-feed-ttl must not be negative, got %s", c.maxFeedTTL)
	}
	if c.env != "dev" && c.env != "prod" {
		return fmt.Errorf("-env must be dev or prod, got %q", c.env)
	}
	return nil
}

// fetchOptions turns the parsed flags into what the loop needs, with the fetcher
// left nil so the loop uses rss.Fetch.
func (c config) fetchOptions() fetchOptions {
	return fetchOptions{
		interval:     c.interval(),
		jitter:       c.jitter(),
		cycleTimeout: c.cycleTimeout(),
		fetchOnStart: c.fetchOnStart,
	}
}

// applyFetchPolicy pushes the flags that are enforced inside other packages out
// to them, so the flags are the single source of truth.
func (c config) applyFetchPolicy() {
	rss.SetMaxItemsPerFeed(c.maxItems)
	rss.SetAllowPrivateURLs(c.allowPrivateURLs)
}

func registerFlags(f *flag.FlagSet, c *config) {
	// MONGO_URI rather than a flag in the scripts: a URI carries
	// user:password, and argv is world readable through ps(1).
	f.StringVar(&c.mongoURI, "address", envOr("MONGO_URI", "mongodb://localhost:27017"), "mongo connection URI (defaults to $MONGO_URI)")
	f.StringVar(&c.database, "database", "", "mongo database name; defaults to the one in -address, then "+mongo.DefaultDatabase)
	f.StringVar(&c.env, "env", "dev", "environment (dev|prod)")
	f.StringVar(&c.addr, "listen", ":9200", "http listen address")
	f.StringVar(&c.templateDir, "templates", "", "read templates from this directory instead of the embedded ones")
	f.DurationVar(&c.fetchEvery, "interval", 0, "how often feeds are refreshed (default: 2m in dev, 20m in prod)")
	f.DurationVar(&c.fetchJitter, "interval-jitter", 0, "random +/- spread on the refresh interval (default: 10s in dev, 2m in prod)")
	f.BoolVar(&c.noJitter, "no-jitter", false, "poll on an exact interval with no random spread")
	f.DurationVar(&c.fetchTimeout, "fetch-timeout", 0, "deadline of one fetch cycle, independent of -interval (default 2m)")
	f.BoolVar(&c.fetchOnStart, "fetch-on-start", true, "refresh all feeds once at startup")
	f.IntVar(&c.maxItems, "max-items-per-feed", rss.DefaultMaxItemsPerFeed, "newest items stored per feed")
	f.BoolVar(&c.keepFeedTTL, "honour-feed-ttl", true, "respect a feed's <ttl> and skip it until that period has passed")
	f.DurationVar(&c.maxFeedTTL, "max-feed-ttl", 0, "cap on the per-feed <ttl> a publisher may impose (0 honours it exactly; try 15m for a news reader)")
	f.BoolVar(&c.allowPrivateURLs, "allow-private-urls", false, "allow feeds that resolve to loopback or private networks (SSRF guard off); only for deliberately internal setups")

	f.StringVar(&c.adminUser, "admin-user", envOr("RSSFETCHER_ADMIN_USER", ""), "username of the admin account to seed on first start (no default)")
	f.StringVar(&c.adminPassword, "admin-password", envOr("RSSFETCHER_ADMIN_PASSWORD", ""), "password of the admin account; when empty, a random one is generated and printed once on first start")
	f.DurationVar(&c.checkTimeout, "check-timeout", handler.DefaultRefetchTimeout, "how long a 'check now' request in the admin UI may take")
	f.StringVar(&c.authSecret, "auth-secret", envOr("RSSFETCHER_AUTH_SECRET", ""), "secret signing CSRF tokens (generated per start when empty)")
	f.DurationVar(&c.sessionTTL, "session-ttl", 12*time.Hour, "login session lifetime, sliding while in use")
	f.BoolVar(&c.cookieSecure, "cookie-secure", false, "mark session cookies Secure (set this behind TLS)")
	f.BoolVar(&c.trustProxy, "trust-proxy", false, "take the client address from X-Forwarded-For; only enable behind a proxy that sets it")
}

// resolveDatabase decides which database the app works on. An explicit
// -database flag is only used when the connection string does not name one:
// the start scripts document MONGO_URI as including the database, so the URI
// has to win over the flag default. "admin" is what an unauthenticated connect
// string resolves to, never an intentional choice.
func resolveDatabase(uri, database string) string {
	if name := uriDatabase(uri); name != "" && name != "admin" {
		return name
	}
	if database != "" {
		return database
	}
	return mongo.DefaultDatabase
}

// uriDatabase returns the path component of a mongodb:// or mongodb+srv:// URI.
func uriDatabase(uri string) string {
	// Both schemes parse as-is; anything that is not a URI (a bare
	// "localhost:27017", say) simply has no database to name.
	if !strings.HasPrefix(uri, "mongodb://") && !strings.HasPrefix(uri, "mongodb+srv://") {
		return ""
	}
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	// A query string belongs to the client options, not the database name.
	return strings.Trim(u.Path, "/")
}

// reportMissingAdminUser handles a start without RSSFETCHER_ADMIN_USER. It is
// not fatal: the fetcher has no business logging in anywhere, and an install
// that already has accounts in usercollection needs no flag at all. With zero
// accounts, though, the admin UI would be silently unreachable - so that case
// is spelled out.
func reportMissingAdminUser(ctx context.Context, store adminUserStore) error {
	exists, err := store.HasAdminUsers(ctx)
	if err != nil {
		return err
	}
	if exists {
		log.Printf("rssfetcher: no admin username configured; using the accounts already in the database")
		return nil
	}
	log.Printf("rssfetcher: WARNING no admin account exists, and RSSFETCHER_ADMIN_USER (or -admin-user) is not set")
	log.Printf("rssfetcher:   the admin UI is unreachable until one is configured, e.g.")
	log.Printf("rssfetcher:     RSSFETCHER_ADMIN_USER=ops RSSFETCHER_ADMIN_PASSWORD='...' ./rssfetcher")
	log.Printf("rssfetcher:   feed fetching continues to work without an admin account")
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	cfg := config{}
	fs := flag.NewFlagSet("rssfetcher", flag.ExitOnError)
	registerFlags(fs, &cfg)
	// Single parse of a dedicated flag set: os.Args is no longer parsed twice
	// and a nil FlagSet panic is off the table.
	if err := fs.Parse(os.Args[1:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := mongo.InitMongoClient(ctx, cfg.mongoURI)
	if err != nil {
		log.Fatalf("starting up: %v", err)
	}
	if warning := cfg.csrfSecretWarning(); warning != "" {
		log.Println("rssfetcher: " + warning)
	}

	cfg.applyFetchPolicy()
	if err := cfg.validate(); err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}

	database := resolveDatabase(cfg.mongoURI, cfg.database)
	store := mongo.NewStore(client, database)
	store.SetHonourFeedTTL(cfg.keepFeedTTL)
	store.SetMaxFeedTTL(cfg.maxFeedTTL)

	// Bind the listen address before anything is written to the database: a
	// stale instance still holding the port used to make this process seed an
	// admin account (and print a one-time password) into the wrong database
	// before failing on ListenAndServe.
	listener, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		log.Fatalf("listening on %s: %v", cfg.addr, err)
	}

	// Indexes make the fetcher correct (unique rssGuid) and fast. A failure
	// here is only logged: the app still works against an existing database
	// whose documents violate the new unique constraints.
	if err := store.EnsureIndexes(ctx); err != nil {
		log.Printf("creating indexes: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Disconnect(ctx); err != nil {
			log.Printf("disconnecting from mongo: %v", err)
		}
	}()

	templates, err := handler.LoadTemplates(cfg.templateDir)
	if err != nil {
		log.Fatalf("loading templates: %v", err)
	}

	// Building the handler can fail: the CSRF secret is derived from crypto/rand,
	// and a process that cannot produce random bytes must not serve a form whose
	// tokens are guessable.
	rootHandler, err := middleware(cfg, store, templates)
	if err != nil {
		log.Fatalf("setting up the web UI: %v", err)
	}

	if err := seedAdminUser(ctx, store, cfg); err != nil {
		log.Fatalf("seeding the admin account: %v", err)
	}

	// The fetch loop gets its own cancellable context so shutdown can wait for an
	// in-flight cycle instead of disconnecting the client underneath it.
	fetchCtx, stopFetcher := context.WithCancel(ctx)
	var fetcher sync.WaitGroup
	fetcher.Add(1)
	go func() {
		defer stopFetcher()
		defer fetcher.Done()
		runFeedFetcher(fetchCtx, store, cfg.fetchOptions())
	}()

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           rootHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("rssfetcher: listening on %s (env=%s, db=%s)", cfg.addr, cfg.env, store.Database())
		// The listener is already bound, so a port conflict is reported before
		// any database writes happen.
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		if err != nil {
			log.Fatalf("serving: %v", err)
		}
	case <-ctx.Done():
		log.Println("shutdown requested")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}

	// Let the fetch cycle that may be mid bulk write finish (its own deadline
	// bounds how long this takes) before the deferred client.Disconnect runs.
	done := make(chan struct{})
	go func() { fetcher.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(cfg.cycleTimeout()):
		log.Println("rssfetcher: fetcher did not stop within its cycle deadline")
	}
	log.Println("rssfetcher: stopped")
}

// handlerOptions maps the process configuration onto the admin UI options. A
// method rather than an inline literal, so an option cannot be added to one side
// and quietly forgotten on the other.
func (c config) handlerOptions() handler.Options {
	return handler.Options{
		SessionTTL:     c.sessionTTL,
		CookieSecure:   c.cookieSecure,
		AuthSecret:     c.authSecret,
		TrustProxy:     c.trustProxy,
		RefetchTimeout: c.checkTimeout,
		// The UI shows when a feed will be polled again, so it needs the same
		// policy the loop applies.
		HonourFeedTTL: c.keepFeedTTL,
		MaxFeedTTL:    c.maxFeedTTL,
	}
}

// middleware builds the handler chain, outermost first. Named rather than inlined
// because the four layers of wrapping parentheses were unreadable, and the order
// matters: the timeout bounds everything, access logging sees the final status,
// recovery must wrap the handlers, and the security headers must also reach
// responses produced by the other three.
func middleware(cfg config, store *mongo.Store, templates *template.Template) (http.Handler, error) {
	app, err := handler.New(store, templates, cfg.handlerOptions())
	if err != nil {
		return nil, err
	}
	routes := app.Routes()
	return handler.WithTimeout(15*time.Second,
		handler.AccessLog(
			handler.Recoverer(
				handler.SecurityHeaders(cfg.cookieSecure)(routes)))), nil
}

// feedRefresher is the part of the store the background fetcher needs; it lets
// the loop be tested without a database.
type feedRefresher interface {
	Feeds(ctx context.Context, onlyActive bool) ([]domain.Feed, error)
	FetchNews(ctx context.Context, feeds []domain.Feed, fetcher rss.FetchFunc) (int, error)
}

// adminUserStore is what the seeding needs from the database; small enough to
// fake in a test.
type adminUserStore interface {
	AdminUserByName(ctx context.Context, username string) (domain.AdminUser, error)
	EnsureAdminUser(ctx context.Context, username, password string) (bool, error)
	HasAdminUsers(ctx context.Context) (bool, error)
}

// seedAdminUser creates the configured admin account, hashing its password with
// scrypt. It never overwrites an existing account: rotating a password is an
// explicit action through the admin UI.
func seedAdminUser(ctx context.Context, store adminUserStore, cfg config) error {
	username := strings.TrimSpace(cfg.adminUser)
	if username == "" {
		return reportMissingAdminUser(ctx, store)
	}

	password, generated, err := adminPasswordToSeed(ctx, store, username, cfg.adminPassword)
	if err != nil {
		return err
	}
	if password == "" {
		// The account already exists and its password is unknown to us by
		// design: nothing to do, and nothing to print.
		return nil
	}

	created, err := store.EnsureAdminUser(ctx, username, password)
	if err != nil {
		return fmt.Errorf("ensuring admin user %q: %w", username, err)
	}
	if !created {
		return nil
	}
	log.Printf("rssfetcher: created admin user %q; only its scrypt hash is stored, never the password", username)
	if generated {
		// The only place a generated credential is ever written down: the
		// process log, once, at creation time.
		log.Printf("rssfetcher:   password: %s", password)
		log.Printf("rssfetcher: shown once and not recoverable from the database; change it in the admin UI, or set -admin-password / RSSFETCHER_ADMIN_PASSWORD")
	}
	return nil
}

// adminPasswordToSeed returns the password to create the account with, and
// whether it was generated rather than configured. A random credential is only
// minted when there is an account to create.
func adminPasswordToSeed(ctx context.Context, store adminUserStore, username, configured string) (string, bool, error) {
	if configured != "" {
		return configured, false, nil
	}

	existing, err := store.AdminUserByName(ctx, username)
	switch {
	case err == nil && existing.Username != "":
		// The account exists; its password is unknown to us by design.
		return "", false, nil
	case err != nil && !errors.Is(err, mongo.ErrNotFound):
		return "", false, fmt.Errorf("looking up the admin account: %w", err)
	}

	password, err := auth.GeneratePassword()
	if err != nil {
		return "", false, err
	}
	return password, true, nil
}

// fetchOptions configures the fetch loop. A struct instead of four positional
// arguments, because the previous signature (ctx, store, interval, on bool) said
// nothing at the call site.
type fetchOptions struct {
	interval     time.Duration
	jitter       time.Duration
	cycleTimeout time.Duration
	fetchOnStart bool
	fetcher      rss.FetchFunc
}

// runFeedFetcher refreshes every active feed on a ticker. The ticker is stopped
// on shutdown (time.Tick leaked it), one slow or panicking cycle cannot stop the
// loop, and the function does not return while a cycle is still writing: main
// disconnects the mongo client right after this returns.
func runFeedFetcher(ctx context.Context, store feedRefresher, opts fetchOptions) {
	if opts.interval <= 0 {
		opts.interval = prodFetchInterval
	}
	if opts.cycleTimeout <= 0 {
		opts.cycleTimeout = defaultFetchTimeout
	}
	if opts.fetcher == nil {
		opts.fetcher = rss.Fetch
	}
	log.Printf("rssfetcher: refreshing feeds every %s (+/- %s), cycle deadline %s", opts.interval, opts.jitter, opts.cycleTimeout)

	var running sync.WaitGroup
	defer running.Wait()

	refresh := func() {
		running.Add(1)
		defer running.Done()
		if _, err := runFetchCycle(ctx, store, opts); err != nil {
			log.Printf("rssfetcher: fetch cycle: %v", err)
		}
	}

	if opts.fetchOnStart {
		refresh()
	}
	for {
		wait := jitteredInterval(opts.interval, opts.jitter, rand.New(rand.NewSource(time.Now().UnixNano())))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Println("rssfetcher: feed fetcher stopped")
			return
		case <-timer.C:
			refresh()
		}
	}
}

// jitteredInterval spreads cycles so a restart does not hit every publisher at
// the same second. The randomness is injected so the test can pin the range.
func jitteredInterval(interval, jitter time.Duration, rng *rand.Rand) time.Duration {
	if jitter <= 0 || interval <= jitter {
		return interval
	}
	return interval + time.Duration(rng.Int63n(int64(2*jitter))) - jitter
}

// runFetchCycle performs one refresh and returns how many items were written.
//
// The cycle deadline is its own budget, independent of the interval, and it is
// deliberately detached from the parent context (context.WithoutCancel): when the
// process shuts up shop, in-flight bulk writes should finish rather than be
// cancelled halfway, which used to leave a feed half stored.
func runFetchCycle(ctx context.Context, store feedRefresher, opts fetchOptions) (int, error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("rssfetcher: feed cycle panicked: %v", r)
		}
	}()
	cycleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opts.cycleTimeout)
	defer cancel()

	feeds, err := store.Feeds(cycleCtx, true)
	if err != nil {
		return 0, fmt.Errorf("listing feeds: %w", err)
	}
	if len(feeds) == 0 {
		return 0, nil
	}
	start := time.Now()
	n, err := store.FetchNews(cycleCtx, feeds, opts.fetcher)
	log.Printf("rssfetcher: %d feeds, %d items written in %s", len(feeds), n, time.Since(start).Round(time.Millisecond))
	if err != nil {
		return n, err
	}
	return n, nil
}
