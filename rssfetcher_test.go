package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jelinden/rssfetcher/app/auth"
	"github.com/jelinden/rssfetcher/app/domain"
	"github.com/jelinden/rssfetcher/app/mongo"
	"github.com/jelinden/rssfetcher/app/rss"
)

func TestConfigInterval(t *testing.T) {
	if got := (config{env: "dev"}).interval(); got != devFetchInterval {
		t.Errorf("dev interval = %s, want %s", got, devFetchInterval)
	}
	if got := (config{env: "prod"}).interval(); got != prodFetchInterval {
		t.Errorf("prod interval = %s, want %s", got, prodFetchInterval)
	}
	if got := (config{env: "prod", fetchEvery: 5 * time.Second}).interval(); got != 5*time.Second {
		t.Errorf("explicit interval = %s, want 5s", got)
	}
}

func TestFlagsParseOnceAndHaveValidDefaults(t *testing.T) {
	cfg := config{}
	fs := flag.NewFlagSet("rssfetcher", flag.ContinueOnError)
	registerFlags(fs, &cfg)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse defaults: %v", err)
	}
	// The default address has to be usable as-is; "localhost" made the process
	// die with a nil client panic.
	if !mongo.ValidURI(cfg.mongoURI) {
		t.Errorf("default -address %q is not a mongo URI", cfg.mongoURI)
	}
	if err := fs.Parse([]string{"-address", "mongodb://user:pw@host:27017/news", "-env", "prod", "-listen", ":9999"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	if cfg.mongoURI != "mongodb://user:pw@host:27017/news" || cfg.env != "prod" || cfg.addr != ":9999" {
		t.Errorf("flags not applied: %+v", cfg)
	}
	// Templates are embedded, so no directory is required to boot.
	if cfg.templateDir != "" {
		t.Errorf("-templates default = %q, want the embedded copies", cfg.templateDir)
	}
}

// Neither half of the login may be baked in: the username has to come from the
// environment or the command line, and the password from there too (or be
// generated on the one start that creates the account).
func TestAdminCredentialsHaveNoDefaults(t *testing.T) {
	t.Setenv("RSSFETCHER_ADMIN_USER", "")
	t.Setenv("RSSFETCHER_ADMIN_PASSWORD", "")

	cfg := config{}
	fs := flag.NewFlagSet("rssfetcher", flag.ContinueOnError)
	registerFlags(fs, &cfg)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if cfg.adminUser != "" {
		t.Errorf("-admin-user default = %q, want empty", cfg.adminUser)
	}
	if cfg.adminPassword != "" {
		t.Errorf("-admin-password default = %q, want empty", cfg.adminPassword)
	}
	// The declared defaults, not just the parsed values: a default hidden inside
	// registerFlags would be invisible to the check above only if env vars were
	// set, so assert on the flag definitions themselves as well.
	for _, name := range []string{"admin-user", "admin-password"} {
		if def := fs.Lookup(name).DefValue; def != "" {
			t.Errorf("flag -%s declares default %q, want none", name, def)
		}
	}
	if cfg.sessionTTL <= 0 {
		t.Errorf("-session-ttl = %s", cfg.sessionTTL)
	}
}

func TestAdminCredentialsCanBeProvidedByEnvironment(t *testing.T) {
	t.Setenv("RSSFETCHER_ADMIN_USER", "operator")
	t.Setenv("RSSFETCHER_ADMIN_PASSWORD", "from-the-environment")
	t.Setenv("RSSFETCHER_AUTH_SECRET", "static-csrf-secret")

	cfg := config{}
	fs := flag.NewFlagSet("rssfetcher", flag.ContinueOnError)
	registerFlags(fs, &cfg)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if cfg.adminUser != "operator" || cfg.adminPassword != "from-the-environment" || cfg.authSecret != "static-csrf-secret" {
		t.Errorf("environment was not picked up: %+v", cfg)
	}
	// Command line wins over the environment.
	if err := fs.Parse([]string{"-admin-user", "explicit"}); err != nil {
		t.Fatal(err)
	}
	if cfg.adminUser != "explicit" {
		t.Errorf("-admin-user = %q, want the flag to win", cfg.adminUser)
	}
}

type fakeRefresher struct {
	mu       sync.Mutex
	cycles   int
	feeds    []domain.Feed
	feedsErr error
	onCycle  func()
}

func (f *fakeRefresher) Feeds(ctx context.Context, onlyActive bool) ([]domain.Feed, error) {
	f.mu.Lock()
	f.cycles++
	f.mu.Unlock()
	if f.feedsErr != nil {
		return nil, f.feedsErr
	}
	return f.feeds, nil
}

func (f *fakeRefresher) FetchNews(ctx context.Context, feeds []domain.Feed, fetcher rss.FetchFunc) (int, error) {
	f.mu.Lock()
	f.cycles++
	f.mu.Unlock()
	if f.onCycle != nil {
		f.onCycle()
	}
	return 0, nil
}

func (f *fakeRefresher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cycles
}

// The fetcher loop must stop when the context is cancelled (time.Tick leaked
// its ticker and nothing ever stopped the loop).
func TestRunFeedFetcherStopsOnContextCancel(t *testing.T) {
	refresher := &fakeRefresher{feeds: []domain.Feed{{URL: "http://example.com/feed"}}}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runFeedFetcher(ctx, refresher, fetchOptions{interval: 10 * time.Millisecond, cycleTimeout: time.Second, fetchOnStart: true})
		close(done)
	}()

	waitForCycles(t, refresher, 2)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runFeedFetcher did not stop after its context was cancelled")
	}
}

