package mongo

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jelinden/rssfetcher/app/auth"
	"github.com/jelinden/rssfetcher/app/domain"
	"github.com/jelinden/rssfetcher/app/rss"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	// DefaultDatabase is the database used in production.
	DefaultDatabase = "news"

	feedCollection        = "feedcollection"
	newsCollection        = "newscollection"
	categoryCollection    = "categorycollection"
	subCategoryCollection = "subcategorycollection"

	// opTimeout bounds every single database operation. Without it a request
	// (or a fetch cycle) could block forever when MongoDB stalls.
	opTimeout = 10 * time.Second

	// MaxParallelFetches bounds how many feeds are downloaded at the same
	// time. The old code started one goroutine per feed, unconditionally.
	MaxParallelFetches = 8

	// DefaultSessionTTL is how long a login cookie stays valid; it slides on
	// every request that gets close to the deadline.
	DefaultSessionTTL = 12 * time.Hour
)

// ErrNotFound is returned when a document does not exist.
var ErrNotFound = mongo.ErrNoDocuments

// ErrInvalidID is returned when an id is not a valid object id. Handlers map it
// to a 400 instead of a 500.
var ErrInvalidID = errors.New("mongo: invalid id")

// ErrDuplicateFeed reports a subscription for a URL that is already active. It
// wraps ErrConflict so handlers can answer 409 without knowing the details.
var ErrDuplicateFeed = fmt.Errorf("%w: this feed is already subscribed", ErrConflict)

// Store is the MongoDB backed repository. All operations take a context and
// return errors instead of only logging them.
type Store struct {
	client   *mongo.Client
	database string
	hasher   *auth.Hasher

	// honourFeedTTL mirrors -honour-feed-ttl: skip a feed until the <ttl> it
	// published has passed. It is an atomic because the fetch loop reads it while
	// a test may change it.
	honourFeedTTL atomic.Bool
}

// NewStore wraps a connected client. The password hasher is exposed through
// WithHasher so tests can use cheap scrypt parameters.
func NewStore(client *mongo.Client, database string) *Store {
	if database == "" {
		database = DefaultDatabase
	}
	store := &Store{client: client, database: database, hasher: auth.NewHasher()}
	store.honourFeedTTL.Store(true)
	return store
}

// SetHonourFeedTTL turns the per feed <ttl> skip on or off (-honour-feed-ttl).
func (s *Store) SetHonourFeedTTL(honour bool) { s.honourFeedTTL.Store(honour) }

// HonourFeedTTL reports the current per feed TTL policy.
func (s *Store) HonourFeedTTL() bool { return s.honourFeedTTL.Load() }

// WithHasher replaces the password hasher (used by tests to keep scrypt cheap).
func (s *Store) WithHasher(h *auth.Hasher) *Store {
	s.hasher = h
	return s
}

// Database exposes the database handle (used by tests to isolate themselves).
func (s *Store) Database() string { return s.database }

// InitMongoClient connects to MongoDB and verifies the connection. A bad URI
// or an unreachable server is reported as an error; it used to panic on the
// nil client returned by mongo.Connect.
// ValidURI reports whether uri looks like a MongoDB connection string.
func ValidURI(uri string) bool {
	return strings.HasPrefix(uri, "mongodb://") || strings.HasPrefix(uri, "mongodb+srv://")
}

func InitMongoClient(ctx context.Context, uri string) (*mongo.Client, error) {
	if !ValidURI(uri) {
		return nil, fmt.Errorf("mongo: invalid URI %q: scheme must be mongodb or mongodb+srv", uri)
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	// The driver default server selection timeout is 30s; a wrong -address
	// should fail startup fast instead of hanging behind a health check.
	client, err := mongo.Connect(options.Client().
		ApplyURI(uri).
		SetServerSelectionTimeout(3 * time.Second))
	if err != nil {
		return nil, fmt.Errorf("mongo: connect %s: %w", uri, err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		// Best effort cleanup so a failed startup does not leak monitoring
		// goroutines.
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("mongo: ping %s: %w", uri, err)
	}
	return client, nil
}

func (s *Store) collection(name string) *mongo.Collection {
	return s.client.Database(s.database).Collection(name)
}

func (s *Store) opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, opTimeout)
}

