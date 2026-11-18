package handler

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jelinden/rssfetcher/app/auth"
	"github.com/jelinden/rssfetcher/app/domain"
	"github.com/jelinden/rssfetcher/app/mongo"
	"github.com/jelinden/rssfetcher/app/rss"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// fakeStore is an in memory Store, so the handlers can be exercised without a
// database (which was impossible before: they called package level functions
// that reached for a global client).

const (
	testUser     = "admin"
	testPassword = "test-password-123"
	testSecret   = "unit-test-csrf-secret"
)

// cheapHasher keeps scrypt affordable in tests.
func cheapHasher() *auth.Hasher {
	return &auth.Hasher{Params: auth.Params{N: 2, R: 1, P: 1, KeyLen: 16}}
}

type fakeStore struct {
	feeds         map[bson.ObjectID]domain.Feed
	categories    map[string]rss.Category
	subCategories map[string]rss.SubCategory
	users         map[string]domain.AdminUser
	sessions      map[string]domain.Session // keyed by token hash

	// Recorded calls of Session (see TestSessionLookupUsesTheConfiguredTTL...).
	lastSessionTTL         time.Duration
	lastSessionExtended    bool
	lastSessionExtendedFor string

	feedsErr       error
	feedErr        error
	saveErr        error
	verifyErr      error
	savedFeeds     []domain.FeedInput
	savedCats      []rss.Category
	savedSubs      []rss.SubCategory
	passwordChange []string
	droppedUsers   []bson.ObjectID
}

func newFakeStore() *fakeStore {
	store := &fakeStore{
		feeds:         map[bson.ObjectID]domain.Feed{},
		categories:    map[string]rss.Category{},
		subCategories: map[string]rss.SubCategory{},
		users:         map[string]domain.AdminUser{},
		sessions:      map[string]domain.Session{},
	}
	hash, err := cheapHasher().Hash(testPassword)
	if err != nil {
		panic(err)
	}
	store.users[testUser] = domain.AdminUser{
		ID:           bson.NewObjectID(),
		Username:     testUser,
		PasswordHash: hash,
		CreatedAt:    time.Now(),
	}
	return store
}

func (f *fakeStore) Feeds(ctx context.Context, onlyActive bool) ([]domain.Feed, error) {
	if f.feedsErr != nil {
		return nil, f.feedsErr
	}
	out := []domain.Feed{}
	for _, feed := range f.feeds {
		if onlyActive && feed.Removed {
			continue
		}
		out = append(out, feed)
	}
	return out, nil
}

func (f *fakeStore) Feed(ctx context.Context, id string) (*domain.Feed, error) {
	if f.feedErr != nil {
		return nil, f.feedErr
	}
	objectID, err := mongo.ParseObjectID(id)
	if err != nil {
		return nil, err
	}
	feed, ok := f.feeds[objectID]
	if !ok {
		return nil, mongo.ErrNotFound
	}
	return &feed, nil
}

func (f *fakeStore) SaveFeed(ctx context.Context, feed *domain.Feed, in domain.FeedInput) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.savedFeeds = append(f.savedFeeds, in)
	f.feeds[in.IDOrNew()] = domain.Feed{
		ID:          in.IDOrNew(),
		Name:        in.Name,
		URL:         in.URL,
		SiteURL:     in.SiteURL,
		Language:    in.Language,
		Category:    in.Category,
		SubCategory: &rss.SubCategory{ID: in.SubCategory.ID, SubCategory: in.SubCategory.SubCategory},
	}
	if in.Category.Name != "" {
		f.categories[in.Category.Name] = in.Category
	}
	return nil
}

func (f *fakeStore) SaveFeedItem(ctx context.Context, feed domain.Feed) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.savedFeeds = append(f.savedFeeds, domain.FeedInput{ID: feed.ID, Name: feed.Name, URL: feed.URL})
	f.feeds[feed.ID] = feed
	return nil
}