// A panicking or erroring cycle must not kill the loop.
func TestRunFeedFetcherSurvivesPanickingCycle(t *testing.T) {
	var panics int32
	refresher := &fakeRefresher{feeds: []domain.Feed{{URL: "http://example.com/feed"}}}
	refresher.onCycle = func() {
		if atomic.AddInt32(&panics, 1) == 1 {
			panic("first cycle explodes")
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runFeedFetcher(ctx, refresher, fetchOptions{interval: 10 * time.Millisecond, cycleTimeout: time.Second})

	waitForCycles(t, refresher, 3)
	if got := refresher.count(); got < 3 {
		t.Fatalf("cycles = %d, want the loop to keep running after a panic", got)
	}
}

func TestRunFeedFetcherReportsStoreErrors(t *testing.T) {
	refresher := &fakeRefresher{feedsErr: errors.New("mongo down")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		runFeedFetcher(ctx, refresher, fetchOptions{interval: 10 * time.Millisecond, cycleTimeout: time.Second, fetchOnStart: true})
		close(done)
	}()
	waitForCycles(t, refresher, 2)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runFeedFetcher did not stop after a store error")
	}
}

func waitForCycles(t *testing.T, f *fakeRefresher, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f.count() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("only %d cycles ran, want %d", f.count(), want)
}

// The start scripts document MONGO_URI as "including the database", and the
// original code honoured that. A URI that names a database must therefore win
// over the -database default, otherwise every documented start script writes
// into "news" no matter what the connection string says.
func TestDatabaseIsTakenFromTheConnectionURI(t *testing.T) {
	tests := []struct {
		uri      string
		database string // -database value passed on the command line
		want     string
	}{
		{"mongodb://127.0.0.1:27017/local_smoke", "", "local_smoke"},
		{"mongodb://127.0.0.1:27017/news", "", "news"},
		{"mongodb://user:pw@host:27017/db1?replicaSet=rs0", "", "db1"},
		{"mongodb+srv://user:pw@cluster.example.com/prod", "", "prod"},
		{"mongodb://127.0.0.1:27017/explicit", "fromflag", "explicit"},
		{"mongodb://127.0.0.1:27017", "", mongo.DefaultDatabase},
		{"mongodb://127.0.0.1:27017/", "", mongo.DefaultDatabase},
		{"mongodb://127.0.0.1:27017/admin", "", mongo.DefaultDatabase},
		{"localhost:27017", "", mongo.DefaultDatabase},
		{"localhost:27017", "fromflag", "fromflag"},
	}
	for _, tc := range tests {
		if got := resolveDatabase(tc.uri, tc.database); got != tc.want {
			t.Errorf("resolveDatabase(%q, %q) = %q, want %q", tc.uri, tc.database, got, tc.want)
		}
	}
}

// fakeAdminStore records what the seeder did; it stands in for the mongo store.
type fakeAdminStore struct {
	existing  domain.AdminUser
	lookupErr error
	anyExists bool
	anyErr    error

	lookups     int
	seededWith  string
	seededUser  string
	seedCreated bool
}

func (f *fakeAdminStore) AdminUserByName(context.Context, string) (domain.AdminUser, error) {
	f.lookups++
	if f.lookupErr != nil {
		return domain.AdminUser{}, f.lookupErr
	}
	return f.existing, nil
}

func (f *fakeAdminStore) EnsureAdminUser(_ context.Context, username, password string) (bool, error) {
	f.seededWith = password
	f.seededUser = username
	f.seedCreated = true
	return true, nil
}

func (f *fakeAdminStore) HasAdminUsers(context.Context) (bool, error) {
	return f.anyExists, f.anyErr
}

// The credential must never be compiled in: with a password configured it is
// used verbatim and the database is not even consulted.
func TestSeedAdminUserUsesConfiguredPassword(t *testing.T) {
	store := &fakeAdminStore{}
	cfg := config{adminUser: "seed-admin", adminPassword: "configured-password-1"}
	if err := seedAdminUser(context.Background(), store, cfg); err != nil {
		t.Fatal(err)
	}
	if store.seededWith != "configured-password-1" || store.seededUser != "seed-admin" {
		t.Errorf("seeded %q with %q", store.seededUser, store.seededWith)
	}
	if store.lookups != 0 {
		t.Errorf("looked up the account %d times, want 0", store.lookups)
	}
}

// With no password configured, a fresh install gets a random one that passes
// the policy.
func TestSeedAdminUserGeneratesPasswordForFreshInstall(t *testing.T) {
	store := &fakeAdminStore{lookupErr: mongo.ErrNotFound}
	cfg := config{adminUser: "seed-admin"}
	if err := seedAdminUser(context.Background(), store, cfg); err != nil {
		t.Fatal(err)
	}
	if !store.seedCreated {
		t.Fatal("no account was created")
	}
	if err := auth.ValidatePassword(store.seededWith); err != nil {
		t.Fatalf("generated password: %v", err)
	}
	if store.seededWith == "" || store.seededWith == "seed-admin" {
		t.Errorf("generated password = %q", store.seededWith)
	}
	if store.seededUser != "seed-admin" {
		t.Errorf("seeded user = %q, want seed-admin", store.seededUser)
	}
}

// An account that already exists keeps its unknown password; the seeder must
// not mint or print a credential in that case.
func TestSeedAdminUserLeavesExistingAccountAlone(t *testing.T) {
	store := &fakeAdminStore{existing: domain.AdminUser{Username: "seed-admin"}}
	cfg := config{adminUser: "seed-admin"}
	if err := seedAdminUser(context.Background(), store, cfg); err != nil {
		t.Fatal(err)
	}
	if store.seedCreated {
		t.Error("the existing account was re-seeded")
	}
	if store.seededWith != "" {
		t.Errorf("seeded with %q, want no password", store.seededWith)
	}
}

func TestSeedAdminUserReportsLookupFailure(t *testing.T) {
	store := &fakeAdminStore{lookupErr: errors.New("boom")}
	if err := seedAdminUser(context.Background(), store, config{adminUser: "seed-admin"}); err == nil {
		t.Fatal("want the lookup failure to surface")
	} else if !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want it to mention boom", err)
	}
}

