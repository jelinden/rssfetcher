package mongo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jelinden/rssfetcher/app/auth"
	"github.com/jelinden/rssfetcher/app/domain"
	"github.com/jelinden/rssfetcher/app/rss"
	"go.mongodb.org/mongo-driver/v2/bson"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

func testURI() string {
	if uri := os.Getenv("MONGO_TEST_URI"); uri != "" {
		return uri
	}
	return "mongodb://127.0.0.1:27017"
}

// testStore returns a store pointing at a throwaway database, or skips the test
// when no mongod is reachable.
func testStore(t *testing.T) *Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := InitMongoClient(ctx, testURI())
	if err != nil {
		t.Skipf("no reachable mongod at %s: %v", testURI(), err)
	}
	name := fmt.Sprintf("rssfetcher_test_%d", time.Now().UnixNano())
	// Cheap scrypt parameters: the default cost is ~100ms per hash, which
	// would make the suite crawl.
	store := NewStore(client, name).WithHasher(&auth.Hasher{Params: auth.Params{N: 2, R: 1, P: 1, KeyLen: 16}})
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Database(name).Drop(dctx)
	})
	return store
}

func TestInitMongoClientUnreachableServer(t *testing.T) {
	// v2's mongo.Connect is lazy, so the Ping inside InitMongoClient is what
	// has to report the failure (and it must not take 30s to do so).
	start := time.Now()
	client, err := InitMongoClient(context.Background(), "mongodb://127.0.0.1:1")
	if err == nil {
		_ = client.Disconnect(context.Background())
		t.Fatal("InitMongoClient to a closed port = nil error, want an error")
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("InitMongoClient took %s, want a fast server-selection failure", elapsed)
	}
	if client != nil {
		t.Errorf("client = %v, want nil on failure", client)
	}
}

func TestInitMongoClientInvalidURI(t *testing.T) {
	// "localhost" is the old default value of -address: it must produce an
	// error, not a nil client that panics on the first Ping.
	client, err := InitMongoClient(context.Background(), "localhost")
	if err == nil {
		t.Fatal("InitMongoClient(localhost) = nil error, want an error")
	}
	if client != nil {
		t.Errorf("client = %v, want nil", client)
	}
	if !strings.Contains(err.Error(), "mongodb") {
		t.Errorf("error %q should explain the required scheme", err)
	}
}

func TestParseObjectID(t *testing.T) {
	id := bson.NewObjectID()
	got, err := ParseObjectID(id.Hex())
	if err != nil {
		t.Fatalf("ParseObjectID: %v", err)
	}
	if got != id {
		t.Errorf("round trip = %v, want %v", got, id)
	}
	for _, bad := range []string{"", "  ", "not-an-id", "public/tmpl"} {
		if _, err := ParseObjectID(bad); err == nil {
			t.Errorf("ParseObjectID(%q) = nil error, want an error", bad)
		}
	}
}