// EnsureIndexes creates the indexes the fetcher relies on. The rssGuid index
// is unique: without it two concurrent fetch cycles could insert the same news
// item twice.
//
// It is unique but *partial*. Every document written by earlier versions of
// this app omits rssGuid entirely, and MongoDB indexes a missing field as
// null: a plain unique index therefore collides on its first run against
// existing data and the whole startup fails. Restricting the index to
// documents that actually carry a non-empty rssGuid keeps the guarantee where
// it matters (dedup of real items) without rejecting the legacy documents.
func (s *Store) EnsureIndexes(ctx context.Context) error {
	indexes := []indexSpec{
		namedIndex(newsCollection, "uniq_rssGuid", bson.D{{Key: "rssGuid", Value: 1}}, func(o *options.IndexOptionsBuilder) {
			o.SetUnique(true)
			// $gt against a string only matches strings, so missing, null and ""
			// are all left out of the unique constraint. Every document written
			// by earlier versions of this app omits rssGuid entirely, and MongoDB
			// indexes a missing field as null: a plain unique index therefore
			// collides on its first run against existing data.
			o.SetPartialFilterExpression(bson.M{"rssGuid": bson.M{"$gt": ""}})
		}),
		namedIndex(newsCollection, "idx_pubDate", bson.D{{Key: "pubDate", Value: -1}}, nil),
		namedIndex(newsCollection, "idx_category_language", bson.D{{Key: "category._id", Value: 1}, {Key: "language", Value: 1}}, nil),
		namedIndex(feedCollection, "idx_url", bson.D{{Key: "url", Value: 1}}, nil),
		namedIndex(feedCollection, "idx_removed", bson.D{{Key: "removed", Value: 1}}, nil),
		namedIndex(categoryCollection, "uniq_categoryName", bson.D{{Key: "categoryName", Value: 1}}, func(o *options.IndexOptionsBuilder) { o.SetUnique(true) }),
		namedIndex(subCategoryCollection, "uniq_subCategory", bson.D{{Key: "subCategory", Value: 1}}, func(o *options.IndexOptionsBuilder) { o.SetUnique(true) }),
	}
	indexes = append(indexes, s.authIndexes()...)

	for _, idx := range indexes {
		ctx, cancel := s.opCtx(ctx)
		name, err := s.collection(idx.collection).Indexes().CreateOne(ctx, idx.model)
		cancel()
		if err != nil {
			return fmt.Errorf("mongo: create index %s on %s: %w", idx.name, idx.collection, err)
		}
		// The driver hands back the effective name. A mismatch means an index
		// under that name already exists with different options, which would
		// otherwise go unnoticed until a duplicate slips through.
		if name != idx.name {
			return fmt.Errorf("mongo: index on %s is named %q, expected %q (an existing index with that name has different options)", idx.collection, name, idx.name)
		}
	}
	return nil
}

// indexSpec pairs a collection with an index to create on it.
type indexSpec struct {
	collection string
	name       string
	model      mongo.IndexModel
}

// namedIndex builds a spec whose name is known statically, so the created index
// can be checked against it.
func namedIndex(collection, name string, keys bson.D, configure func(*options.IndexOptionsBuilder)) indexSpec {
	builder := options.Index().SetName(name)
	if configure != nil {
		configure(builder)
	}
	return indexSpec{collection: collection, name: name, model: mongo.IndexModel{Keys: keys, Options: builder}}
}

// feedSetDocument is the single definition of "the mutable part of a feed".
// SaveFeed and SaveFeedItem used to carry two copies of this list, and the second
// one silently fell behind (a field added to the struct was simply never written
// by one of the two paths).
func feedSetDocument(feed domain.Feed) bson.D {
	return bson.D{
		{Key: "feedTitle", Value: feed.Name},
		{Key: "url", Value: feed.URL},
		{Key: "siteUrl", Value: feed.SiteURL},
		{Key: "category", Value: feed.Category},
		{Key: "subCategory", Value: feed.EffectiveSubCategory()},
		{Key: "language", Value: feed.Language},
		{Key: "removed", Value: feed.Removed},
	}
}

