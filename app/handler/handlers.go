package handler

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jelinden/rssfetcher/app/auth"
	"github.com/jelinden/rssfetcher/app/domain"
	"github.com/jelinden/rssfetcher/app/mongo"
	"github.com/jelinden/rssfetcher/app/rss"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Store is everything the web layer needs from the persistence backend. Having
// it as an interface (instead of calling package level functions that reach for
// a global client) is what makes these handlers testable at all.
type Store interface {
	Feeds(ctx context.Context, onlyActive bool) ([]domain.Feed, error)
	Feed(ctx context.Context, id string) (*domain.Feed, error)
	SaveFeed(ctx context.Context, feed *domain.Feed, in domain.FeedInput) error
	SaveFeedItem(ctx context.Context, feed domain.Feed) error
	// FetchOne downloads a single feed now, stores its items and records the
	// outcome on the feed. It backs the "check now" button: after fixing a URL
	// the operator should not have to wait for the next cycle to learn whether
	// the publisher answers.
	FetchOne(ctx context.Context, feedID string, fetcher ...rss.FetchFunc) (mongo.FetchReport, error)
	Categories(ctx context.Context) ([]rss.Category, error)
	SubCategories(ctx context.Context) ([]rss.SubCategory, error)
	CategoryByName(ctx context.Context, name string) (rss.Category, error)
	SubCategoryByName(ctx context.Context, name string) (rss.SubCategory, error)
	SaveCategory(ctx context.Context, cat rss.Category) error
	SaveSubCategory(ctx context.Context, sub rss.SubCategory) error

	// Credentials and login sessions.
	VerifyAdminPassword(ctx context.Context, username, password string) (domain.AdminUser, error)
	CreateSession(ctx context.Context, userID bson.ObjectID, username string, ttl time.Duration) (string, domain.Session, error)
	// Session resolves a cookie value, sliding the expiry towards ttl. The bool
	// reports that the deadline moved, so the handler can re-issue the cookie.
	Session(ctx context.Context, token string, ttl time.Duration) (domain.Session, bool, error)
	DeleteSession(ctx context.Context, token string) error
	ChangePassword(ctx context.Context, username, currentPassword, newPassword string) error
}

// Handler serves the admin UI.
type Handler struct {
	store     Store
	templates *template.Template
	opts      Options
	limiter   *loginLimiter
}

// New builds the admin UI handler. It fails when the process cannot produce
// cryptographically random bytes: every CSRF token is derived from that secret,
// and the previous fallback (a timestamp) was guessable by anyone who had seen a
// log line - which is worse than refusing to start.
func New(store Store, templates *template.Template, opts Options) (*Handler, error) {
	opts.normalize()
	if store == nil {
		return nil, errors.New("handler: a store is required")
	}
	if templates == nil {
		return nil, errors.New("handler: parsed templates are required")
	}
	if opts.AuthSecret == "" {
		secret, err := auth.NewSecret()
		if err != nil {
			return nil, fmt.Errorf("handler: generating the csrf secret: %w", err)
		}
		opts.AuthSecret = secret
	}
	return &Handler{
		store:     store,
		templates: templates,
		opts:      opts,
		limiter:   newLoginLimiter(opts.LoginMaxFailures, opts.LoginWindow),
	}, nil
}

// Routes wires the handlers. The chain is: resolve the session cookie, bound the
// request body, enforce CSRF, then dispatch. Every mutating endpoint is
// restricted to POST, so an <img src="/delete/..."> or a browser prefetch can no
// longer change state, and everything except the login page and the static
// assets requires a valid session.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/static/", StaticHandler())

	// Public.
	mux.HandleFunc("/login/", h.loginRoute)
	mux.Handle("/logout/", h.method(http.MethodPost, h.Logout))

	// Authenticated.
	mux.HandleFunc("/", h.notFound)
	mux.Handle("/view/", h.requireAuth(h.method(http.MethodGet, h.ViewHandler)))
	mux.Handle("/edit/", h.requireAuth(h.method(http.MethodGet, h.EditHandler)))
	mux.Handle("/delete/", h.requireAuth(h.method(http.MethodPost, h.RemoveHandler)))
	mux.Handle("/check/", h.requireAuth(h.method(http.MethodPost, h.CheckHandler)))
	mux.Handle("/save/", h.requireAuth(h.method(http.MethodPost, h.SaveHandler)))
	mux.Handle("/save/category", h.requireAuth(h.method(http.MethodPost, h.SaveCategoryHandler)))
	mux.Handle("/save/subcategory", h.requireAuth(h.method(http.MethodPost, h.SaveSubCategoryHandler)))
	mux.Handle("/account/password", h.requireAuth(h.method(http.MethodPost, h.ChangePassword)))

	return h.authenticate(h.limitBody(h.csrf(mux)))
}

