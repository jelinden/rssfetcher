package handler

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jelinden/rssfetcher/app/domain"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// The fetch cycle logs a broken feed and forgets it. An operator looking at the
// UI cannot tell a live subscription from one that has been 404ing for a month,
// so the row itself has to carry the status.
func TestViewShowsFeedStatus(t *testing.T) {
	store := newFakeStore()
	broken := seedFeed(store, domain.Feed{
		Name:        "Dead feed",
		URL:         "http://example.com/dead.xml",
		LastError:   "rss: got status 404 for http://example.com/dead.xml",
		LastErrorAt: time.Now().Add(-2 * time.Hour),
		Failures:    14,
		OKAt:        time.Now().Add(-24 * time.Hour),
	})
	healthy := seedFeed(store, domain.Feed{
		Name:      "Live feed",
		URL:       "http://example.com/live.xml",
		OKAt:      time.Now().Add(-3 * time.Minute),
		LastItems: 5,
	})
	fresh := seedFeed(store, domain.Feed{Name: "Never polled", URL: "http://example.com/new.xml"})

	_ = broken
	_ = healthy
	_ = fresh

	body := authedGet(t, store, "/view/")
	if !strings.Contains(body, "rss: got status 404") {
		t.Error("the feed list does not show the error text an operator needs to fix the URL")
	}
	if !strings.Contains(body, "class=\"status broken\"") {
		t.Error("the failing feed has no broken badge")
	}
	if !strings.Contains(body, "14") {
		t.Error("the failing feed does not say how many consecutive attempts failed")
	}
	if !strings.Contains(body, "class=\"status ok\"") {
		t.Error("the healthy feed has no ok status")
	}
	// "0 items" would be a lie about a feed that has ten articles stored: the
	// count is of what the last fetch wrote.
	if strings.Contains(body, "0 items") {
		t.Error("the row counts stored items instead of new ones")
	}
	if !strings.Contains(body, "never checked") {
		t.Error("a feed that was never polled is not distinguished from a checked one")
	}
	// The summary at the top is what makes a broken feed impossible to miss in
	// a list of seventy.
	if !strings.Contains(body, "7 broken") && !strings.Contains(body, "1 broken") {
		t.Error("no count of broken feeds in the header")
	}
}

// With seventy one feeds, the ones that need attention have to be listable on
// their own.
func TestViewCanFilterToBrokenFeeds(t *testing.T) {
	store := newFakeStore()
	seedFeed(store, domain.Feed{Name: "Broken one", URL: "http://example.com/broken.xml", LastError: "status 404", LastErrorAt: time.Now()})
	seedFeed(store, domain.Feed{Name: "Healthy one", URL: "http://example.com/ok.xml", OKAt: time.Now()})

	filtered := authedGet(t, store, "/view/?status=broken")
	if strings.Contains(filtered, "Healthy one") {
		t.Error("?status=broken still lists healthy feeds")
	}
	if !strings.Contains(filtered, "Broken one") {
		t.Error("?status=broken does not list the broken feed")
	}

	all := authedGet(t, store, "/view/")
	for _, want := range []string{"Broken one", "Healthy one"} {
		if !strings.Contains(all, want) {
			t.Errorf("the unfiltered list is missing %s", want)
		}
	}

	// A filter name we do not know is not a reason to show nothing.
	unknown := authedGet(t, store, "/view/?status=potato")
	if !strings.Contains(unknown, "Healthy one") || !strings.Contains(unknown, "Broken one") {
		t.Error("an unknown status filter hid feeds")
	}
}

// A removed feed is not polled, so showing it as broken would be noise; the
// status is only interesting for active subscriptions.
func TestRemovedFeedDoesNotClaimToBeBroken(t *testing.T) {
	store := newFakeStore()
	seedFeed(store, domain.Feed{Name: "Deleted while broken", URL: "http://example.com/gone.xml", Removed: true, LastError: "status 404"})

	body := authedGet(t, store, "/view/?status=broken")
	if strings.Contains(body, "Deleted while broken") {
		t.Error("a removed feed appears in the broken list")
	}
}