// SaveFeedItem updates an existing feed (used for the "removed" flag). It does
// *not* upsert: flagging a feed that has been deleted used to recreate it as a
// stub, which then showed up in the list as an empty row.
func (s *Store) SaveFeedItem(ctx context.Context, feed domain.Feed) error {
	if feed.ID.IsZero() {
		return fmt.Errorf("%w: feed id is required", ErrInvalidID)
	}
	log.Println("mongo: updating feed", feed.ID.Hex(), feed.URL)

	ctx, cancel := s.opCtx(ctx)
	defer cancel()

	res, err := s.collection(feedCollection).UpdateOne(ctx,
		bson.M{"_id": feed.ID},
		bson.D{{Key: "$set", Value: feedSetDocument(feed)}},
	)
	if err != nil {
		return fmt.Errorf("mongo: save feed %s: %w", feed.ID.Hex(), err)
	}
	if res.MatchedCount == 0 {
		return fmt.Errorf("%w: feed %s", ErrNotFound, feed.ID.Hex())
	}
	return nil
}

// MaxStatusMessageLen bounds the error text stored on a feed document. A parse
// failure quotes the beginning of the response, which for an HTML error page is
// several hundred bytes of markup; the log keeps the whole thing.
const MaxStatusMessageLen = 400

// FetchOutcome is the result of one attempt at one feed. It is stored on the
// feed document so the admin UI can show what is broken: before this, the only
// sign of a dead subscription was a line in the server log.
type FetchOutcome struct {
	// Err is set when the attempt failed.
	Err error
	// Unchanged marks a 304 answer: the feed is reachable and healthy, there is
	// just nothing new, so the stored validators stay as they are.
	Unchanged bool

	// ETag, LastModified and TTL describe a fetch that got a body, and Items is
	// how many of its items were stored.
	ETag         string
	LastModified string
	TTL          time.Duration
	Items        int
}

// RecordFetch writes the outcome of a fetch attempt.
//
// Two details matter. A failure must not disturb the conditional GET validators:
// a feed that returned 500 today should still ask conditionally tomorrow rather
// than re-download its whole history. And the failure counter is incremented by
// the database, so two overlapping cycles cannot lose one of the increments.
func (s *Store) RecordFetch(ctx context.Context, id bson.ObjectID, outcome FetchOutcome) error {
	if id.IsZero() {
		return fmt.Errorf("%w: feed id is required", ErrInvalidID)
	}
	now := time.Now().UTC()

	var update bson.D
	if outcome.Err != nil {
		update = append(update,
			bson.E{Key: "$set", Value: bson.D{
				{Key: "lastError", Value: truncateStatus(outcome.Err.Error(), MaxStatusMessageLen)},
				{Key: "lastErrorAt", Value: now},
			}},
			// The counter is incremented in the database, so two overlapping
			// cycles cannot lose one of the increments.
			bson.E{Key: "$inc", Value: bson.D{{Key: "failures", Value: 1}}},
		)
	} else {
		set := bson.D{
			{Key: "okAt", Value: now},
			{Key: "failures", Value: 0},
		}
		if !outcome.Unchanged {
			// fetchedAt records the last response that carried content, which is
			// what the <ttl> window is measured from: a 304 says the stored items
			// are still current, not that new ones arrived.
			set = append(set,
				bson.E{Key: "etag", Value: outcome.ETag},
				bson.E{Key: "lastModified", Value: outcome.LastModified},
				bson.E{Key: "fetchedAt", Value: now},
				bson.E{Key: "ttl", Value: outcome.TTL},
				bson.E{Key: "lastItems", Value: outcome.Items},
			)
		}
		// A feed that answers again is no longer broken, whatever it did before.
		update = append(update,
			bson.E{Key: "$set", Value: set},
			bson.E{Key: "$unset", Value: bson.D{
				{Key: "lastError", Value: ""},
				{Key: "lastErrorAt", Value: ""},
			}},
		)
	}

	ctx, cancel := s.opCtx(ctx)
	defer cancel()

	if _, err := s.collection(feedCollection).UpdateOne(ctx, bson.M{"_id": id}, update); err != nil {
		return fmt.Errorf("mongo: record fetch of %s: %w", id.Hex(), err)
	}
	return nil
}