func TestNormalizeGUID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://example.com/a", "example.com/a"},
		{"http://example.com/a", "example.com/a"},
		{"https://example.com/a?x=1#y", "example.com/a?x=1#y"},
		{"example.com/a", "example.com/a"},
		{"  https://example.com/b  ", "example.com/b"},
		{"", ""},
	} {
		if got := NormalizeGUID(tc.in); got != tc.want {
			t.Errorf("NormalizeGUID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPrepareNewsItem(t *testing.T) {
	feed := domain.Feed{ID: bson.NewObjectID(), Name: "Source", Language: "sv", Category: rss.Category{ID: bson.NewObjectID(), Name: "Tech"}}

	item := &rss.Item{
		Title: "  Hello  ",
		Link:  " http://example.com/1 ",
		GUID:  "https://example.com/1",
		Date:  time.Now().Add(48 * time.Hour), // clock skew must be clamped
	}
	prepared, ok := PrepareNewsItem(item, feed)
	if !ok {
		t.Fatal("PrepareNewsItem rejected a valid item")
	}
	if prepared.Title != "Hello" || prepared.Link != "http://example.com/1" {
		t.Errorf("not trimmed: %+v", prepared)
	}
	if prepared.GUID != "example.com/1" {
		t.Errorf("GUID = %q, want example.com/1", prepared.GUID)
	}
	if prepared.SubCategory != nil {
		t.Errorf("SubCategory = %v, want nil for a feed without one", prepared.SubCategory)
	}
	if prepared.Date.After(time.Now()) {
		t.Errorf("Date = %v, should not be in the future", prepared.Date)
	}
	if prepared.Source != "Source" || prepared.Language != "sv" {
		t.Errorf("source/language not copied: %+v", prepared)
	}
}

// A feed stored without a subCategory has a nil pointer. This used to panic with
// "invalid memory address or nil pointer dereference", which crashed the whole
// process because saveNewsItems runs in a goroutine.
func TestPrepareNewsItemNilSubCategoryDoesNotPanic(t *testing.T) {
	feed := domain.Feed{ID: bson.NewObjectID(), Name: "f"}
	if feed.SubCategory != nil {
		t.Fatal("precondition: subcategory should be nil")
	}
	item := &rss.Item{Title: "t", Link: "http://example.com/1", GUID: "g1"}
	prepared, ok := PrepareNewsItem(item, feed)
	if !ok {
		t.Fatal("PrepareNewsItem rejected a valid item")
	}
	if prepared.SubCategory != nil {
		t.Errorf("SubCategory = %v, want nil", prepared.SubCategory)
	}
}

func TestPrepareNewsItemRejectsIncompleteItems(t *testing.T) {
	feed := domain.Feed{ID: bson.NewObjectID()}
	for name, item := range map[string]*rss.Item{
		"no title": {Link: "http://example.com/1", GUID: "g"},
		"no link":  {Title: "t", GUID: "g"},
		"nothing":  nil,
	} {
		if _, ok := PrepareNewsItem(item, feed); ok {
			t.Errorf("%s: PrepareNewsItem accepted an unusable item", name)
		}
	}
}

func TestNewsItemUpdatesCapsAndDedupes(t *testing.T) {
	feed := domain.Feed{ID: bson.NewObjectID(), Name: "f"}
	limit := rss.MaxItemsPerFeed()
	items := make([]*rss.Item, 0, limit+3)
	for i := 0; i < limit+2; i++ {
		items = append(items, &rss.Item{
			Title: fmt.Sprintf("t%d", i),
			Link:  fmt.Sprintf("http://example.com/%d", i),
			GUID:  fmt.Sprintf("guid-%d", i),
		})
	}
	// exact duplicate inside the same fetch
	items = append(items, &rss.Item{Title: "dup", Link: "http://example.com/0", GUID: "guid-0"})

	models := NewsItemUpdates(feed, items)
	if len(models) != limit {
		t.Fatalf("len(models) = %d, want %d", len(models), limit)
	}
}

func TestSaveFeedItemWithoutSubCategory(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	feed := domain.Feed{ID: bson.NewObjectID(), Name: "no sub", URL: "http://example.com/feed.xml"}
	// SaveFeedItem updates an existing document; the row has to exist first.
	if err := store.SaveFeed(ctx, nil, domain.FeedInput{ID: feed.ID, Name: feed.Name, URL: feed.URL}); err != nil {
		t.Fatalf("SaveFeed: %v", err)
	}
	if err := store.SaveFeedItem(ctx, feed); err != nil {
		t.Fatalf("SaveFeedItem: %v", err)
	}
	stored, err := store.Feed(ctx, feed.ID.Hex())
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if stored.SubCategory != nil {
		t.Errorf("subCategory = %+v, want nil", stored.SubCategory)
	}
	if stored.Name != "no sub" {
		t.Errorf("Name = %q", stored.Name)
	}
}

func TestSaveFeedCreatesAndUpdates(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	in := domain.FeedInput{Name: "first", URL: "http://example.com/feed.xml", Language: "en"}
	if err := store.SaveFeed(ctx, nil, in); err != nil {
		t.Fatalf("SaveFeed insert: %v", err)
	}
	feeds, err := store.Feeds(ctx, false)
	if err != nil {
		t.Fatalf("Feeds: %v", err)
	}
	if len(feeds) != 1 {
		t.Fatalf("len(feeds) = %d, want 1", len(feeds))
	}
	loaded := feeds[0]

	in.ID = loaded.ID
	in.Name = "second"
	if err := store.SaveFeed(ctx, &loaded, in); err != nil {
		t.Fatalf("SaveFeed update: %v", err)
	}
	feeds, err = store.Feeds(ctx, false)
	if err != nil {
		t.Fatalf("Feeds: %v", err)
	}
	if len(feeds) != 1 {
		t.Fatalf("update inserted a second document: %d feeds", len(feeds))
	}
	if feeds[0].Name != "second" || feeds[0].ID != loaded.ID {
		t.Errorf("update did not modify the document: %+v", feeds[0])
	}
}

func TestFeedNotFoundIsReported(t *testing.T) {
	store := testStore(t)
	if _, err := store.Feed(context.Background(), bson.NewObjectID().Hex()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Feed(missing) err = %v, want ErrNotFound", err)
	}
	if _, err := store.Feed(context.Background(), "junk"); err == nil {
		t.Fatal("Feed(junk) = nil error, want an error")
	}
}

func TestFeedsFiltersRemoved(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	keep := domain.Feed{ID: bson.NewObjectID(), Name: "keep", URL: "http://example.com/keep"}
	drop := domain.Feed{ID: bson.NewObjectID(), Name: "drop", URL: "http://example.com/drop", Removed: true}
	for _, f := range []domain.Feed{keep, drop} {
		// SaveFeedItem updates only (it used to upsert, which let a "mark as
		// removed" click resurrect a deleted feed as a stub).
		if err := store.SaveFeed(ctx, nil, domain.FeedInput{ID: f.ID, Name: f.Name, URL: f.URL}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := store.SaveFeedItem(ctx, f); err != nil {
			t.Fatalf("mark removed: %v", err)
		}
	}

	active, err := store.Feeds(ctx, true)
	if err != nil {
		t.Fatalf("Feeds: %v", err)
	}
	if len(active) != 1 || active[0].Name != "keep" {
		t.Errorf("active feeds = %+v, want only keep", active)
	}
	all, err := store.Feeds(ctx, false)
	if err != nil {
		t.Fatalf("Feeds: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("all feeds = %d, want 2", len(all))
	}
}

func TestCategoriesUpsert(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	if err := store.SaveCategory(ctx, rss.Category{ID: bson.NewObjectID(), Name: "Tech"}); err != nil {
		t.Fatalf("SaveCategory: %v", err)
	}
	// Saving the same name again must not create a duplicate.
	if err := store.SaveCategory(ctx, rss.Category{ID: bson.NewObjectID(), Name: "Tech"}); err != nil {
		t.Fatalf("SaveCategory (duplicate): %v", err)
	}
	cats, err := store.Categories(ctx)
	if err != nil {
		t.Fatalf("Categories: %v", err)
	}
	if len(cats) != 1 {
		t.Fatalf("categories = %+v, want exactly one", cats)
	}
	if got, err := store.CategoryByName(ctx, "Tech"); err != nil || got.Name != "Tech" {
		t.Errorf("CategoryByName = %+v, %v", got, err)
	}
	if got, err := store.CategoryByName(ctx, "Missing"); err != nil || !got.ID.IsZero() {
		t.Errorf("CategoryByName(missing) = %+v, %v; want a zero category and no error", got, err)
	}
	if err := store.SaveCategory(ctx, rss.Category{Name: "  "}); err == nil {
		t.Error("SaveCategory accepted an empty name")
	}
}

func TestSubCategoriesUpsert(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	sub := rss.SubCategory{ID: bson.NewObjectID(), SubCategory: "AI"}
	if err := store.SaveSubCategory(ctx, sub); err != nil {
		t.Fatalf("SaveSubCategory: %v", err)
	}
	if err := store.SaveSubCategory(ctx, rss.SubCategory{ID: bson.NewObjectID(), SubCategory: "AI"}); err != nil {
		t.Fatalf("SaveSubCategory (duplicate): %v", err)
	}
	subs, err := store.SubCategories(ctx)
	if err != nil {
		t.Fatalf("SubCategories: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("sub categories = %+v, want exactly one", subs)
	}
	if got, err := store.SubCategoryByName(ctx, "Missing"); err != nil || !got.ID.IsZero() {
		t.Errorf("SubCategoryByName(missing) = %+v, %v", got, err)
	}
}

// docTime converts a decoded BSON date into a time.Time.
func docTime(t *testing.T, v any) time.Time {
	t.Helper()
	switch d := v.(type) {
	case bson.DateTime:
		return d.Time()
	case time.Time:
		return d
	default:
		t.Fatalf("unexpected pubDate type %T", v)
		return time.Time{}
	}
}

func itemByGUID(t *testing.T, store *Store, guid string) bson.M {
	t.Helper()
	var doc bson.M
	err := store.collection(newsCollection).FindOne(context.Background(), bson.M{"rssGuid": guid}).Decode(&doc)
	if err != nil {
		t.Fatalf("decode %s: %v", guid, err)
	}
	return doc
}

// The news upsert must keep the click counter, the read flag and the original
// publication date when a feed re-publishes an item.
func TestSaveNewsItemsPreservesClicksAndDate(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	feed := domain.Feed{ID: bson.NewObjectID(), Name: "Source", Language: "sv"}
	first := time.Now().Add(-72 * time.Hour)
	item := &rss.Item{Title: "Headline", Link: "http://example.com/1", GUID: "https://example.com/1", Date: first}

	if _, err := store.saveNewsItems(ctx, feed, []*rss.Item{item}); err != nil {
		t.Fatalf("saveNewsItems: %v", err)
	}
	doc := itemByGUID(t, store, "example.com/1")
	storedDate := docTime(t, doc["pubDate"])
	if !storedDate.Equal(first.Truncate(time.Nanosecond)) && storedDate.After(first.Add(time.Second)) {
		t.Fatalf("pubDate = %v, want %v", storedDate, first)
	}
	clicksKey := "clicks"
	if _, ok := doc[clicksKey]; !ok {
		t.Fatalf("clicks missing from %+v", doc)
	}

	// Simulate a reader clicking, then re-fetch the same item with a new title.
	if _, err := store.collection(newsCollection).UpdateOne(ctx, bson.M{"rssGuid": "example.com/1"},
		bson.M{"$set": bson.M{"clicks": 42, "read": true}}); err != nil {
		t.Fatalf("bump clicks: %v", err)
	}
	if _, err := store.saveNewsItems(ctx, feed, []*rss.Item{{Title: "Renamed", Link: "http://example.com/1", GUID: "https://example.com/1", Date: time.Now()}}); err != nil {
		t.Fatalf("saveNewsItems (again): %v", err)
	}

	doc = itemByGUID(t, store, "example.com/1")
	if doc["rssTitle"] != "Renamed" {
		t.Errorf("title = %v, want Renamed", doc["rssTitle"])
	}
	if doc["clicks"] != int32(42) {
		t.Errorf("clicks = %v, want 42 to be preserved", doc["clicks"])
	}
	if doc["read"] != true {
		t.Errorf("read = %v, want true to be preserved", doc["read"])
	}
	if got := docTime(t, doc["pubDate"]); !got.Equal(storedDate) {
		t.Errorf("pubDate = %v, want the first seen date %v", got, storedDate)
	}

	// And still exactly one document for that GUID.
	total, err := store.collection(newsCollection).CountDocuments(ctx, bson.M{"rssGuid": "example.com/1"})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 1 {
		t.Errorf("%d documents for one guid, want 1", total)
	}
}

func TestEnsureIndexesMakesGuidUnique(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.EnsureIndexes(ctx); err != nil {
		t.Fatalf("EnsureIndexes: %v", err)
	}
	_, err := store.collection(newsCollection).InsertMany(ctx, []any{
		&rss.Item{ID: bson.NewObjectID(), GUID: "dup", Title: "a", Link: "http://example.com/a"},
		&rss.Item{ID: bson.NewObjectID(), GUID: "dup", Title: "b", Link: "http://example.com/b"},
	})
	if err == nil {
		t.Fatal("duplicate rssGuid accepted, want the unique index to reject it")
	}
	if !mongo.IsDuplicateKeyError(err) {
		t.Fatalf("err = %v, want a duplicate key error", err)
	}
}

func TestFetchNewsBoundsConcurrencyAndSurvivesFailures(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	var mu sync.Mutex
	current, peak := 0, 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current++
		if current > peak {
			peak = current
		}
		mu.Unlock()

		time.Sleep(50 * time.Millisecond)
		if strings.Contains(r.URL.Path, "boom") {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			fmt.Fprintf(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>t</title><link>l</link><description>d</description>
<item><title>i%[2]s</title><link>%[1]s%[2]s/post</link><guid>%[1]s%[2]s/guid</guid><pubDate>Mon, 02 Jan 2006 15:04:05 MST</pubDate></item>
</channel></rss>`, baseURL(r), r.URL.Path)
		}
		mu.Lock()
		current--
		mu.Unlock()
	}))
	defer srv.Close()

	const feeds = 30
	list := make([]domain.Feed, 0, feeds)
	for i := 0; i < feeds; i++ {
		url := fmt.Sprintf("%s/feed-%d", srv.URL, i)
		if i%5 == 0 {
			url = srv.URL + "/boom"
		}
		list = append(list, domain.Feed{ID: bson.NewObjectID(), Name: fmt.Sprintf("feed %d", i), URL: url, Language: "en"})
	}

	// Every fifth feed answers 500, so the cycle must report it while still
	// storing the healthy ones (M3: it used to return nil whatever happened).
	written, err := store.FetchNews(ctx, list, rss.Fetch)
	if err == nil {
		t.Fatal("FetchNews = nil, want the failing feeds reported")
	}
	mu.Lock()
	gotPeak := peak
	mu.Unlock()
	if gotPeak > MaxParallelFetches {
		t.Errorf("peak concurrency = %d, want <= %d", gotPeak, MaxParallelFetches)
	}
	if gotPeak < 2 {
		t.Errorf("peak concurrency = %d, downloads should be parallel", gotPeak)
	}

	const healthy = feeds - feeds/5 // every fifth URL answers 500
	stored, err := store.collection(newsCollection).CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if int(stored) != healthy {
		t.Errorf("stored %d items, want %d (one per healthy feed)", stored, healthy)
	}
	if written != healthy {
		t.Errorf("FetchNews reported %d changed items, want %d", written, healthy)
	}

	// A second cycle must not duplicate anything nor rewrite unchanged items.
	written, _ = store.FetchNews(ctx, list, rss.Fetch)
	stored, err = store.collection(newsCollection).CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if int(stored) != healthy {
		t.Errorf("second cycle stored %d items, want %d (no duplicates)", stored, healthy)
	}
	if written != 0 {
		t.Errorf("second cycle reported %d changed items, want 0", written)
	}
}

// baseURL returns the base URL of the test server from inside its handler.
func baseURL(r *http.Request) string {
	return "http://" + r.Host
}

func TestFetchNewsSurvivesPanickingFetcher(t *testing.T) {
	store := testStore(t)

	feeds := []domain.Feed{
		{ID: bson.NewObjectID(), Name: "panics", URL: "http://example.com/panic"},
		{ID: bson.NewObjectID(), Name: "empty url"},
		{ID: bson.NewObjectID(), Name: "ok", URL: "http://example.com/ok"},
	}
	fetcher := func(_ context.Context, req rss.FeedRequest) (*rss.FeedResponse, error) {
		switch req.URL {
		case "http://example.com/panic":
			panic("boom")
		case "http://example.com/ok":
			return &rss.FeedResponse{Feed: &rss.Feed{Items: []*rss.Item{{Title: "t", Link: "http://example.com/ok/1", GUID: "ok-1"}}}}, nil
		}
		return nil, errors.New("unreachable")
	}

	done := make(chan struct{})
	var written int
	var err error
	go func() {
		written, err = store.FetchNews(context.Background(), feeds, fetcher)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("FetchNews did not return")
	}
	// The panic is recovered and the other failures are joined into the returned
	// error, but the healthy feed is still stored.
	if err == nil {
		t.Fatal("FetchNews = nil, want the panic and the unreachable feed reported")
	}
	if written != 1 {
		t.Errorf("written = %d, want 1 (the healthy feed only)", written)
	}
}

func TestFetchNewsWithoutFeeds(t *testing.T) {
	store := testStore(t)
	written, err := store.FetchNews(context.Background(), nil, nil)
	if err != nil || written != 0 {
		t.Fatalf("FetchNews(nil) = %d, %v", written, err)
	}
}

// Documents written by earlier versions of this app never stored rssGuid. A
// plain unique index treats every one of those as the same null key, so
// EnsureIndexes failed on startup against an existing database and the fetcher
// silently ran without its dedup guarantee.
func TestEnsureIndexesOverLegacyDocumentsWithoutGUIDs(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// The legacy shape: no rssGuid, no guid.
	legacy := []any{
		bson.M{"title": "old one", "language": "en", "pubDate": time.Now().Add(-24 * time.Hour)},
		bson.M{"title": "old two", "language": "en", "pubDate": time.Now().Add(-48 * time.Hour)},
		bson.M{"title": "old three", "language": "sv", "pubDate": time.Now().Add(-72 * time.Hour)},
	}
	if _, err := store.collection(newsCollection).InsertMany(ctx, legacy); err != nil {
		t.Fatalf("seeding legacy documents: %v", err)
	}

	if err := store.EnsureIndexes(ctx); err != nil {
		t.Fatalf("EnsureIndexes over legacy documents: %v", err)
	}

	// More GUID-less documents must still be insertable.
	if _, err := store.collection(newsCollection).InsertOne(ctx,
		bson.M{"title": "old four", "language": "en"}); err != nil {
		t.Fatalf("a second document without rssGuid was rejected: %v", err)
	}

	// And the constraint must still hold for documents that do have one.
	_, err := store.collection(newsCollection).InsertMany(ctx, []any{
		&rss.Item{ID: bson.NewObjectID(), GUID: "dup-guid", Title: "a", Link: "http://example.com/a"},
		&rss.Item{ID: bson.NewObjectID(), GUID: "dup-guid", Title: "b", Link: "http://example.com/b"},
	})
	if err == nil {
		t.Fatal("duplicate rssGuid accepted, want the unique index to reject it")
	}
	if !mongo.IsDuplicateKeyError(err) {
		t.Fatalf("err = %v, want a duplicate key error", err)
	}
}

// A whole fetch cycle over a collection of legacy documents must work: this is
// exactly what happens the first time the rewritten app meets real data.
func TestSaveNewsItemsOverLegacyDocuments(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if _, err := store.collection(newsCollection).InsertOne(ctx,
		bson.M{"title": "legacy", "language": "en", "rssSource": "http://example.com/rss"}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureIndexes(ctx); err != nil {
		t.Fatalf("EnsureIndexes: %v", err)
	}

	feed := domain.Feed{ID: bson.NewObjectID(), Name: "Source", Language: "en", URL: "http://example.com/rss"}
	if _, err := store.saveNewsItems(ctx, feed, []*rss.Item{
		{Title: "fresh", Link: "http://example.com/fresh", GUID: "guid-1", Date: time.Now()},
	}); err != nil {
		t.Fatalf("SaveNewsItems over legacy documents: %v", err)
	}
	stored, err := store.collection(newsCollection).CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatal(err)
	}
	if stored != 2 {
		t.Errorf("news documents = %d, want 2 (the legacy one plus the new one)", stored)
	}
}
