package handler

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jelinden/rssfetcher/app/domain"
	"github.com/jelinden/rssfetcher/app/rss"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRoutesRequireTheRightMethod(t *testing.T) {
	store := newFakeStore()
	seedFeed(store, domain.Feed{Name: "Existing", URL: "http://example.com/a"})
	c := authenticatedClient(t, store)

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/view/", http.StatusOK},
		{http.MethodPost, "/view/", http.StatusMethodNotAllowed},
		{http.MethodGet, "/delete/" + bson.NewObjectID().Hex(), http.StatusMethodNotAllowed},
		{http.MethodGet, "/save/" + bson.NewObjectID().Hex(), http.StatusMethodNotAllowed},
		{http.MethodGet, "/save/category", http.StatusMethodNotAllowed},
		{http.MethodPost, "/save/category", http.StatusBadRequest}, // empty name
		{http.MethodGet, "/nope/", http.StatusNotFound},
		{http.MethodGet, "/", http.StatusFound},
	} {
		var rec *httptest.ResponseRecorder
		if tc.method == http.MethodGet {
			rec = c.get(tc.path)
		} else {
			rec = c.post(tc.path, nil)
		}
		if rec.Code != tc.want {
			t.Errorf("%s %s = %d, want %d (%q)", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestViewHandlerRendersFeeds(t *testing.T) {
	store := newFakeStore()
	// A feed without any sub category: the template must not panic on the nil
	// pointer that the omitempty bson tag produces.
	seedFeed(store, domain.Feed{ID: bson.NewObjectID(), Name: "Without sub", URL: "http://example.com/a"})
	seedFeed(store, domain.Feed{ID: bson.NewObjectID(), Name: "Removed one", URL: "http://example.com/b", Removed: true})

	body := authenticatedClient(t, store).get("/view/?flash=saved").Body.String()
	for _, want := range []string{"Without sub", "Removed one", "saved"} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not contain %q", want)
		}
	}
	// Only a removed feed may render as removed; the delete control must be a
	// POST form, not a link a prefetch can trigger.
	if strings.Contains(body, `href="/delete/`) {
		t.Error("delete must not be a GET link")
	}
	if n := strings.Count(body, `name="csrf_token"`); n < 4 {
		t.Errorf("only %d csrf tokens in the view page, every POST form needs one", n)
	}
}

func TestViewHandlerReportsStoreErrors(t *testing.T) {
	store := newFakeStore()
	c := authenticatedClient(t, store)
	store.feedsErr = errors.New("boom")

	rec := c.get("/view/")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("internal error details leaked to the client")
	}
}

