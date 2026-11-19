package main

import (
	"context"
	"flag"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/jelinden/rssfetcher/app/domain"
	"github.com/jelinden/rssfetcher/app/rss"
)

// L2: the prod default of 80 seconds for every feed is aggressive enough to get
// the instance rate limited by small publishers.
func TestDefaultIntervalIsPolite(t *testing.T) {
	if got := (config{env: "prod"}).interval(); got < 10*time.Minute {
		t.Errorf("prod interval = %s, want at least 10m", got)
	}
	if got := (config{env: "dev"}).interval(); got > 2*time.Minute {
		t.Errorf("dev interval = %s, want a short but not tiny period", got)
	}
	if got := (config{env: "prod"}).jitter(); got <= 0 {
		t.Errorf("prod jitter = %s, want a nonzero spread so a restart does not synchronize all feeds", got)
	}
}

// L2: a cycle must be bounded by its own timeout. With -interval 80s and a slow
// publisher, the old code cut downloads off at the tick period; with a 15m
// interval it would let a wedged fetch run for 15 minutes.
func TestFetchCycleTimeoutIsIndependentOfInterval(t *testing.T) {
	started := make(chan struct{})
	refresher := &blockingRefresher{feeds: []domain.Feed{{URL: "https://example.com/f"}}, release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var deadline time.Time
	var mu sync.Mutex
	refresher.observe = func(c context.Context) {
		mu.Lock()
		if d, ok := c.Deadline(); ok {
			deadline = d
		}
		mu.Unlock()
		close(started)
	}

	go func() {
		// A long interval must not become the download deadline.
		_, _ = runFetchCycle(ctx, refresher, fetchOptions{cycleTimeout: 3 * time.Second})
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("cycle never started")
	}
	mu.Lock()
	got := deadline
	mu.Unlock()
	if got.IsZero() {
		t.Fatal("the cycle context has no deadline, so a wedged download blocks forever")
	}
	if remaining := time.Until(got); remaining < time.Second {
		t.Errorf("cycle deadline = %s, want the configured 3s", remaining)
	}
	close(refresher.release)
}

// L10: at shutdown the process used to disconnect the client before the fetcher
// goroutine finished, cancelling in-flight bulk writes.
func TestFetcherWaitsForTheInFlightCycle(t *testing.T) {
	refresher := &blockingRefresher{
		feeds:    []domain.Feed{{URL: "https://example.com/f"}},
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
	}
	refresher.observe = func(context.Context) { close(refresher.entered) }

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		// A long interval: the only cycle is the one started at boot.
		runFeedFetcher(ctx, refresher, fetchOptions{interval: time.Hour, cycleTimeout: 5 * time.Second, fetchOnStart: true})
		close(stopped)
	}()

	<-refresher.entered
	cancel() // shutdown begins while a cycle is in flight
	close(refresher.release)

	select {
	case <-refresher.finished:
	case <-time.After(2 * time.Second):
		t.Fatal("the in-flight cycle was cut off instead of being allowed to write")
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("runFeedFetcher returned before the cycle finished")
	}
}

// blockingRefresher lets a test hold a cycle open and observe its context.
type blockingRefresher struct {
	feeds    []domain.Feed
	observe  func(context.Context)
	entered  chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func (b *blockingRefresher) Feeds(context.Context, bool) ([]domain.Feed, error) {
	return b.feeds, nil
}

func (b *blockingRefresher) FetchNews(ctx context.Context, _ []domain.Feed, _ rss.FetchFunc) (int, error) {
	if b.observe != nil {
		b.observe(ctx)
	}
	if b.release != nil {
		<-b.release
	}
	if b.finished != nil {
		close(b.finished)
	}
	return 0, ctx.Err()
}

// L2: jitter keeps retries from synchronizing, but must stay in a sane band.
func TestJitteredIntervalStaysInRange(t *testing.T) {
	base := time.Minute
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		got := jitteredInterval(base, 15*time.Second, rng)
		if got < 45*time.Second || got > 75*time.Second {
			t.Fatalf("jitteredInterval = %s, want within +/-15s of %s", got, base)
		}
	}
	if got := jitteredInterval(base, 0, rng); got != base {
		t.Errorf("with no jitter configured = %s, want %s", got, base)
	}
}

