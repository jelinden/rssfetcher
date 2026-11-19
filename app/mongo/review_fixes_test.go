package mongo

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jelinden/rssfetcher/app/domain"
	"github.com/jelinden/rssfetcher/app/rss"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// M3: FetchNews used to return nil whatever happened, so "every feed returned
// 503" and "the database is down" both looked like a healthy cycle.
func TestFetchNewsReportsCycleFailures(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	feeds := []domain.Feed{{ID: bson.NewObjectID(), Name: "failing", URL: "https://example.com/failing.xml"}}

	failing := func(context.Context, rss.FeedRequest) (*rss.FeedResponse, error) {
		return nil, errors.New("dns is down")
	}
	written, err := store.FetchNews(ctx, feeds, failing)
	if err == nil {
		t.Fatal("FetchNews = nil error, want the cycle error")
	}
	if !strings.Contains(err.Error(), "dns is down") {
		t.Errorf("err = %v, want it to name the cause", err)
	}
	if written != 0 {
		t.Errorf("written = %d, want 0", written)
	}

	// A single bad feed must not stop the others, but it must still be reported.
	healthy := domain.Feed{ID: bson.NewObjectID(), Name: "good", URL: "https://example.com/good.xml"}
	var mu sync.Mutex
	seen := 0
	mixed := func(_ context.Context, req rss.FeedRequest) (*rss.FeedResponse, error) {
		mu.Lock()
		seen++
		mu.Unlock()
		if strings.Contains(req.URL, "failing") {
			return nil, errors.New("boom")
		}
		return &rss.FeedResponse{Feed: testFeed(req.URL)}, nil
	}
	written, err = store.FetchNews(ctx, append(feeds, healthy), mixed)
	if err == nil {
		t.Fatal("FetchNews = nil error, want the per-feed error even when another feed succeeded")
	}
	if seen != 2 {
		t.Errorf("downloaded %d feeds in the mixed cycle, want both attempted (the failure isolated, not aborting the fan-out)", seen)
	}
	if written == 0 {
		t.Error("the healthy feed was not stored")
	}
}

// L4: the comment promised "the newest items", the code took the first N in
// document order.
func TestNewsItemUpdatesKeepsTheNewestItems(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	feed := domain.Feed{ID: bson.NewObjectID(), Name: "ordered", URL: "https://example.com/ordered.xml", Language: "en"}

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	n := rss.MaxItemsPerFeed()
	items := make([]*rss.Item, 0, n+3)
	// Oldest first, so "the first N" and "the newest N" differ.
	for i := n + 2; i >= 0; i-- {
		id := bson.NewObjectID().Hex()
		items = append(items, &rss.Item{
			Title: "item",
			Link:  "https://example.com/" + id,
			GUID:  "https://example.com/" + id,
			Date:  now.Add(-time.Duration(i) * time.Hour),
		})
	}
	if len(items) <= n {
		t.Fatalf("fixture too small: %d items, limit %d", len(items), n)
	}

	models := NewsItemUpdates(feed, items)
	if len(models) != n {
		t.Fatalf("built %d models, want the newest %d", len(models), n)
	}
	guids := filterGuids(t, models)
	if !guids[NormalizeGUID(items[len(items)-1].GUID)] {
		t.Error("the newest item is missing from the stored set")
	}
	if guids[NormalizeGUID(items[0].GUID)] {
		t.Error("the oldest item was stored instead of a newer one")
	}

	// And that is what actually lands in the database.
	if _, err := store.saveNewsItems(ctx, feed, items); err != nil {
		t.Fatal(err)
	}
	count, err := store.collection(newsCollection).CountDocuments(ctx, bson.M{"rssSource": "ordered"})
	if err != nil {
		t.Fatal(err)
	}
	if count != int64(n) {
		t.Errorf("stored %d items, want %d", count, n)
	}
}