func TestEditHandler(t *testing.T) {
	store := newFakeStore()
	catID := bson.NewObjectID()
	store.categories["Tech"] = rss.Category{ID: catID, Name: "Tech"}
	store.subCategories["Space"] = rss.SubCategory{ID: bson.NewObjectID(), SubCategory: "Space"}
	id := seedFeed(store, domain.Feed{
		Name:        "Existing",
		URL:         "http://example.com/feed",
		Language:    "sv",
		Category:    rss.Category{ID: catID, Name: "Tech"},
		SubCategory: &rss.SubCategory{ID: bson.NewObjectID(), SubCategory: "Space"},
	})

	c := authenticatedClient(t, store)

	body := c.get("/edit/" + id.Hex()).Body.String()
	if !strings.Contains(body, `value="Existing"`) {
		t.Error("feed name not prefilled")
	}
	if !strings.Contains(body, `value="Tech" selected`) {
		t.Error("category of the feed should be preselected")
	}
	if !strings.Contains(body, `value="Space" selected`) {
		t.Error("sub category of the feed should be preselected")
	}

	// Unknown and malformed ids open an empty form instead of panicking.
	for _, path := range []string{"/edit/", "/edit/" + bson.NewObjectID().Hex(), "/edit/not-hex"} {
		rec := c.get(path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200 (new feed form), body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestEditHandlerPropagatesStoreErrors(t *testing.T) {
	store := newFakeStore()
	id := seedFeed(store, domain.Feed{Name: "x", URL: "http://example.com/x"})
	c := authenticatedClient(t, store)
	store.feedErr = errors.New("connection reset")

	if rec := c.get("/edit/" + id.Hex()); rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestSaveHandlerCreatesFeed(t *testing.T) {
	store := newFakeStore()
	store.categories["Tech"] = rss.Category{ID: bson.NewObjectID(), Name: "Tech"}
	c := authenticatedClient(t, store)

	rec := c.post("/save/", map[string]string{
		"name": "New feed", "url": "https://example.com/feed.xml", "siteUrl": "example.com",
		"category": "Tech", "language": "sv",
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303, body = %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/view/") {
		t.Errorf("Location = %q, want a redirect to the feed list", loc)
	}
	if len(store.feeds) != 1 {
		t.Fatalf("feeds = %+v, want exactly one stored", store.feeds)
	}
	stored := store.savedFeeds[0]
	if stored.Name != "New feed" || stored.URL != "https://example.com/feed.xml" {
		t.Errorf("stored input = %+v", stored)
	}
	if stored.Category.Name != "Tech" || stored.Category.ID.IsZero() {
		t.Errorf("category = %+v, want the existing Tech category", stored.Category)
	}
	if !strings.HasPrefix(stored.SiteURL, "https://") {
		t.Errorf("siteUrl = %q, want the missing scheme to be defaulted", stored.SiteURL)
	}
}

func TestSaveHandlerValidatesInput(t *testing.T) {
	c := authenticatedClient(t, newFakeStore())

	for name, values := range map[string]map[string]string{
		"missing name":   {"name": "", "url": "https://example.com/feed.xml"},
		"missing url":    {"name": "n", "url": ""},
		"non http url":   {"name": "n", "url": "file:///etc/passwd"},
		"no host":        {"name": "n", "url": "https://"},
		"javascript url": {"name": "n", "url": "javascript:alert(1)"},
	} {
		rec := c.post("/save/", values)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body=%s)", name, rec.Code, rec.Body.String())
		}
	}
}

func TestSaveHandlerReportsStoreErrors(t *testing.T) {
	store := newFakeStore()
	c := authenticatedClient(t, store)
	store.saveErr = errors.New("write concern failed")

	rec := c.post("/save/", map[string]string{"name": "n", "url": "https://example.com/feed.xml"})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestSaveHandlerRejectsOversizedBody(t *testing.T) {
	c := authenticatedClient(t, newFakeStore())
	pad := strings.Repeat("x", 8<<20)
	rec := c.postRaw("/save/", "name=n&url=https://example.com/feed.xml&language="+pad+"&csrf_token="+c.csrf)
	if rec.Code == http.StatusSeeOther {
		t.Error("an 8MB form body was accepted, want it to be rejected")
	}
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 400/413", rec.Code)
	}
}

// A form value naming an unknown category used to be stored with a freshly
// generated object id, pointing at a category document that never existed.
func TestSaveHandlerCreatesUnknownCategory(t *testing.T) {
	store := newFakeStore()
	c := authenticatedClient(t, store)

	rec := c.post("/save/", map[string]string{
		"name": "n", "url": "https://example.com/feed.xml", "category": "Science", "subCategory": "Space",
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	cat, ok := store.categories["Science"]
	if !ok || cat.ID.IsZero() {
		t.Errorf("category Science was not stored properly: %+v", store.categories)
	}
	if sub, ok := store.subCategories["Space"]; !ok || sub.ID.IsZero() {
		t.Errorf("sub category Space was not stored: %+v", store.subCategories)
	}
}

func TestRemoveHandlerMarksFeedRemoved(t *testing.T) {
	store := newFakeStore()
	id := seedFeed(store, domain.Feed{Name: "Doomed", URL: "http://example.com/d"})
	c := authenticatedClient(t, store)

	if rec := c.post("/delete/"+id.Hex(), nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if !store.feeds[id].Removed {
		t.Errorf("feed not marked removed: %+v", store.feeds[id])
	}

	// Unknown ids must not be silently ignored, and they are the client's
	// problem (404), not a server fault (500).
	if rec := c.post("/delete/"+bson.NewObjectID().Hex(), nil); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a missing feed", rec.Code)
	}
	if rec := c.post("/delete/garbage", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a malformed id", rec.Code)
	}
}

func TestSaveCategoryHandlers(t *testing.T) {
	c := authenticatedClient(t, newFakeStore())

	if rec := c.post("/save/category", map[string]string{"categoryName": "Tech", "enName": "Tech"}); rec.Code != http.StatusSeeOther {
		t.Fatalf("category save status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec := c.post("/save/category", map[string]string{"categoryName": "   "}); rec.Code != http.StatusBadRequest {
		t.Errorf("empty category name = %d, want 400", rec.Code)
	}
	if rec := c.post("/save/subcategory", map[string]string{"subCategory": "Space"}); rec.Code != http.StatusSeeOther {
		t.Fatalf("sub category save status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec := c.post("/save/subcategory", map[string]string{}); rec.Code != http.StatusBadRequest {
		t.Errorf("empty sub category name = %d, want 400", rec.Code)
	}
}

func TestRecovererTurnsPanicsInto500(t *testing.T) {
	h := Recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("handler exploded")
	}))
	if rec := newClient(t, h).get("/"); rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	rec := newClient(t, SecurityHeaders(false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))).get("/")
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
}

func TestSafeNext(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "/view/"},
		{"/edit/123", "/edit/123"},
		{"/view/?flash=x", "/view/?flash=x"},
		{"https://evil.example/", "/view/"},
		{"//evil.example/", "/view/"},
		{"http://evil.example", "/view/"},
		{"\\evil.example", "/view/"},
		{"javascript:alert(1)", "/view/"},
	} {
		if got := safeNext(tc.in); got != tc.want {
			t.Errorf("safeNext(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