func (h *Handler) loginRoute(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPost:
		h.LoginPage(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "GET or POST required", http.StatusMethodNotAllowed)
	}
}

// limitBody bounds the request body we are willing to buffer before anything
// (including the CSRF check) reads it.
func (h *Handler) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			r.Body = http.MaxBytesReader(w, r.Body, h.opts.MaxFormBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// page builds the shared template fields: title, signed in user and the CSRF
// token every form has to carry.
func (h *Handler) page(w http.ResponseWriter, r *http.Request, title string) domain.Page {
	page := domain.Page{Title: title, CSRF: h.csrfToken(w, r)}
	if session, ok := sessionFrom(r.Context()); ok {
		page.Username = session.Username
	}
	return page
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/view/", http.StatusFound)
}

// method restricts an endpoint to one HTTP method, which together with the CSRF
// check keeps state changes away from GET requests.
func (h *Handler) method(method string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			http.Error(w, method+" required", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	})
}

func (h *Handler) ViewHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	viewPage := domain.ViewPage{Page: h.page(w, r, "Feeds")}

	viewPage.StatusFilter = strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	viewPage.Now = time.Now()
	viewPage.HonourTTL = h.opts.HonourFeedTTL
	viewPage.MaxTTL = h.opts.MaxFeedTTL

	var err error
	// Removed feeds are listed too (that is how you see what you deleted), but
	// they are not polled, so they never belong in the broken count.
	all, err := h.store.Feeds(ctx, false)
	if err != nil {
		h.fail(w, r, "loading feeds", err)
		return
	}
	viewPage.Total = len(all)
	for _, feed := range all {
		if !feed.Removed && feed.Failing() {
			viewPage.Broken++
		}
	}
	if viewPage.StatusFilter == statusBroken {
		viewPage.Feeds = activeFeeds(all)
		viewPage.Feeds = brokenFeeds(viewPage.Feeds)
	} else {
		viewPage.Feeds = all
	}
	if viewPage.Categories, err = h.store.Categories(ctx); err != nil {
		h.fail(w, r, "loading categories", err)
		return
	}
	if viewPage.SubCategories, err = h.store.SubCategories(ctx); err != nil {
		h.fail(w, r, "loading sub categories", err)
		return
	}
	if flash := r.URL.Query().Get("flash"); flash != "" {
		viewPage.Flash = flash
	}
	h.render(w, "view", &viewPage)
}

func (h *Handler) EditHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := idFromPath(r.URL.Path)
	feed, err := h.store.Feed(ctx, id)
	switch {
	case errors.Is(err, mongo.ErrNotFound), errors.Is(err, mongo.ErrInvalidID):
		// New feed: pre-fill with a fresh id so the form posts to /save/<new>.
		feed = &domain.Feed{ID: newObjectID()}
	case err != nil:
		h.fail(w, r, "loading feed", err)
		return
	}
	// Feeds stored without a sub category have a nil pointer here; the template
	// dereferences it, which used to panic.
	feed.SubCategory = feed.EffectiveSubCategory()
	if feed.SubCategory == nil {
		feed.SubCategory = &rss.SubCategory{}
	}

	page := domain.EditPage{Page: h.page(w, r, "Edit feed"), Feed: *feed}
	if page.Categories, err = h.store.Categories(ctx); err != nil {
		h.fail(w, r, "loading categories", err)
		return
	}
	if page.SubCategories, err = h.store.SubCategories(ctx); err != nil {
		h.fail(w, r, "loading sub categories", err)
		return
	}
	h.render(w, "edit", page)
}