// Guard against a credential or a personal handle creeping back into the tree.
// The patterns are built by concatenation so this file does not match itself.
func TestTreeContainsNoSeedCredentials(t *testing.T) {
	forbidden := []string{
		"Emerge" + "009", // the password that used to be the default
		"lin" + "dejo",   // the username that used to be the default
	}

	skipExt := map[string]bool{".sum": true, ".log": true}
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		switch {
		case err != nil:
			return err
		case info.IsDir():
			// Note: the root is reported as "." itself, so a blanket
			// strings.HasPrefix(path, ".") here would skip the whole walk and
			// make this test vacuously green.
			if path == "." {
				return nil
			}
			base := filepath.Base(path)
			if base == "vendor" || base == ".git" || strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			return nil
		case skipExt[filepath.Ext(path)]:
			return nil
		}
		if path == "rssfetcher" || strings.HasSuffix(path, ".exe") || strings.HasSuffix(path, ".s") {
			return nil // built binaries and assembly
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.ContainsRune(data, 0) {
			return nil // not text
		}
		for _, want := range forbidden {
			if strings.Contains(string(data), want) {
				t.Errorf("%s contains %q", path, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Without a username there is nothing to seed, and that must not be an error:
// the fetcher works fine on its own, and an install that already has accounts
// needs no flag at all.
func TestSeedAdminUserWithoutUsername(t *testing.T) {
	tests := []struct {
		name  string
		store *fakeAdminStore
	}{
		{"accounts already exist", &fakeAdminStore{anyExists: true}},
		{"database is empty", &fakeAdminStore{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config{adminUser: "   "} // whitespace is not a username
			if err := seedAdminUser(context.Background(), tc.store, cfg); err != nil {
				t.Fatalf("seedAdminUser: %v", err)
			}
			if tc.store.seedCreated {
				t.Errorf("created %q with a password, want no seeding at all", tc.store.seededUser)
			}
			if tc.store.lookups != 0 {
				t.Errorf("looked up a blank username %d times", tc.store.lookups)
			}
		})
	}
}

func TestSeedAdminUserReportsHasAdminUsersFailure(t *testing.T) {
	store := &fakeAdminStore{anyErr: errors.New("unreachable")}
	if err := seedAdminUser(context.Background(), store, config{}); err == nil {
		t.Fatal("want the counting failure to surface")
	}
}