// filterGuids reads the rssGuid of every upsert filter.
func filterGuids(t *testing.T, models []mongo.WriteModel) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, m := range models {
		update, ok := m.(*mongo.UpdateOneModel)
		if !ok {
			t.Fatalf("model is %T, want *mongo.UpdateOneModel", m)
		}
		filter, ok := update.Filter.(bson.M)
		if !ok {
			t.Fatalf("filter is %T, want bson.M", update.Filter)
		}
		guid, _ := filter["rssGuid"].(string)
		out[guid] = true
	}
	return out
}

// L5: SaveFeedItem upserted, so flagging an already deleted feed recreated it
// as a stub document.
func TestSaveFeedItemDoesNotResurrectDeletedFeeds(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)

	feed := domain.Feed{ID: bson.NewObjectID(), Name: "ghost", URL: "https://example.com/ghost.xml"}
	if err := store.SaveFeedItem(ctx, feed); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SaveFeedItem of an unknown feed = %v, want ErrNotFound", err)
	}
	stored, err := store.Feeds(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range stored {
		if f.ID == feed.ID {
			t.Fatal("the unknown feed was created as a side effect of a flag update")
		}
	}

	// An existing feed is still updated in place.
	if err := store.SaveFeed(ctx, nil, feedInput(feed)); err != nil {
		t.Fatal(err)
	}
	feed.Removed = true
	if err := store.SaveFeedItem(ctx, feed); err != nil {
		t.Fatalf("SaveFeedItem of an existing feed: %v", err)
	}
	loaded, err := store.Feed(ctx, feed.ID.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Removed {
		t.Error("removed flag not stored")
	}
}

// L6: the edit form offers enName/styleName for categories, but the write was
// $setOnInsert only, so editing an existing category did nothing at all.
func TestCategoryEditsAreStored(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)

	first := rss.Category{ID: bson.NewObjectID(), Name: "tech", EnName: "Technology", StyleName: "tech-style"}
	if err := store.SaveCategory(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID = bson.NewObjectID() // a different id must not create a second row
	second.EnName = "Science & Tech"
	second.StyleName = "science"
	if err := store.SaveCategory(ctx, second); err != nil {
		t.Fatal(err)
	}

	categories, err := store.Categories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(categories) != 1 {
		t.Fatalf("got %d categories, want the edit to update rather than duplicate", len(categories))
	}
	if categories[0].EnName != "Science & Tech" || categories[0].StyleName != "science" {
		t.Errorf("category = %+v, want the edited names", categories[0])
	}
	if categories[0].ID != first.ID {
		t.Error("editing a category replaced its id")
	}

	sub := rss.SubCategory{ID: bson.NewObjectID(), SubCategory: "ai", EnName: "Artificial Intelligence"}
	if err := store.SaveSubCategory(ctx, sub); err != nil {
		t.Fatal(err)
	}
	sub.EnName = "AI & Robotics"
	if err := store.SaveSubCategory(ctx, sub); err != nil {
		t.Fatal(err)
	}
	subs, err := store.SubCategories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 || subs[0].EnName != "AI & Robotics" {
		t.Errorf("sub categories = %+v, want the edited enName", subs)
	}
}

// L7: SaveFeed and SaveFeedItem kept two copies of the same $set document, which
// is how fields quietly stop being written.
func TestFeedWritesCoverEveryFeedField(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)

	feed := domain.Feed{
		ID: bson.NewObjectID(), Name: "full", URL: "https://example.com/full.xml",
		SiteURL:     "https://example.com",
		Language:    "de",
		Category:    rss.Category{ID: bson.NewObjectID(), Name: "cat"},
		SubCategory: &rss.SubCategory{ID: bson.NewObjectID(), SubCategory: "sub", EnName: "Sub"},
		Removed:     true,
	}
	if err := store.SaveFeed(ctx, nil, feedInput(feed)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveFeedItem(ctx, feed); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Feed(ctx, feed.ID.Hex())
	if err != nil {
		t.Fatal(err)
	}
	// A field missing from the shared $set document comes back empty.
	if loaded.Name != "full" || loaded.URL != feed.URL || loaded.SiteURL != feed.SiteURL {
		t.Errorf("identity fields lost: %+v", loaded)
	}
	if loaded.Language != "de" {
		t.Errorf("language = %q, want de", loaded.Language)
	}
	if loaded.Category.Name != "cat" {
		t.Errorf("category = %+v, want cat", loaded.Category)
	}
	if loaded.EffectiveSubCategory() == nil || loaded.EffectiveSubCategory().SubCategory != "sub" {
		t.Errorf("sub category = %+v, want sub", loaded.SubCategory)
	}
	if !loaded.Removed {
		t.Error("removed lost")
	}
}

func feedInput(f domain.Feed) domain.FeedInput {
	in := domain.FeedInput{ID: f.ID, Name: f.Name, URL: f.URL, SiteURL: f.SiteURL, Language: f.Language, Category: f.Category}
	if f.SubCategory != nil {
		in.SubCategory = *f.SubCategory
	}
	return in
}

// L2 (storage half): the validators of the previous fetch have to be persisted,
// sent back on the next cycle, and a 304 must leave the stored items alone.
func TestConditionalFetchingUsesStoredValidators(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	feed := domain.Feed{ID: bson.NewObjectID(), Name: "cond", URL: "https://example.com/cond.xml", Language: "en"}
	if err := store.SaveFeed(ctx, nil, feedInput(feed)); err != nil {
		t.Fatal(err)
	}

	var requests []rss.FeedRequest
	cycle := 0
	fetcher := func(_ context.Context, req rss.FeedRequest) (*rss.FeedResponse, error) {
		requests = append(requests, req)
		cycle++
		if cycle == 1 {
			return &rss.FeedResponse{
				Feed:         testFeed(req.URL),
				ETag:         `"v1"`,
				LastModified: "Mon, 02 Jan 2026 10:00:00 GMT",
			}, nil
		}
		return &rss.FeedResponse{NotModified: true, ETag: req.ETag, LastModified: req.LastModified}, nil
	}

	if _, err := store.FetchNews(ctx, []domain.Feed{feed}, fetcher); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Feed(ctx, feed.ID.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ETag != `"v1"` || loaded.LastModified != "Mon, 02 Jan 2026 10:00:00 GMT" {
		t.Fatalf("validators not persisted: %+v", loaded)
	}

	before := countNews(ctx, t, store)
	if _, err := store.FetchNews(ctx, []domain.Feed{*loaded}, fetcher); err != nil {
		t.Fatal(err)
	}
	if requests[1].ETag != `"v1"` || requests[1].LastModified != "Mon, 02 Jan 2026 10:00:00 GMT" {
		t.Errorf("second cycle sent %+v, want the stored validators", requests[1])
	}
	if after := countNews(ctx, t, store); after != before {
		t.Errorf("a 304 cycle changed the stored items: %d -> %d", before, after)
	}
	if before != 1 {
		t.Fatalf("first cycle stored %d items, want 1", before)
	}
}

func countNews(ctx context.Context, t *testing.T, store *Store) int64 {
	t.Helper()
	n, err := store.collection(newsCollection).CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func testFeed(url string) *rss.Feed {
	return &rss.Feed{
		Title:     "Test",
		Link:      "https://example.com",
		UpdateURL: url,
		Items: []*rss.Item{{
			Title: "one",
			Link:  url + "/1",
			GUID:  url + "/1",
			Date:  time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC),
		}},
	}
}

// L8: a failed index creation has to be reported (and name the collection),
// because the whole dedup guarantee rests on that unique index.
func TestEnsureIndexesReportsConflictingIndexes(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)

	// Same name, different options: the server refuses, and starting the HTTP
	// server anyway would mean running without the constraint.
	_, err := store.collection(newsCollection).Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "rssGuid", Value: 1}},
		Options: options.Index().SetName("uniq_rssGuid").SetUnique(false),
	})
	if err != nil {
		t.Fatalf("seeding the conflicting index: %v", err)
	}

	err = store.EnsureIndexes(ctx)
	if err == nil {
		t.Fatal("EnsureIndexes = nil, want the conflicting index reported")
	}
	if !strings.Contains(err.Error(), newsCollection) {
		t.Errorf("err = %v, want it to name the collection", err)
	}
}

