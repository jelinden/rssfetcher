package mongo

import (
	"os"
	"testing"

	"github.com/jelinden/rssfetcher/app/rss"
)

// TestMain switches off the private address guard of the fetcher: every test in
// this package downloads from an httptest server, which listens on loopback by
// definition. The guard is covered by its own tests in app/rss.
func TestMain(m *testing.M) {
	rss.SetAllowPrivateURLs(true)
	os.Exit(m.Run())
}
