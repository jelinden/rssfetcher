package main

import (
	"flag"
	"testing"
	"time"

	"github.com/jelinden/rssfetcher/app/handler"
)

// The admin UI can now fetch a single feed on demand. That request is served to a
// browser, so it needs its own budget - the cycle deadline (2 minutes by default)
// is far too long to make someone wait for, and the handler default is only
// reachable if the config reaches it.
func TestCheckTimeoutReachesTheHandler(t *testing.T) {
	cfg := config{}
	fs := flag.NewFlagSet("rssfetcher", flag.ContinueOnError)
	registerFlags(fs, &cfg)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if cfg.checkTimeout != handler.DefaultRefetchTimeout {
		t.Errorf("default -check-timeout = %v, want %v", cfg.checkTimeout, handler.DefaultRefetchTimeout)
	}
	if got := cfg.handlerOptions().RefetchTimeout; got != handler.DefaultRefetchTimeout {
		t.Errorf("handler RefetchTimeout = %v, want the default", got)
	}

	if err := fs.Parse([]string{"-check-timeout", "5s"}); err != nil {
		t.Fatal(err)
	}
	if got := cfg.handlerOptions().RefetchTimeout; got != 5*time.Second {
		t.Errorf("handler RefetchTimeout = %v, want 5s", got)
	}

	// Other handler options must keep coming from the config as before.
	cfg.sessionTTL = 3 * time.Hour
	cfg.cookieSecure = true
	cfg.trustProxy = true
	opts := cfg.handlerOptions()
	if opts.SessionTTL != 3*time.Hour || !opts.CookieSecure || !opts.TrustProxy {
		t.Errorf("handlerOptions() dropped part of the config: %+v", opts)
	}
}

// The prod cadence is the whole product: a news reader that polls every six hours
// looks broken. The rewrite raised the interval far above what the deployment
// actually did (80s) and added per-feed <ttl> honouring on top, so this pins both
// the flag and the cap that keeps a publisher from starving the others.
func TestMaxFeedTTLFlagAndValidation(t *testing.T) {
	cfg := config{}
	fs := flag.NewFlagSet("rssfetcher", flag.ContinueOnError)
	registerFlags(fs, &cfg)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if cfg.maxFeedTTL != 0 {
		t.Errorf("default -max-feed-ttl = %v, want 0 (honour the publisher exactly)", cfg.maxFeedTTL)
	}

	if err := fs.Parse([]string{"-max-feed-ttl", "15m"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.validate(); err != nil {
		t.Errorf("validate() with a cap: %v", err)
	}

	if err := fs.Parse([]string{"-max-feed-ttl", "-1m"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.validate(); err == nil {
		t.Error("a negative -max-feed-ttl was accepted")
	}
}