// L2 (scheduler half): a feed that said "refresh me hourly" must not be pulled
// every two minutes just because our interval is shorter.
func TestFetchNewsSkipsFeedsThatAreStillFresh(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)

	// Real wall clock time: the TTL window is compared against time.Now().
	now := time.Now().UTC()
	fresh := domain.Feed{ID: bson.NewObjectID(), Name: "fresh", URL: "https://example.com/fresh.xml", TTL: time.Hour, FetchedAt: now.Add(-5 * time.Minute)}
	stale := domain.Feed{ID: bson.NewObjectID(), Name: "stale", URL: "https://example.com/stale.xml", TTL: time.Hour, FetchedAt: now.Add(-3 * time.Hour)}
	never := domain.Feed{ID: bson.NewObjectID(), Name: "never", URL: "https://example.com/never.xml", TTL: time.Hour}

	var mu sync.Mutex
	fetched := map[string]bool{}
	fetcher := func(_ context.Context, req rss.FeedRequest) (*rss.FeedResponse, error) {
		mu.Lock()
		fetched[req.URL] = true
		mu.Unlock()
		return &rss.FeedResponse{Feed: testFeed(req.URL)}, nil
	}

	if _, err := store.FetchNews(ctx, []domain.Feed{fresh, stale, never}, fetcher); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := map[string]bool{}
	for k, v := range fetched {
		got[k] = v
	}
	mu.Unlock()
	if got[fresh.URL] {
		t.Error("a feed inside its own <ttl> window was fetched anyway")
	}
	if !got[stale.URL] || !got[never.URL] {
		t.Errorf("stale/never fetched feeds = %v, want both fetched", got)
	}

	// -honour-feed-ttl=false must fetch everything again.
	store.SetHonourFeedTTL(false)
	mu.Lock()
	fetched = map[string]bool{}
	mu.Unlock()
	if _, err := store.FetchNews(ctx, []domain.Feed{fresh}, fetcher); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	again := fetched[fresh.URL]
	mu.Unlock()
	if !again {
		t.Error("with -honour-feed-ttl=false the feed should still be fetched")
	}
}