// truncateStatus cuts a message to max bytes without splitting a UTF-8 rune: the
// messages here quote page bodies, which are full of ä and €.
func truncateStatus(message string, max int) string {
	if max <= 0 || len(message) <= max {
		return message
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}
	return message[:cut] + "…"
}

// SaveFeed creates or updates a feed.
func (s *Store) SaveFeed(ctx context.Context, feed *domain.Feed, in domain.FeedInput) error {
	sub := &rss.SubCategory{ID: in.SubCategory.ID, SubCategory: in.SubCategory.SubCategory, EnName: in.SubCategory.EnName}
	f := domain.Feed{
		ID:          in.IDOrNew(),
		Name:        in.Name,
		URL:         in.URL,
		SiteURL:     in.SiteURL,
		Category:    in.Category,
		SubCategory: sub,
		Language:    in.Language,
	}
	f.SubCategory = f.EffectiveSubCategory()

	ctx, cancel := s.opCtx(ctx)
	defer cancel()

	if feed != nil {
		log.Println("mongo: updating feed", f.ID.Hex(), f.URL)
		f.Removed = feed.Removed
		// The validators of the previous fetch stay: this form does not edit them.
		_, err := s.collection(feedCollection).UpdateOne(ctx,
			bson.M{"_id": f.ID},
			bson.D{{Key: "$set", Value: feedSetDocument(f)}},
		)
		if err != nil {
			return fmt.Errorf("mongo: update feed %s: %w", f.ID.Hex(), err)
		}
		return nil
	}

	// Nothing used to stop the same URL from being subscribed twice; the second
	// row was polled, listed and stored next to the first, and its items (which
	// carry an empty category when the form left it out) disappeared from the
	// category views. A removed row with the same URL may be re-added.
	duplicate, err := s.collection(feedCollection).CountDocuments(ctx, bson.M{
		"url":     f.URL,
		"removed": bson.M{"$ne": true},
	}, options.Count().SetLimit(1))
	if err != nil {
		return fmt.Errorf("mongo: checking for an existing feed %s: %w", f.URL, err)
	}
	if duplicate > 0 {
		return fmt.Errorf("%w: %s", ErrDuplicateFeed, f.URL)
	}

	log.Println("mongo: inserting feed", f.URL)
	if _, err := s.collection(feedCollection).InsertOne(ctx, &f); err != nil {
		return fmt.Errorf("mongo: insert feed %s: %w", f.URL, err)
	}
	return nil
}

// Feed loads a feed by hex object id. ErrNotFound is returned when the feed
// does not exist; a malformed id yields ErrInvalidID.
func (s *Store) Feed(ctx context.Context, feedID string) (*domain.Feed, error) {
	id, err := ParseObjectID(feedID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := s.opCtx(ctx)
	defer cancel()

	var feed domain.Feed
	if err := s.collection(feedCollection).FindOne(ctx, bson.M{"_id": id}).Decode(&feed); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("mongo: load feed %s: %w", feedID, err)
	}
	return &feed, nil
}

// Feeds lists feeds; removed ones are excluded when onlyActive is true.
func (s *Store) Feeds(ctx context.Context, onlyActive bool) ([]domain.Feed, error) {
	query := bson.M{}
	if onlyActive {
		query = bson.M{"removed": bson.M{"$ne": true}}
	}
	ctx, cancel := s.opCtx(ctx)
	defer cancel()

	cursor, err := s.collection(feedCollection).Find(ctx, query, options.Find().SetSort(bson.D{{Key: "feedTitle", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("mongo: list feeds: %w", err)
	}
	defer cursor.Close(ctx)

	feeds := []domain.Feed{}
	if err := cursor.All(ctx, &feeds); err != nil {
		return nil, fmt.Errorf("mongo: decode feeds: %w", err)
	}
	return feeds, nil
}

// SaveCategory inserts a category unless the name already exists. The unique
// index on categoryName makes the insert race-free.
func (s *Store) SaveCategory(ctx context.Context, cat rss.Category) error {
	if strings.TrimSpace(cat.Name) == "" {
		return errors.New("mongo: category name is required")
	}
	if cat.ID.IsZero() {
		cat.ID = bson.NewObjectID()
	}
	ctx, cancel := s.opCtx(ctx)
	defer cancel()

	// $set for the editable presentation fields, $setOnInsert for the identity:
	// a pure $setOnInsert made the form's enName/styleName inputs do nothing at
	// all for a category that already existed.
	_, err := s.collection(categoryCollection).UpdateOne(ctx,
		bson.M{"categoryName": cat.Name},
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "enName", Value: cat.EnName},
				{Key: "styleName", Value: cat.StyleName},
			}},
			{Key: "$setOnInsert", Value: bson.D{{Key: "_id", Value: cat.ID}}},
		},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil // somebody else created it concurrently, which is fine
		}
		return fmt.Errorf("mongo: save category %q: %w", cat.Name, err)
	}
	return nil
}