// "Check now" is what closes the loop: fix the URL, press the button, see whether
// the publisher answers. Without it the operator waits for the next cycle.
func TestCheckHandlerRefetchesOneFeed(t *testing.T) {
	store := newFakeStore()
	id := seedFeed(store, domain.Feed{Name: "Retry me", URL: "http://example.com/retry.xml", LastError: "status 404", Failures: 3})

	c := authenticatedClient(t, store)
	rec := c.post("/check/"+id.Hex(), nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if store.checkCalls != 1 || store.checkID != id {
		t.Fatalf("FetchOne called %d times for %v, want once for %v", store.checkCalls, store.checkID, id)
	}
	location := rec.Header().Get("Location")
	if !strings.Contains(location, "checked") {
		t.Fatalf("flash = %q, want it to report what the check did", location)
	}
	decoded, err := url.QueryUnescape(location)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(decoded, "checked Retry me: 2 items") {
		t.Errorf("flash = %q, want the number of items stored", decoded)
	}
}

// The check failed: the flash must carry the reason, because that is what the
// operator reads before deciding to delete the subscription.
func TestCheckHandlerReportsAFailingFeed(t *testing.T) {
	store := newFakeStore()
	store.checkErr = errors.New("rss: got status 404 for http://example.com/retry.xml")
	id := seedFeed(store, domain.Feed{Name: "Still broken", URL: "http://example.com/retry.xml"})

	c := authenticatedClient(t, store)
	rec := c.post("/check/"+id.Hex(), nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 even when the feed fails", rec.Code)
	}
	decoded, _ := url.QueryUnescape(rec.Header().Get("Location"))
	if !strings.Contains(decoded, "404") {
		t.Errorf("flash = %q, want the fetch error quoted", decoded)
	}
}

func TestCheckHandlerIsAPostOnlyStateChange(t *testing.T) {
	store := newFakeStore()
	id := seedFeed(store, domain.Feed{Name: "Check", URL: "http://example.com/c.xml"})
	c := authenticatedClient(t, store)

	if rec := c.get("/check/" + id.Hex()); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /check = %d, want 405: a fetch costs a request to the publisher", rec.Code)
	}
	// Without a token it must not reach the store at all.
	if rec := c.postRaw("/check/"+id.Hex(), ""); rec.Code != http.StatusForbidden {
		t.Errorf("untokened POST /check = %d, want 403", rec.Code)
	}
	if store.checkCalls != 0 {
		t.Errorf("the store was called %d times without a valid request", store.checkCalls)
	}
	if rec := c.post("/check/"+bson.NewObjectID().Hex(), nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown feed = %d, want 404", rec.Code)
	}
	if rec := c.post("/check/garbage", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed id = %d, want 400", rec.Code)
	}
}

// A manual fetch talks to a remote host, so it cannot be allowed to hold a
// request open for as long as a publisher decides to stall: the handler must
// impose its own deadline, and a stuck feed must not hang the admin UI.
func TestCheckHandlerBoundsTheFetch(t *testing.T) {
	store := newFakeStore()
	store.checkBlocks = true // the fake only returns when its context is done
	id := seedFeed(store, domain.Feed{Name: "Slow", URL: "http://example.com/slow.xml"})

	done := make(chan int, 1)
	go func() {
		c := authenticatedClientWithOptions(t, store, func(o *Options) {
			o.RefetchTimeout = 80 * time.Millisecond
		})
		done <- c.post("/check/"+id.Hex(), nil).Code
	}()
	select {
	case code := <-done:
		// A timed out check is still an answer the operator reads, not a 500.
		if code != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303 with a timeout in the flash", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the check request was not bounded by the configured timeout")
	}
	if store.checkDeadline.IsZero() {
		t.Fatal("FetchOne was called with a context that has no deadline")
	}
	if until := time.Until(store.checkDeadline); until > time.Second {
		t.Errorf("deadline is %v away, want the configured 80ms", until)
	}
}

// The default must be sane even when nothing is configured, because the zero
// value of Options is what a caller gets.
func TestCheckHandlerDefaultTimeoutIsBounded(t *testing.T) {
	store := newFakeStore()
	id := seedFeed(store, domain.Feed{Name: "Quick", URL: "http://example.com/q.xml"})
	c := authenticatedClientWithOptions(t, store, func(o *Options) { o.RefetchTimeout = 0 })
	c.post("/check/"+id.Hex(), nil)
	if store.checkDeadline.IsZero() {
		t.Fatal("no deadline was imposed with the default options")
	}
	if until := time.Until(store.checkDeadline); until > 5*time.Minute {
		t.Errorf("default deadline is %v, want minutes at most", until)
	}
}

// The edit form is where a wrong URL gets fixed, so the reason it is wrong
// belongs on the same screen.
func TestEditPageShowsTheStatus(t *testing.T) {
	store := newFakeStore()
	id := seedFeed(store, domain.Feed{
		Name:        "Entity trouble",
		URL:         "http://example.com/entities.xml",
		LastError:   "XML syntax error on line 11: invalid character entity &auml;",
		LastErrorAt: time.Now(),
		Failures:    2,
	})
	body := authedGet(t, store, "/edit/"+id.Hex())
	if !strings.Contains(body, "invalid character entity") {
		t.Error("the edit page does not show why the feed is failing")
	}
	if !strings.Contains(body, "class=\"status broken\"") {
		t.Error("the edit page has no broken badge")
	}
}

