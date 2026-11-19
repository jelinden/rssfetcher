package mongo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jelinden/rssfetcher/app/domain"
	"github.com/jelinden/rssfetcher/app/rss"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// seedFeedDoc inserts a feed document and returns it, so the status methods have
// something real to update. The other fetch tests pass feeds inline; recording a
// status needs a row in feedcollection.
func seedFeedDoc(t *testing.T, store *Store, feed domain.Feed) domain.Feed {
	t.Helper()
	feed.ID = bson.NewObjectID()
	in := feedInput(feed)
	if err := store.SaveFeed(context.Background(), nil, in); err != nil {
		t.Fatal(err)
	}
	created, err := store.Feed(context.Background(), in.ID.Hex())
	if err != nil {
		t.Fatal(err)
	}
	return *created
}

// countFeedNews is the news side of the same story: a failed fetch must not take
// the stored items with it.
// countFeedNews counts the stored items of one feed. Items point at their feed
// by title (rssSource) and are keyed by rssGuid, so the guid is the stable thing
// to select on here.
func countFeedNews(t *testing.T, store *Store, guid string) int {
	t.Helper()
	n, err := store.collection(newsCollection).CountDocuments(context.Background(), bson.M{"rssGuid": guid})
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

// The fetch cycle already knew which feeds were broken and threw that away: the
// only place it could be seen was the server log. Each attempt has to land on
// the feed document so the admin UI can list it.
func TestRecordFetchSuccessClearsTheError(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	feed := seedFeedDoc(t, store, domain.Feed{Name: "healthy", URL: "http://127.0.0.1:1/ok.xml"})

	// First it failed twice...
	if err := store.RecordFetch(ctx, feed.ID, FetchOutcome{Err: errors.New("rss: got status 404")}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordFetch(ctx, feed.ID, FetchOutcome{Err: errors.New("rss: got status 404")}); err != nil {
		t.Fatal(err)
	}
	// ...then it worked.
	err := store.RecordFetch(ctx, feed.ID, FetchOutcome{
		ETag:         `W/"abc"`,
		LastModified: "Mon, 02 Jan 2026 10:00:00 GMT",
		TTL:          30 * time.Minute,
		Items:        7,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.Feed(ctx, feed.ID.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if got.LastError != "" {
		t.Errorf("LastError = %q, want cleared", got.LastError)
	}
	if !got.LastErrorAt.IsZero() {
		t.Errorf("LastErrorAt = %v, want zero", got.LastErrorAt)
	}
	if got.Failures != 0 {
		t.Errorf("Failures = %d, want 0 after a success", got.Failures)
	}
	if got.ETag != `W/"abc"` || got.TTL != 30*time.Minute {
		t.Errorf("validators not stored: etag=%q ttl=%v", got.ETag, got.TTL)
	}
	if got.OKAt.IsZero() {
		t.Error("OKAt was not recorded")
	}
	if got.LastItems != 7 {
		t.Errorf("LastItems = %d, want 7", got.LastItems)
	}
}

// A failing fetch must not lose the conditional GET validators: the feed may be
// broken because of a transient 500, and the next attempt should still be
// conditional rather than a full re-download of everything.
func TestRecordFetchFailureKeepsTheValidators(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	feed := seedFeedDoc(t, store, domain.Feed{Name: "flaky", URL: "http://127.0.0.1:1/flaky.xml"})

	if err := store.RecordFetch(ctx, feed.ID, FetchOutcome{ETag: `"keep-me"`, LastModified: "Tue, 03 Feb 2026 09:00:00 GMT", TTL: time.Hour, Items: 3}); err != nil {
		t.Fatal(err)
	}
	before, err := store.Feed(ctx, feed.ID.Hex())
	if err != nil {
		t.Fatal(err)
	}

	if err := store.RecordFetch(ctx, feed.ID, FetchOutcome{Err: errors.New("rss: got status 503")}); err != nil {
		t.Fatal(err)
	}
	after, err := store.Feed(ctx, feed.ID.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if after.ETag != before.ETag || after.LastModified != before.LastModified || after.TTL != before.TTL {
		t.Errorf("a failure rewrote the validators: %+v", after)
	}
	if after.LastItems != 3 {
		t.Errorf("LastItems = %d, want the 3 of the last success", after.LastItems)
	}
	if !strings.Contains(after.LastError, "503") {
		t.Errorf("LastError = %q, want the status", after.LastError)
	}
	if after.Failures != 1 {
		t.Errorf("Failures = %d, want 1", after.Failures)
	}
	if after.LastErrorAt.IsZero() {
		t.Error("LastErrorAt was not recorded")
	}

	// The counter is what tells "newly broken" from "broken for a week", so it
	// has to keep counting up while the feed stays down.
	if err := store.RecordFetch(ctx, feed.ID, FetchOutcome{Err: errors.New("rss: got status 503")}); err != nil {
		t.Fatal(err)
	}
	after, err = store.Feed(ctx, feed.ID.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if after.Failures != 2 {
		t.Errorf("Failures = %d, want 2", after.Failures)
	}
}

// A parse error quotes the beginning of the response, which for an HTML error
// page is a couple of hundred bytes of markup. The document must stay a
// reasonable size, so long messages are cut.
func TestRecordFetchTruncatesLongErrors(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	feed := seedFeedDoc(t, store, domain.Feed{Name: "html", URL: "http://127.0.0.1:1/html.xml"})

	long := "rss: unknown feed format: " + strings.Repeat("<!DOCTYPE html><html>", 400)
	if err := store.RecordFetch(ctx, feed.ID, FetchOutcome{Err: errors.New(long)}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Feed(ctx, feed.ID.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.LastError) > MaxStatusMessageLen+8 {
		t.Errorf("stored %d bytes of error, want at most about %d", len(got.LastError), MaxStatusMessageLen)
	}
	if !strings.HasSuffix(got.LastError, "…") {
		t.Errorf("LastError = %q, want a truncation marker", got.LastError)
	}
	if !strings.HasPrefix(got.LastError, "rss: unknown feed format") {
		t.Error("truncation cut the beginning of the message, which is the part that identifies the problem")
	}
}

// Truncation must not cut a multi byte character in half: the Finnish feeds here
// quote bodies full of ä and €.
func TestTruncateStatusKeepsUTF8Intact(t *testing.T) {
	got := truncateStatus("hyvää päivää € "+strings.Repeat("ä", 400), 20)
	if !utf8.ValidString(got) {
		t.Errorf("truncated message is not valid UTF-8: %q", got)
	}
	if len(got) > 20+8 {
		t.Errorf("truncated to %d bytes, want at most 28", len(got))
	}
}

// The point of the whole change: a cycle where one feed 404s and another works
// must leave exactly one feed marked broken.
func TestFetchNewsRecordsPerFeedStatus(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	good := seedFeedDoc(t, store, domain.Feed{Name: "good", URL: "http://good.example/feed.xml"})
	bad := seedFeedDoc(t, store, domain.Feed{Name: "bad", URL: "http://bad.example/feed.xml"})

	fetcher := func(ctx context.Context, req rss.FeedRequest) (*rss.FeedResponse, error) {
		if req.URL == good.URL {
			return &rss.FeedResponse{Feed: &rss.Feed{
				Title: "good",
				Items: []*rss.Item{{Title: "one", Link: "http://good.example/1", GUID: "good-1"}},
			}, ETag: `"etag-good"`}, nil
		}
		return nil, errors.New("rss: got status 404 for " + req.URL)
	}

	written, err := store.FetchNews(ctx, []domain.Feed{good, bad}, fetcher)
	if err == nil {
		t.Fatal("FetchNews reported no error although a feed failed")
	}
	if written != 1 {
		t.Errorf("written = %d, want 1", written)
	}

	gotGood, err := store.Feed(ctx, good.ID.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if gotGood.Failing() {
		t.Errorf("the healthy feed was marked broken: %+v", gotGood)
	}
	if gotGood.LastItems != 1 || gotGood.ETag != `"etag-good"` {
		t.Errorf("healthy feed: items=%d etag=%q", gotGood.LastItems, gotGood.ETag)
	}

	gotBad, err := store.Feed(ctx, bad.ID.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if !gotBad.Failing() {
		t.Fatalf("broken feed shows no error: %+v", gotBad)
	}
	if !strings.Contains(gotBad.LastError, "404") {
		t.Errorf("LastError = %q, want the 404", gotBad.LastError)
	}
	if gotBad.Failures != 1 {
		t.Errorf("Failures = %d, want 1", gotBad.Failures)
	}
}

// A feed that stored items before it broke keeps them: recording a failure must
// not touch the news collection.
func TestFetchNewsFailureLeavesStoredItems(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	feed := seedFeedDoc(t, store, domain.Feed{Name: "was fine", URL: "http://wasfine.example/feed.xml"})

	first := func(ctx context.Context, req rss.FeedRequest) (*rss.FeedResponse, error) {
		return &rss.FeedResponse{Feed: &rss.Feed{Title: "was fine", Items: []*rss.Item{
			{Title: "kept", Link: "http://wasfine.example/kept", GUID: "kept-1"},
		}}}, nil
	}
	if _, err := store.FetchNews(ctx, []domain.Feed{feed}, first); err != nil {
		t.Fatal(err)
	}

	second := func(ctx context.Context, req rss.FeedRequest) (*rss.FeedResponse, error) {
		return nil, errors.New("rss: got status 500")
	}
	if _, err := store.FetchNews(ctx, []domain.Feed{feed}, second); err == nil {
		t.Fatal("want the failing cycle to report an error")
	}

	if got := countFeedNews(t, store, "kept-1"); got != 1 {
		t.Errorf("stored items = %d, want the 1 from before the failure", got)
	}
}

// The "check now" button in the UI needs to re-fetch one feed on demand, so an
// operator can fix a URL and see the result immediately instead of waiting for
// the next cycle.
func TestFetchOneRefetchesASingleFeed(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	feed := seedFeedDoc(t, store, domain.Feed{Name: "retry", URL: "http://retry.example/feed.xml"})

	fetcher := func(ctx context.Context, req rss.FeedRequest) (*rss.FeedResponse, error) {
		return &rss.FeedResponse{Feed: &rss.Feed{Title: "retry", Items: []*rss.Item{
			{Title: "fresh", Link: "http://retry.example/fresh", GUID: "fresh-1"},
			{Title: "older", Link: "http://retry.example/older", GUID: "older-1"},
		}}}, nil
	}

	report, err := store.FetchOne(ctx, feed.ID.Hex(), fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if report.Items != 2 || report.Unchanged {
		t.Errorf("report = %+v, want 2 items and not unchanged", report)
	}
	got, err := store.Feed(ctx, feed.ID.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if got.Failing() || got.OKAt.IsZero() || got.LastItems != 2 {
		t.Errorf("status after a manual check: failing=%v okAt=%v items=%d", got.Failing(), got.OKAt, got.LastItems)
	}
}

// A manual check that still fails has to say so, both in the error and on the
// document - that is the whole reason the button exists.
func TestFetchOneReportsAFailure(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	feed := seedFeedDoc(t, store, domain.Feed{Name: "still broken", URL: "http://broken.example/feed.xml"})

	fetcher := func(ctx context.Context, req rss.FeedRequest) (*rss.FeedResponse, error) {
		return nil, fmt.Errorf("rss: got status 404 for %s", req.URL)
	}
	report, err := store.FetchOne(ctx, feed.ID.Hex(), fetcher)
	if err == nil {
		t.Fatal("want an error from a broken feed")
	}
	if report.Items != 0 {
		t.Errorf("report = %+v, want no items", report)
	}
	got, err := store.Feed(ctx, feed.ID.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.LastError, "404") {
		t.Errorf("LastError = %q", got.LastError)
	}
}

func TestFetchOneUnknownFeed(t *testing.T) {
	store := testStore(t)
	if _, err := store.FetchOne(context.Background(), bson.NewObjectID().Hex(), nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if _, err := store.FetchOne(context.Background(), "not-an-id", nil); !errors.Is(err, ErrInvalidID) {
		t.Errorf("err = %v, want ErrInvalidID", err)
	}
}

// An unremoved feed that was never polled shows as "never checked" rather than
// as an error, and a removed feed never appears in a cycle at all.
func TestStatusFieldsRoundTrip(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	feed := seedFeedDoc(t, store, domain.Feed{Name: "status", URL: "http://status.example/feed.xml"})

	status := FetchOutcome{Err: errors.New("XML syntax error on line 11: invalid character entity &auml;")}
	if err := store.RecordFetch(ctx, feed.ID, status); err != nil {
		t.Fatal(err)
	}
	list, err := store.Feeds(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("feeds = %d, want 1", len(list))
	}
	if !strings.Contains(list[0].LastError, "&auml;") {
		t.Errorf("LastError = %q, want the entity error verbatim", list[0].LastError)
	}
	if list[0].Failures != 1 {
		t.Errorf("Failures = %d, want 1", list[0].Failures)
	}
}