func (s *Store) SaveSubCategory(ctx context.Context, sub rss.SubCategory) error {
	if strings.TrimSpace(sub.SubCategory) == "" {
		return errors.New("mongo: sub category name is required")
	}
	if sub.ID.IsZero() {
		sub.ID = bson.NewObjectID()
	}
	ctx, cancel := s.opCtx(ctx)
	defer cancel()

	_, err := s.collection(subCategoryCollection).UpdateOne(ctx,
		bson.M{"subCategory": sub.SubCategory},
		bson.D{
			{Key: "$set", Value: bson.D{{Key: "enName", Value: sub.EnName}}},
			{Key: "$setOnInsert", Value: bson.D{{Key: "_id", Value: sub.ID}}},
		},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil
		}
		return fmt.Errorf("mongo: save sub category %q: %w", sub.SubCategory, err)
	}
	return nil
}

func (s *Store) Categories(ctx context.Context) ([]rss.Category, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()

	cursor, err := s.collection(categoryCollection).Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "categoryName", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("mongo: list categories: %w", err)
	}
	defer cursor.Close(ctx)

	categories := []rss.Category{}
	if err := cursor.All(ctx, &categories); err != nil {
		return nil, fmt.Errorf("mongo: decode categories: %w", err)
	}
	return categories, nil
}

func (s *Store) SubCategories(ctx context.Context) ([]rss.SubCategory, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()

	cursor, err := s.collection(subCategoryCollection).Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "subCategory", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("mongo: list sub categories: %w", err)
	}
	defer cursor.Close(ctx)

	subs := []rss.SubCategory{}
	if err := cursor.All(ctx, &subs); err != nil {
		return nil, fmt.Errorf("mongo: decode sub categories: %w", err)
	}
	return subs, nil
}

// CategoryByName returns the category with that name. When it does not exist a
// zero valued category is returned (callers decide whether to allocate an ID).
func (s *Store) CategoryByName(ctx context.Context, name string) (rss.Category, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()

	var category rss.Category
	err := s.collection(categoryCollection).FindOne(ctx, bson.M{"categoryName": name}).Decode(&category)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return rss.Category{}, nil
	}
	if err != nil {
		return rss.Category{}, fmt.Errorf("mongo: load category %q: %w", name, err)
	}
	return category, nil
}

func (s *Store) SubCategoryByName(ctx context.Context, name string) (rss.SubCategory, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()

	var sub rss.SubCategory
	err := s.collection(subCategoryCollection).FindOne(ctx, bson.M{"subCategory": name}).Decode(&sub)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return rss.SubCategory{}, nil
	}
	if err != nil {
		return rss.SubCategory{}, fmt.Errorf("mongo: load sub category %q: %w", name, err)
	}
	return sub, nil
}

// FeedsDueForFetch returns the feeds that must be downloaded now: everything
// without a recorded fetch, plus everything whose own <ttl> has elapsed. A feed
// that published "refresh me hourly" should not be pulled every two minutes
// because our global interval happens to be shorter.
func FeedsDueForFetch(feeds []domain.Feed, now time.Time) []domain.Feed {
	due := make([]domain.Feed, 0, len(feeds))
	for _, feed := range feeds {
		if feed.TTL <= 0 || feed.FetchedAt.IsZero() || !now.Before(feed.FetchedAt.Add(feed.TTL)) {
			due = append(due, feed)
		}
	}
	return due
}