// The TTL of the feed document is what the next cycle reads, so it has to be
// stored together with the validators.
func TestRecordFetchStoresTheValidatorsAndTTL(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	feed := domain.Feed{ID: bson.NewObjectID(), Name: "ttlstore", URL: "https://example.com/ttlstore.xml"}
	if err := store.SaveFeed(ctx, nil, feedInput(feed)); err != nil {
		t.Fatal(err)
	}
	err := store.RecordFetch(ctx, feed.ID, FetchOutcome{ETag: `"e"`, LastModified: "Mon, 02 Jan 2026 10:00:00 GMT", TTL: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Feed(ctx, feed.ID.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ETag != `"e"` || loaded.LastModified == "" {
		t.Errorf("validators = %+v", loaded)
	}
	if loaded.TTL != 30*time.Minute {
		t.Errorf("TTL = %s, want 30m", loaded.TTL)
	}
	if loaded.FetchedAt.IsZero() {
		t.Error("FetchedAt not recorded")
	}
}

// Found in the live smoke run, not in the review: nothing stops the same feed URL
// from being subscribed twice, and the second row is polled, listed and stored
// alongside the first. A stub created that way carries an empty category, so its
// items silently disappear from category views.
func TestSaveFeedRejectsADuplicateSubscription(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	feed := domain.Feed{ID: bson.NewObjectID(), Name: "dup", URL: "https://example.com/dup.xml", Language: "en"}
	if err := store.SaveFeed(ctx, nil, feedInput(feed)); err != nil {
		t.Fatalf("first subscription: %v", err)
	}

	second := feed
	second.ID = bson.NewObjectID()
	second.Name = "dup again"
	err := store.SaveFeed(ctx, nil, feedInput(second))
	if !errors.Is(err, ErrDuplicateFeed) {
		t.Fatalf("subscribing the same URL twice = %v, want ErrDuplicateFeed", err)
	}

	// Renaming the same feed (an update carrying its own id) must still work, and
	// so must re-subscribing a URL whose old row was removed.
	updated := feed
	updated.Name = "renamed"
	if err := store.SaveFeed(ctx, &feed, feedInput(updated)); err != nil {
		t.Fatalf("updating a feed with the same URL: %v", err)
	}
	feed.Removed = true
	if err := store.SaveFeedItem(ctx, feed); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveFeed(ctx, nil, feedInput(domain.Feed{ID: bson.NewObjectID(), Name: "again", URL: feed.URL})); err != nil {
		t.Errorf("re-subscribing a removed feed's URL: %v", err)
	}
}

// A publisher whose <ttl> is a week (some do) used to be polled on the global
// interval; once the ttl is honoured it would only be fetched weekly, which no
// news reader survives. The cap is what limits that, and it has to be applied
// before deciding whether the feed is due.
func TestFeedsDueForFetchCapsTheTTL(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	feeds := []domain.Feed{
		{ID: bson.NewObjectID(), Name: "greedy", URL: "https://a.example/feed", FetchedAt: now.Add(-10 * time.Minute), TTL: 24 * time.Hour},
		{ID: bson.NewObjectID(), Name: "modest", URL: "https://b.example/feed", FetchedAt: now.Add(-10 * time.Minute), TTL: 5 * time.Minute},
		{ID: bson.NewObjectID(), Name: "fresh", URL: "https://c.example/feed", FetchedAt: now.Add(-1 * time.Minute), TTL: 5 * time.Minute},
		{ID: bson.NewObjectID(), Name: "untouched", URL: "https://d.example/feed"},
	}

	// No cap: only the two that have waited their own ttl.
	due := FeedsDueForFetch(feeds, now, 0)
	if names := feedNames(due); len(names) != 2 || !contains(names, "modest") || !contains(names, "untouched") {
		t.Errorf("uncapped due = %v, want modest+untouched", names)
	}

	// Capped at 2m: everything except the one fetched a minute ago.
	due = FeedsDueForFetch(feeds, now, 2*time.Minute)
	if names := feedNames(due); len(names) != 3 || contains(names, "fresh") {
		t.Errorf("capped due = %v, want all but the freshly fetched one", names)
	}
}

// The store has to apply the configured cap, not just expose a pure function.
func TestStoreMaxFeedTTLCapsPolling(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	store.SetHonourFeedTTL(true)
	store.SetMaxFeedTTL(time.Minute)

	feed := seedFeedDoc(t, store, domain.Feed{Name: "greedy", URL: "http://cap.example/feed.xml"})
	// Ten minutes old content with a publisher asking for a day.
	if err := store.RecordFetch(ctx, feed.ID, FetchOutcome{ETag: `"e"`, TTL: 24 * time.Hour, Items: 1}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Feed(ctx, feed.ID.Hex())
	if err != nil {
		t.Fatal(err)
	}

	called := 0
	fetcher := func(context.Context, rss.FeedRequest) (*rss.FeedResponse, error) {
		called++
		return &rss.FeedResponse{Feed: testFeed("http://cap.example/feed.xml")}, nil
	}
	loaded.FetchedAt = time.Now().Add(-5 * time.Minute)
	if _, err := store.FetchNews(ctx, []domain.Feed{*loaded}, fetcher); err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Errorf("fetcher called %d times, want the cap to make the feed due again", called)
	}

	// With no cap the same feed must be left alone.
	called = 0
	store.SetMaxFeedTTL(0)
	if _, err := store.FetchNews(ctx, []domain.Feed{*loaded}, fetcher); err != nil {
		t.Fatal(err)
	}
	if called != 0 {
		t.Errorf("fetcher called %d times with the cap off, want the publisher's own ttl honoured", called)
	}
}

func feedNames(feeds []domain.Feed) []string {
	out := make([]string, 0, len(feeds))
	for _, f := range feeds {
		out = append(out, f.Name)
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