func (h *Handler) RemoveHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := idFromPath(r.URL.Path)
	feed, err := h.store.Feed(ctx, id)
	switch {
	case errors.Is(err, mongo.ErrInvalidID):
		http.Error(w, "invalid feed id", http.StatusBadRequest)
		return
	case err != nil:
		h.fail(w, r, "loading feed "+id, err)
		return
	}
	feed.Removed = true
	if err := h.store.SaveFeedItem(ctx, *feed); err != nil {
		h.fail(w, r, "removing feed", err)
		return
	}
	log.Println("removed feed", feed.ID.Hex(), feed.Name)
	h.redirect(w, r, "removed "+feed.Name)
}

// statusBroken is the only ?status= value; anything else lists everything.
const statusBroken = "broken"

// activeFeeds drops the removed rows.
func activeFeeds(feeds []domain.Feed) []domain.Feed {
	out := make([]domain.Feed, 0, len(feeds))
	for _, feed := range feeds {
		if !feed.Removed {
			out = append(out, feed)
		}
	}
	return out
}

// brokenFeeds keeps the subscriptions whose last fetch attempt failed.
func brokenFeeds(feeds []domain.Feed) []domain.Feed {
	out := make([]domain.Feed, 0, len(feeds))
	for _, feed := range feeds {
		if feed.Failing() {
			out = append(out, feed)
		}
	}
	return out
}

// CheckHandler fetches one feed on demand and reports the result. It is a POST
// because it costs the publisher a request, and it is bounded by
// Options.RefetchTimeout rather than the cycle deadline: an operator pressing a
// button should get an answer, even if the remote host is merely stalling.
func (h *Handler) CheckHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := idFromPath(r.URL.Path)
	feed, err := h.store.Feed(ctx, id)
	switch {
	case errors.Is(err, mongo.ErrInvalidID):
		http.Error(w, "invalid feed id", http.StatusBadRequest)
		return
	case err != nil:
		h.fail(w, r, "loading feed "+id, err)
		return
	}

	ctx, cancel := context.WithTimeout(ctx, h.opts.RefetchTimeout)
	defer cancel()

	report, err := h.store.FetchOne(ctx, feed.ID.Hex())
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		h.redirect(w, r, fmt.Sprintf("checked %s: no answer within %s", feed.Name, h.opts.RefetchTimeout))
	case err != nil:
		// The failure is also recorded on the feed document, so the list shows
		// it after the redirect.
		h.redirect(w, r, fmt.Sprintf("checked %s: still failing: %v", feed.Name, err))
	case report.Unchanged:
		h.redirect(w, r, fmt.Sprintf("checked %s: the publisher said nothing has changed", feed.Name))
	case report.Items == 0:
		// A body arrived but every item in it was already stored. Saying "0 items"
		// alone would read like the feed is empty.
		h.redirect(w, r, fmt.Sprintf("checked %s: no new items", feed.Name))
	default:
		h.redirect(w, r, fmt.Sprintf("checked %s: %d items", feed.Name, report.Items))
	}
}

func (h *Handler) SaveHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	id := idFromPath(r.URL.Path)
	feed, err := h.store.Feed(ctx, id)
	switch {
	case errors.Is(err, mongo.ErrNotFound), errors.Is(err, mongo.ErrInvalidID):
		// Unknown id: treat the form as the creation of a new feed.
		feed = nil
	case err != nil:
		h.fail(w, r, "loading feed", err)
		return
	}

	in := domain.FeedInput{
		Name:     r.FormValue("name"),
		URL:      r.FormValue("url"),
		SiteURL:  r.FormValue("siteUrl"),
		Language: r.FormValue("language"),
	}
	if feed != nil {
		in.ID = feed.ID
	}
	in.Normalize()
	if err := in.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if in.Category, err = h.category(ctx, r.FormValue("category")); err != nil {
		h.fail(w, r, "resolving category", err)
		return
	}
	if in.SubCategory, err = h.subCategory(ctx, r.FormValue("subCategory")); err != nil {
		h.fail(w, r, "resolving sub category", err)
		return
	}
	if err := h.store.SaveFeed(ctx, feed, in); err != nil {
		h.fail(w, r, "saving feed", err)
		return
	}
	h.redirect(w, r, "saved "+in.Name)
}