// fetchResult is one feed download attempt.
// FetchReport is what a manual check of a single feed produced.
type FetchReport struct {
	// Items is how many items the fetch stored (0 when the publisher said 304).
	Items int
	// Unchanged reports a conditional GET that was answered with 304.
	Unchanged bool
}

// FetchOne downloads a single feed and stores its items and status, which is
// what the "check now" button in the admin UI needs: an operator fixes a URL and
// wants to know right then whether the publisher answers. The conditional GET
// validators are honoured, so a feed that was just fetched is not re-downloaded.
func (s *Store) FetchOne(ctx context.Context, feedID string, fetcher ...rss.FetchFunc) (FetchReport, error) {
	feed, err := s.Feed(ctx, feedID)
	if err != nil {
		return FetchReport{}, err
	}
	var fn rss.FetchFunc
	if len(fetcher) > 0 {
		fn = fetcher[0]
	}
	if fn == nil {
		fn = rss.Fetch
	}

	res := s.fetchFeed(ctx, *feed, fn)
	switch {
	case res.err != nil:
		s.recordFetchQuietly(ctx, feed.ID, FetchOutcome{Err: res.err})
		return FetchReport{}, res.err
	case res.notModified:
		s.recordFetchQuietly(ctx, feed.ID, FetchOutcome{Unchanged: true})
		return FetchReport{Unchanged: true}, nil
	}

	n, err := s.saveNewsItems(ctx, *feed, res.items)
	if err != nil {
		s.recordFetchQuietly(ctx, feed.ID, FetchOutcome{Err: err})
		return FetchReport{}, err
	}
	s.recordFetchQuietly(ctx, feed.ID, FetchOutcome{
		ETag:         res.etag,
		LastModified: res.lastModified,
		TTL:          res.ttl,
		Items:        n,
	})
	return FetchReport{Items: n}, nil
}

// recordFetchQuietly writes a fetch outcome, complaining only in the log: the
// status of one feed is never worth failing a whole cycle over.
func (s *Store) recordFetchQuietly(ctx context.Context, id bson.ObjectID, outcome FetchOutcome) {
	if id.IsZero() {
		return
	}
	if err := s.RecordFetch(ctx, id, outcome); err != nil {
		log.Printf("mongo: recording the status of %s: %v", id.Hex(), err)
	}
}

type fetchResult struct {
	feed        domain.Feed
	items       []*rss.Item
	notModified bool
	err         error

	// Validators of a response that carried content, to be stored with the
	// health status in one write.
	etag         string
	lastModified string
	ttl          time.Duration
}

// FetchNews downloads every feed and stores its newest items. Downloads run
// with a bounded concurrency and a panic in one feed parser can no longer
// take the whole process (or the cycle) down. It returns how many items were
// written.
func (s *Store) FetchNews(ctx context.Context, feeds []domain.Feed, fetcher rss.FetchFunc) (int, error) {
	if len(feeds) == 0 {
		return 0, nil
	}
	if fetcher == nil {
		fetcher = rss.Fetch
	}

	due := feeds
	if s.HonourFeedTTL() {
		due = FeedsDueForFetch(feeds, time.Now())
		if skipped := len(feeds) - len(due); skipped > 0 {
			log.Printf("rssfetcher: %d of %d feeds are inside their own <ttl> window, skipping them", skipped, len(feeds))
		}
		if len(due) == 0 {
			return 0, nil
		}
	}

	results := s.downloadFeeds(ctx, due, fetcher)

	written := 0
	unchanged := 0
	var errs []error
	failures := 0
	for _, res := range results {
		if res.err != nil {
			failures++
			log.Printf("rssfetcher: feed %s failed: %v", res.feed.URL, res.err)
			errs = append(errs, fmt.Errorf("feed %s: %w", res.feed.URL, res.err))
			s.recordFetchQuietly(ctx, res.feed.ID, FetchOutcome{Err: res.err})
			continue
		}
		if res.notModified {
			// The publisher said "unchanged": there is nothing to write and the
			// stored items and validators stay as they are. The feed is still
			// marked healthy, because it answered.
			unchanged++
			s.recordFetchQuietly(ctx, res.feed.ID, FetchOutcome{Unchanged: true})
			continue
		}
		n, err := s.saveNewsItems(ctx, res.feed, res.items)
		if err != nil {
			failures++
			errs = append(errs, fmt.Errorf("store %s: %w", res.feed.URL, err))
			log.Printf("rssfetcher: storing items of %s: %v", res.feed.URL, err)
			s.recordFetchQuietly(ctx, res.feed.ID, FetchOutcome{Err: err})
			continue
		}
		written += n
		s.recordFetchQuietly(ctx, res.feed.ID, FetchOutcome{
			ETag:         res.etag,
			LastModified: res.lastModified,
			TTL:          res.ttl,
			Items:        n,
		})
	}

	if unchanged > 0 {
		// Worth a line of its own: it is the difference between a cycle that
		// polled forty feeds and one that was told thirty-nine times that there
		// was nothing new.
		log.Printf("mongo: %d of %d feeds answered 304 (unchanged)", unchanged, len(due))
	}

	// A cycle in which nothing was written and something failed is a failed
	// cycle, even when the failures were per-feed: this is what a health check
	// or a log alert has to be able to see.
	if len(errs) > 0 {
		err := errors.Join(errs...)
		if failures == len(due) {
			return written, fmt.Errorf("mongo: all %d feeds failed: %w", failures, err)
		}
		return written, err
	}
	return written, nil
}