// L11 and M6: both policies have to be reachable from the command line.
func TestFetchPolicyFlags(t *testing.T) {
	cfg := config{}
	fs := flag.NewFlagSet("rssfetcher", flag.ContinueOnError)
	registerFlags(fs, &cfg)
	if err := fs.Parse([]string{"-max-items-per-feed", "50", "-allow-private-urls", "-fetch-timeout", "30s"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.maxItems != 50 {
		t.Errorf("maxItems = %d, want 50", cfg.maxItems)
	}
	if !cfg.allowPrivateURLs {
		t.Error("-allow-private-urls not recorded")
	}
	if cfg.fetchTimeout != 30*time.Second {
		t.Errorf("fetchTimeout = %s, want 30s", cfg.fetchTimeout)
	}
	if err := fs.Parse([]string{"-max-items-per-feed", "-1"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cfg.validate(); err == nil {
		t.Error("a negative item limit passed validation")
	}
	if err := fs.Parse([]string{"-max-items-per-feed", "0"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cfg.validate(); err != nil {
		t.Errorf("0 means 'use the package default', but validate rejected it: %v", err)
	}
	if err := fs.Parse([]string{"-max-items-per-feed", "10", "-env", "staging"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cfg.validate(); err == nil {
		t.Error("an unknown -env passed validation")
	}
}

// Applying the flags must reach the packages that enforce them.
func TestApplyFetchPolicySetsTheGlobals(t *testing.T) {
	previousLimit := rss.MaxItemsPerFeed()
	previousPrivate := rss.AllowPrivateURLs()
	t.Cleanup(func() {
		rss.SetMaxItemsPerFeed(previousLimit)
		rss.SetAllowPrivateURLs(previousPrivate)
	})

	cfg := config{maxItems: 40, allowPrivateURLs: true}
	cfg.applyFetchPolicy()
	if rss.MaxItemsPerFeed() != 40 {
		t.Errorf("rss limit = %d, want 40", rss.MaxItemsPerFeed())
	}
	if !rss.AllowPrivateURLs() {
		t.Error("address guard still on after -allow-private-urls")
	}
}

// The flag help says 0 disables jitter; a zero default would have made that
// indistinguishable from "not set".
func TestJitterFlagDistinguishesZeroFromUnset(t *testing.T) {
	fs := flag.NewFlagSet("rssfetcher", flag.ContinueOnError)
	cfg := config{}
	registerFlags(fs, &cfg)
	if err := fs.Parse([]string{"-no-jitter"}); err != nil {
		t.Fatal(err)
	}
	if !cfg.noJitter {
		t.Fatal("-no-jitter not recorded")
	}
	if got := (config{env: "dev"}).jitter(); got != devFetchJitter {
		t.Errorf("dev jitter = %s, want %s", got, devFetchJitter)
	}
	if got := (config{env: "prod"}).jitter(); got != defaultFetchJitter {
		t.Errorf("prod jitter = %s, want %s", got, defaultFetchJitter)
	}
	if got := (config{env: "dev", noJitter: true}).jitter(); got != 0 {
		t.Errorf("-no-jitter = %s, want jitter disabled", got)
	}
	if got := (config{env: "dev", fetchJitter: 3 * time.Second}).jitter(); got != 3*time.Second {
		t.Errorf("-interval-jitter 3s = %s, want 3s", got)
	}

	// A jitter of the interval itself would make cycles overlap or run
	// immediately, so it is rejected.
	if err := (config{env: "dev", maxItems: 25, fetchEvery: time.Minute, fetchJitter: time.Minute}).validate(); err == nil {
		t.Error("a jitter as large as the interval passed validation")
	}
	if err := (config{env: "dev", maxItems: 25, fetchEvery: 2 * time.Minute, fetchJitter: 10 * time.Second}).validate(); err != nil {
		t.Errorf("a sane jitter was rejected: %v", err)
	}
}

// Lowering -interval used to fail startup, because the jitter that goes with the
// environment (2m in prod) was then larger than the interval and validation
// rejected the combination. Nobody asks for that: the derived default has to fit
// the schedule it was derived for, and only a jitter that was actually written
// down should be argued about.
func TestShortIntervalDoesNotContradictTheDefaultJitter(t *testing.T) {
	cfg := config{env: "prod", fetchEvery: 40 * time.Second, maxItems: 25}
	if err := cfg.validate(); err != nil {
		t.Fatalf("a short interval with the default jitter: %v", err)
	}
	j := cfg.jitter()
	if j <= 0 {
		t.Fatal("clamping removed the jitter entirely")
	}
	if j >= cfg.interval() {
		t.Errorf("jitter %s is not smaller than the interval %s", j, cfg.interval())
	}
	if j > defaultFetchJitter {
		t.Errorf("jitter %s exceeds the configured default %s", j, defaultFetchJitter)
	}

	// An explicit jitter is a request, not a default, so nonsense is still refused.
	explicit := config{env: "prod", fetchEvery: 40 * time.Second, fetchJitter: 5 * time.Minute, maxItems: 25}
	if err := explicit.validate(); err == nil {
		t.Error("-interval-jitter larger than -interval was accepted")
	}
	if got := (config{env: "prod", fetchEvery: 10 * time.Second, fetchJitter: 5 * time.Second}).jitter(); got != 5*time.Second {
		t.Errorf("explicit jitter was clamped to %s, want it kept", got)
	}
}