// authedGet performs the login dance and returns the body of a GET.
func authedGet(t *testing.T, store *fakeStore, path string) string {
	t.Helper()
	c := authenticatedClient(t, store)
	rec := c.get(path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (body=%s)", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// A manual check of a feed that answers but has nothing new must not report
// "0 items", which reads like an empty feed.
func TestCheckHandlerSaysNoNewItems(t *testing.T) {
	store := newFakeStore()
	store.checkItems = 0
	id := seedFeed(store, domain.Feed{Name: "Unchanged", URL: "http://example.com/u.xml"})
	c := authenticatedClient(t, store)

	rec := c.post("/check/"+id.Hex(), nil)
	decoded, _ := url.QueryUnescape(rec.Header().Get("Location"))
	if !strings.Contains(decoded, "no new items") {
		t.Errorf("flash = %q, want it to say there was nothing new", decoded)
	}

	store.checkUnchanged = true
	rec = c.post("/check/"+id.Hex(), nil)
	decoded, _ = url.QueryUnescape(rec.Header().Get("Location"))
	if !strings.Contains(decoded, "nothing has changed") {
		t.Errorf("flash = %q, want the 304 explained as such", decoded)
	}
}

// The other half of "why is this feed not updating": a row that was fetched an hour
// ago and has not moved looks broken, but with the publisher asking for an hour it
// is simply not due yet. The list has to say so, using the same policy the loop
// uses (including -max-feed-ttl).
func TestViewExplainsThePollingWindow(t *testing.T) {
	store := newFakeStore()
	seedFeed(store, domain.Feed{
		Name:      "Hourly publisher",
		URL:       "http://example.com/hourly.xml",
		OKAt:      time.Now().Add(-time.Minute),
		FetchedAt: time.Now().Add(-time.Minute),
		TTL:       time.Hour,
		LastItems: 3,
	})

	bodies := map[string]string{}
	c := authenticatedClientWithOptions(t, store, func(o *Options) {
		o.HonourFeedTTL = true
	})
	bodies["honoured"] = bodyOf(t, c, "/view/")
	if !strings.Contains(bodies["honoured"], "next poll in 59m") {
		t.Error("with -honour-feed-ttl the row does not say when the feed is fetched again")
	}

	// The cap changes what the operator should expect, so it has to be reflected.
	c = authenticatedClientWithOptions(t, store, func(o *Options) {
		o.HonourFeedTTL = true
		o.MaxFeedTTL = 15 * time.Minute
	})
	body := bodyOf(t, c, "/view/")
	if !strings.Contains(body, "next poll in 14m") {
		t.Errorf("the row does not honour -max-feed-ttl: %s", between(body, "Hourly publisher", "edit"))
	}

	// With ttl skipping off the feed is polled on every cycle, and saying "next
	// poll in ..." would be a lie.
	c = authenticatedClientWithOptions(t, store, func(o *Options) { o.HonourFeedTTL = false })
	body = bodyOf(t, c, "/view/")
	if strings.Contains(body, "next poll in") {
		t.Error("the row promises a polling window that is not configured")
	}
}

// A feed whose window has passed is due now, and that needs no announcement.
func TestViewStaysQuietAboutDueFeeds(t *testing.T) {
	store := newFakeStore()
	seedFeed(store, domain.Feed{
		Name: "Overdue", URL: "http://example.com/over.xml",
		FetchedAt: time.Now().Add(-2 * time.Hour), TTL: time.Hour, OKAt: time.Now().Add(-2 * time.Hour),
	})
	c := authenticatedClientWithOptions(t, store, func(o *Options) { o.HonourFeedTTL = true })
	if body := bodyOf(t, c, "/view/"); strings.Contains(body, "next poll in") {
		t.Error("a due feed is described as being held back")
	}
}

func bodyOf(t *testing.T, c *client, path string) string {
	t.Helper()
	rec := c.get(path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", path, rec.Code)
	}
	return rec.Body.String()
}

func between(body, start, end string) string {
	i := strings.Index(body, start)
	if i < 0 {
		return body
	}
	rest := body[i:]
	if j := strings.Index(rest, end); j > 0 {
		return rest[:j]
	}
	return rest[:min(len(rest), 200)]
}