func (s *Store) downloadFeeds(ctx context.Context, feeds []domain.Feed, fetcher rss.FetchFunc) []fetchResult {
	results := make([]fetchResult, len(feeds))
	sem := make(chan struct{}, MaxParallelFetches)
	var wg sync.WaitGroup

	for i := range feeds {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			// Drain what we already started before returning, so callers get a
			// complete result slice.
			wg.Wait()
			return results
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = s.fetchFeed(ctx, feeds[i], fetcher)
		}(i)
	}
	wg.Wait()
	return results
}

func (s *Store) fetchFeed(ctx context.Context, feed domain.Feed, fetcher rss.FetchFunc) (res fetchResult) {
	res = fetchResult{feed: feed}
	defer func() {
		if r := recover(); r != nil {
			// A panicking feed parser used to kill the process: this fetch ran
			// in a goroutine with no recovery at all.
			log.Printf("mongo: panic while fetching %s: %v\n%s", feed.URL, r, debug.Stack())
			res.err = fmt.Errorf("panic while fetching %s: %v", feed.URL, r)
		}
	}()
	if strings.TrimSpace(feed.URL) == "" {
		res.err = errors.New("feed has no URL")
		return res
	}
	response, err := fetcher(ctx, rss.FeedRequest{URL: feed.URL, ETag: feed.ETag, LastModified: feed.LastModified})
	if err != nil {
		res.err = err
		return res
	}
	if response == nil {
		res.err = errors.New("feed fetcher returned no response")
		return res
	}
	if response.NotModified {
		res.notModified = true
		return res
	}
	if response.Feed == nil {
		res.err = errors.New("feed fetcher returned a response without a feed")
		return res
	}

	res.items = response.Feed.Items
	// The validators are reported to the caller and stored there, in the same
	// write as the health status: splitting them meant a cycle could record the
	// one and not the other.
	res.etag = response.ETag
	res.lastModified = response.LastModified
	res.ttl = response.Feed.TTL
	return res
}

// saveNewsItems upserts the newest items of one feed with a single bulk write.
// The previous implementation issued two queries per item (find + update or
// insert), which for a handful of feeds meant thousands of round trips.
func (s *Store) saveNewsItems(ctx context.Context, feed domain.Feed, items []*rss.Item) (int, error) {
	models := NewsItemUpdates(feed, items)
	if len(models) == 0 {
		return 0, nil
	}

	ctx, cancel := s.opCtx(ctx)
	defer cancel()

	res, err := s.collection(newsCollection).BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
	if err != nil {
		return 0, fmt.Errorf("mongo: bulk write news items of %s: %w", feed.URL, err)
	}
	// MatchedCount is deliberately ignored: a document that already holds the
	// exact same values is not a change.
	if res == nil {
		return 0, nil
	}
	return int(res.UpsertedCount + res.ModifiedCount), nil
}