func (f *fakeStore) Categories(ctx context.Context) ([]rss.Category, error) {
	if f.feedsErr != nil {
		return nil, f.feedsErr
	}
	out := []rss.Category{}
	for _, c := range f.categories {
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeStore) SubCategories(ctx context.Context) ([]rss.SubCategory, error) {
	if f.feedsErr != nil {
		return nil, f.feedsErr
	}
	out := []rss.SubCategory{}
	for _, s := range f.subCategories {
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeStore) CategoryByName(ctx context.Context, name string) (rss.Category, error) {
	return f.categories[name], nil
}

func (f *fakeStore) SubCategoryByName(ctx context.Context, name string) (rss.SubCategory, error) {
	return f.subCategories[name], nil
}

func (f *fakeStore) SaveCategory(ctx context.Context, cat rss.Category) error {
	if strings.TrimSpace(cat.Name) == "" {
		return errors.New("category name is required")
	}
	if existing, ok := f.categories[cat.Name]; ok {
		cat.ID = existing.ID
		f.categories[cat.Name] = cat
		return nil
	}
	f.savedCats = append(f.savedCats, cat)
	f.categories[cat.Name] = cat
	return nil
}

func (f *fakeStore) SaveSubCategory(ctx context.Context, sub rss.SubCategory) error {
	if strings.TrimSpace(sub.SubCategory) == "" {
		return errors.New("sub category name is required")
	}
	if existing, ok := f.subCategories[sub.SubCategory]; ok {
		sub.ID = existing.ID
		f.subCategories[sub.SubCategory] = sub
		return nil
	}
	f.savedSubs = append(f.savedSubs, sub)
	f.subCategories[sub.SubCategory] = sub
	return nil
}

func (f *fakeStore) VerifyAdminPassword(ctx context.Context, username, password string) (domain.AdminUser, error) {
	if f.verifyErr != nil {
		return domain.AdminUser{}, f.verifyErr
	}
	user, ok := f.users[strings.TrimSpace(username)]
	if !ok {
		return domain.AdminUser{}, mongo.ErrInvalidCredentials
	}
	ok, err := auth.Verify(user.PasswordHash, password)
	if err != nil || !ok {
		return domain.AdminUser{}, mongo.ErrInvalidCredentials
	}
	user.PasswordHash = ""
	return user, nil
}

func (f *fakeStore) CreateSession(ctx context.Context, userID bson.ObjectID, username string, ttl time.Duration) (string, domain.Session, error) {
	token, err := auth.NewToken()
	if err != nil {
		return "", domain.Session{}, err
	}
	session := domain.Session{
		ID:        bson.NewObjectID(),
		TokenHash: auth.TokenHash(token),
		UserID:    userID,
		Username:  username,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(ttl),
	}
	f.sessions[session.TokenHash] = session
	return token, session, nil
}

// lastSession* record how the handler called Session, so the TTL plumbing can be
// asserted without a database.
func (f *fakeStore) Session(ctx context.Context, token string, ttl time.Duration) (domain.Session, bool, error) {
	f.lastSessionTTL = ttl
	f.lastSessionExtendedFor = ""
	session, ok := f.sessions[auth.TokenHash(token)]
	if !ok {
		return domain.Session{}, false, mongo.ErrSessionExpired
	}
	if session.Expired(time.Now()) {
		return domain.Session{}, false, mongo.ErrSessionExpired
	}
	// Mirror the store's renewal rule: less than half the lifetime left.
	if remaining := time.Until(session.ExpiresAt); remaining > 0 && remaining < ttl/2 {
		session.ExpiresAt = time.Now().Add(ttl)
		f.sessions[session.TokenHash] = session
		f.lastSessionExtended = true
		f.lastSessionExtendedFor = token
		return session, true, nil
	}
	f.lastSessionExtended = false
	return session, false, nil
}

func (f *fakeStore) DeleteSession(ctx context.Context, token string) error {
	delete(f.sessions, auth.TokenHash(token))
	return nil
}

func (f *fakeStore) ChangePassword(ctx context.Context, username, currentPassword, newPassword string) error {
	f.passwordChange = append(f.passwordChange, username)
	if _, err := f.VerifyAdminPassword(ctx, username, currentPassword); err != nil {
		return err
	}
	if err := auth.ValidatePassword(newPassword); err != nil {
		return fmt.Errorf("%w: %v", mongo.ErrWeakPassword, err)
	}
	user := f.users[username]
	hash, err := cheapHasher().Hash(newPassword)
	if err != nil {
		return err
	}
	user.PasswordHash = hash
	f.users[username] = user
	if err := f.DeleteUserSessions(ctx, user.ID); err != nil {
		return err
	}
	return nil
}

func (f *fakeStore) DeleteUserSessions(ctx context.Context, userID bson.ObjectID) error {
	f.droppedUsers = append(f.droppedUsers, userID)
	for hash, session := range f.sessions {
		if session.UserID == userID {
			delete(f.sessions, hash)
		}
	}
	return nil
}

func testTemplates(t *testing.T) *template.Template {
	t.Helper()
	// The UI is embedded in the binary, so no working directory and no
	// "public/tmpl" on disk is involved any more. Before this, templates were
	// parsed in a package level var and the test binary panicked during init.
	tmpl, err := LoadTemplates("")
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}
	return tmpl
}

func testOptions() Options {
	return Options{
		AuthSecret:       testSecret,
		SessionTTL:       time.Hour,
		LoginMaxFailures: 3,
		LoginWindow:      time.Minute,
	}
}

func testHandler(t *testing.T, store Store) http.Handler {
	t.Helper()
	return Recoverer(mustNew(t, store, testTemplates(t), testOptions()).Routes())
}

// mustNew is the test side of New: the options below always work, so an error is
// a broken fixture rather than a condition to handle.
func mustNew(t *testing.T, store Store, templates *template.Template, opts Options) *Handler {
	t.Helper()
	h, err := New(store, templates, opts)
	if err != nil {
		t.Fatalf("handler.New: %v", err)
	}
	return h
}

var csrfPattern = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

func extractCSRF(body string) string {
	m := csrfPattern.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

// client keeps the cookies of a logged in browser and knows the CSRF token of
// the page it last loaded.
type client struct {
	t        *testing.T
	handler  http.Handler
	cookies  []*http.Cookie
	csrf     string
	username string
	password string
	// reqIP controls the peer address, so the login throttle can be tested per
	// client.
	reqIP string
}

func newClient(t *testing.T, h http.Handler) *client {
	return &client{t: t, handler: h, username: testUser, password: testPassword, reqIP: "192.0.2.10"}
}

func (c *client) request(method, path string, body string) *httptest.ResponseRecorder {
	c.t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.RemoteAddr = c.reqIP + ":55555"
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, cookie := range c.cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	c.absorb(rec)
	return rec
}

func (c *client) absorb(rec *httptest.ResponseRecorder) {
	resp := rec.Result()
	var zeroTime time.Time
	for _, cookie := range resp.Cookies() {
		expired := cookie.Expires != zeroTime && cookie.Expires.Before(time.Now())
		if cookie.MaxAge < 0 || expired {
			for i, existing := range c.cookies {
				if existing.Name == cookie.Name {
					c.cookies = append(c.cookies[:i], c.cookies[i+1:]...)
					break
				}
			}
			continue
		}
		replaced := false
		for i, existing := range c.cookies {
			if existing.Name == cookie.Name {
				c.cookies[i] = cookie
				replaced = true
				break
			}
		}
		if !replaced {
			c.cookies = append(c.cookies, cookie)
		}
	}
}

func (c *client) get(path string) *httptest.ResponseRecorder {
	rec := c.request(http.MethodGet, path, "")
	if token := extractCSRF(rec.Body.String()); token != "" {
		c.csrf = token
	}
	return rec
}

func (c *client) post(path string, values map[string]string) *httptest.ResponseRecorder {
	form := []string{}
	for k, v := range values {
		form = append(form, k+"="+v)
	}
	form = append(form, "csrf_token="+c.csrf)
	return c.request(http.MethodPost, path, strings.Join(form, "&"))
}

// postRaw sends a form without the CSRF token (for testing the guard).
func (c *client) postRaw(path, body string) *httptest.ResponseRecorder {
	return c.request(http.MethodPost, path, body)
}

// login performs the real login flow, so the tests exercise the same code path
// a browser does.
func (c *client) login() *httptest.ResponseRecorder {
	c.get("/login/")
	if c.csrf == "" {
		c.t.Fatal("login form has no csrf token")
	}
	return c.post("/login/", map[string]string{"username": c.username, "password": c.password})
}

func (c *client) cookie(name string) *http.Cookie {
	for _, cookie := range c.cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func seedFeed(store *fakeStore, feed domain.Feed) bson.ObjectID {
	if feed.ID.IsZero() {
		feed.ID = bson.NewObjectID()
	}
	store.feeds[feed.ID] = feed
	return feed.ID
}

func authenticatedClient(t *testing.T, store *fakeStore) *client {
	t.Helper()
	c := newClient(t, testHandler(t, store))
	rec := c.login()
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	}
	if c.cookie("rssfetcher_session") == nil {
		t.Fatal("login did not set a session cookie")
	}
	if c.get("/view/").Code != http.StatusOK {
		t.Fatal("authenticated /view/ is not reachable")
	}
	return c
}