func (h *Handler) SaveCategoryHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	cat := rss.Category{
		Name:      strings.TrimSpace(r.FormValue("categoryName")),
		EnName:    strings.TrimSpace(r.FormValue("enName")),
		StyleName: strings.TrimSpace(r.FormValue("styleName")),
	}
	if cat.Name == "" {
		http.Error(w, "category name is required", http.StatusBadRequest)
		return
	}
	if err := h.store.SaveCategory(ctx, cat); err != nil {
		h.fail(w, r, "saving category", err)
		return
	}
	h.redirect(w, r, "saved category "+cat.Name)
}

func (h *Handler) SaveSubCategoryHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	sub := rss.SubCategory{SubCategory: strings.TrimSpace(r.FormValue("subCategory"))}
	if sub.SubCategory == "" {
		http.Error(w, "sub category name is required", http.StatusBadRequest)
		return
	}
	if err := h.store.SaveSubCategory(ctx, sub); err != nil {
		h.fail(w, r, "saving sub category", err)
		return
	}
	h.redirect(w, r, "saved sub category "+sub.SubCategory)
}

// category resolves a form value into a category, creating it when it is new.
// The old code attached a random object id to the feed instead, which left the
// feed pointing at a category document that never existed.
func (h *Handler) category(ctx context.Context, name string) (rss.Category, error) {
	name = strings.TrimSpace(name)
	if isPlaceholder(name) {
		return rss.Category{}, nil
	}
	category, err := h.store.CategoryByName(ctx, name)
	if err != nil {
		return rss.Category{}, err
	}
	if category.ID.IsZero() {
		category = rss.Category{ID: newObjectID(), Name: name}
		if err := h.store.SaveCategory(ctx, category); err != nil {
			return rss.Category{}, err
		}
	}
	return category, nil
}

func (h *Handler) subCategory(ctx context.Context, name string) (rss.SubCategory, error) {
	name = strings.TrimSpace(name)
	if isPlaceholder(name) {
		return rss.SubCategory{}, nil
	}
	sub, err := h.store.SubCategoryByName(ctx, name)
	if err != nil {
		return rss.SubCategory{}, err
	}
	if sub.ID.IsZero() {
		sub = rss.SubCategory{ID: newObjectID(), SubCategory: name}
		if err := h.store.SaveSubCategory(ctx, sub); err != nil {
			return rss.SubCategory{}, err
		}
	}
	return sub, nil
}

func isPlaceholder(name string) bool {
	return name == "" || name == "0" || name == "Select"
}

// idFromPath returns the trailing id of a /<verb>/<id> path. Indexing the URL
// with a magic offset (r.URL.Path[6:]) was both fragile and wrong for paths
// such as /save/category.
func idFromPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-1]
}

func (h *Handler) render(w http.ResponseWriter, tmpl string, data any) {
	h.renderStatus(w, http.StatusOK, tmpl, data)
}

func (h *Handler) renderStatus(w http.ResponseWriter, status int, tmpl string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := h.templates.ExecuteTemplate(w, tmpl, data); err != nil {
		log.Printf("http: rendering %s: %v", tmpl, err)
	}
}

// fail logs the real error and answers with something a client can act on. The
// prefixed package errors ("mongo: conflict: ...") go to the log only.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, op string, err error) {
	log.Printf("http: %s %s: %v: %v", r.Method, r.URL.Path, op, err)
	switch {
	case errors.Is(err, mongo.ErrDuplicateFeed):
		http.Error(w, "that feed is already subscribed", http.StatusConflict)
	case errors.Is(err, mongo.ErrConflict):
		http.Error(w, "it changed under you, please try again", http.StatusConflict)
	case errors.Is(err, mongo.ErrInvalidID):
		http.Error(w, "invalid id", http.StatusBadRequest)
	case errors.Is(err, mongo.ErrNotFound):
		http.Error(w, "could not "+op, http.StatusNotFound)
	default:
		http.Error(w, "could not "+op, http.StatusInternalServerError)
	}
}

// redirect sends the browser back to the feed list (PRG pattern) with a short
// flash message.
func (h *Handler) redirect(w http.ResponseWriter, r *http.Request, flash string) {
	target := "/view/"
	if flash != "" {
		target += "?flash=" + url.QueryEscape(flash)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// WithTimeout bounds a request handler; the server wide timeouts only cover the
// network layer.
func WithTimeout(d time.Duration, next http.Handler) http.Handler {
	return http.TimeoutHandler(next, d, "request timed out")
}

func newObjectID() bson.ObjectID { return bson.NewObjectID() }