// NewsItemUpdates normalises items and turns them into upsert models. Clicks,
// the read flag and the first seen pubDate are intentionally left untouched on
// updates.
func NewsItemUpdates(feed domain.Feed, items []*rss.Item) []mongo.WriteModel {
	limit := rss.MaxItemsPerFeed()
	// The parsers hand us the items in document order; a feed that lists its
	// oldest entries first used to have exactly those stored.
	items = rss.NewestItems(items, limit)

	models := make([]mongo.WriteModel, 0, len(items))
	seen := make(map[string]struct{}, len(items))

	for _, item := range items {
		if len(models) >= limit {
			break
		}
		prepared, ok := PrepareNewsItem(item, feed)
		if !ok {
			continue
		}
		if _, dup := seen[prepared.GUID]; dup {
			continue
		}
		seen[prepared.GUID] = struct{}{}

		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"rssGuid": prepared.GUID}).
			SetUpdate(bson.D{
				{Key: "$set", Value: bson.D{
					{Key: "rssTitle", Value: prepared.Title},
					{Key: "rssLink", Value: prepared.Link},
					{Key: "rssDesc", Value: prepared.Content},
					{Key: "rssGuid", Value: prepared.GUID},
					{Key: "rssSource", Value: prepared.Source},
					{Key: "language", Value: prepared.Language},
					{Key: "category", Value: prepared.Category},
					{Key: "subCategory", Value: prepared.SubCategory},
					{Key: "enclosure", Value: prepared.Enclosure},
				}},
				{Key: "$setOnInsert", Value: bson.D{
					{Key: "_id", Value: prepared.ID},
					{Key: "pubDate", Value: prepared.Date},
					{Key: "clicks", Value: 0},
					{Key: "read", Value: false},
				}},
			}).
			SetUpsert(true))
	}
	return models
}

// PrepareNewsItem fills in the feed derived fields and normalises the item. It
// reports false when the item is not usable (no title, link or guid).
func PrepareNewsItem(item *rss.Item, feed domain.Feed) (*rss.Item, bool) {
	if item == nil {
		return nil, false
	}
	prepared := *item
	prepared.Title = strings.TrimSpace(prepared.Title)
	prepared.Link = strings.TrimSpace(prepared.Link)
	prepared.Content = strings.TrimSpace(prepared.Content)

	guid := NormalizeGUID(prepared.GUID)
	if guid == "" {
		guid = NormalizeGUID(prepared.Link)
	}
	if prepared.Title == "" || prepared.Link == "" || guid == "" {
		return nil, false
	}
	prepared.GUID = guid

	prepared.Language = feed.Language
	prepared.Category = feed.Category
	prepared.SubCategory = feed.EffectiveSubCategory()
	prepared.Source = feed.Name

	// Items without a date are treated as just published. A *slightly* future
	// date is normal (the publisher's clock runs a little ahead) and is kept;
	// anything more than a day in the future is a broken parse and is clamped,
	// rather than rewriting every item's publication time as the old code did.
	switch now := time.Now(); {
	case prepared.Date.IsZero():
		prepared.Date = now
	case prepared.Date.After(now.Add(24 * time.Hour)):
		prepared.Date = now
	}
	if prepared.ID.IsZero() {
		prepared.ID = bson.NewObjectID()
	}
	return &prepared, true
}

// NormalizeGUID strips the scheme of a GUID so http/https variants of the same
// item collapse into one document.
func NormalizeGUID(guid string) string {
	guid = strings.TrimSpace(guid)
	switch {
	case strings.HasPrefix(guid, "https://"):
		guid = guid[len("https://"):]
	case strings.HasPrefix(guid, "http://"):
		guid = guid[len("http://"):]
	}
	return guid
}

// ParseObjectID converts a hex string into an object id.
func ParseObjectID(id string) (bson.ObjectID, error) {
	id = strings.TrimPrefix(strings.TrimSpace(id), "/")
	if id == "" {
		return bson.ObjectID{}, fmt.Errorf("%w: id is empty", ErrInvalidID)
	}
	objectID, err := bson.ObjectIDFromHex(id)
	if err != nil {
		return bson.ObjectID{}, fmt.Errorf("%w %q: %w", ErrInvalidID, id, err)
	}
	return objectID, nil
}
