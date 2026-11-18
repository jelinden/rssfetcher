package handler

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jelinden/rssfetcher/app/auth"
	"github.com/jelinden/rssfetcher/app/domain"
	"github.com/jelinden/rssfetcher/app/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// H1: /\evil.com parses as a relative path (so Host == "") but browsers normalise
// the backslash and follow it as //evil.com, i.e. an external host.
func TestSafeNextKeepsRedirectsLocal(t *testing.T) {
	for _, in := range []string{"/view/", "/edit/", "/view/?flash=ok", "/edit/abc123"} {
		if got := safeNext(in); got != in {
			t.Errorf("safeNext(%q) = %q, want it unchanged", in, got)
		}
	}
	for _, in := range []string{
		`/\evil.com`, `/\/evil.com`, `\evil.com`, `\\evil.com`, `//evil.com`,
		`///evil.com`, `http://evil.com`, `//evil.com/x`, `javascript:alert(1)`,
		`/\evil.com/x?y=1`, "", "/",
	} {
		if got := safeNext(in); got != "/view/" {
			t.Errorf("safeNext(%q) = %q, want the safe fallback /view/", in, got)
		}
	}
}

func TestLoginRedirectIsAlwaysLocal(t *testing.T) {
	for _, next := range []string{`/\evil.com`, `//evil.com`, `/view/?flash=hi`} {
		store := newFakeStore()
		c := newClient(t, testHandler(t, store))
		c.get("/login/?next=" + next)
		rec := c.postRaw("/login/", "username="+testUser+"&password="+testPassword+"&csrf_token="+c.csrf)
		loc := rec.Header().Get("Location")
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("next=%q: status = %d, want 303", next, rec.Code)
		}
		if strings.HasPrefix(loc, `/\`) || strings.HasPrefix(loc, `//`) || strings.Contains(loc, "evil.com") {
			t.Errorf("next=%q redirected to %q", next, loc)
		}
	}
}

// H2: the store has to be told which TTL this handler runs with, and the cookie
// must track the database's deadline - otherwise the browser still drops the
// cookie at the original expiry however far the store slid it.
func TestSessionLookupUsesTheConfiguredTTLAndReissuesTheCookie(t *testing.T) {
	store := newFakeStore()
	h := mustNew(t, store, testTemplates(t), Options{AuthSecret: testSecret, SessionTTL: 30 * time.Minute})
	c := newClient(t, h.Routes())

	// A session with 10 minutes left of a 30 minute TTL is inside the renewal
	// window (half the lifetime), so the store must extend it.
	store.sessions[auth.TokenHash("tok")] = domain.Session{
		ID:        bson.NewObjectID(),
		TokenHash: auth.TokenHash("tok"),
		Username:  testUser,
		CreatedAt: time.Now().Add(-20 * time.Minute),
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}

	c.cookies = append(c.cookies, &http.Cookie{Name: "rssfetcher_session", Value: "tok"})
	c.request(http.MethodGet, "/view/", "")
	if got := store.lastSessionTTL; got != 30*time.Minute {
		t.Errorf("store.Session got ttl %s, want the configured 30m", got)
	}
	if !store.lastSessionExtended {
		t.Fatal("a session inside the renewal window must be reported as extended")
	}
	if store.lastSessionExtendedFor == "" {
		t.Error("the handler must re-issue the cookie when the store extended the session")
	}
	cookie := c.cookie("rssfetcher_session")
	if cookie == nil {
		t.Fatal("no session cookie after the renewal")
	}
	if got := time.Duration(cookie.MaxAge) * time.Second; got > 30*time.Minute || got < 20*time.Minute {
		t.Errorf("cookie Max-Age = %s, want it to track the extended deadline", got)
	}
}

// M1: stale clients must be reclaimed, or every address that ever fails a login
// is remembered forever.
func TestLoginLimiterPrunesStaleEntries(t *testing.T) {
	l := newLoginLimiter(3, 20*time.Millisecond)
	for i := 0; i < 4000; i++ {
		l.failure("198.51.100." + itoa(i%250+1))
	}
	time.Sleep(30 * time.Millisecond)
	for i := 0; i < 4000; i++ {
		l.failure("203.0.113." + itoa(i%250+1))
	}
	l.mu.Lock()
	size := len(l.attempts)
	l.mu.Unlock()
	if size > loginLimiterMaxEntries {
		t.Fatalf("limiter kept %d entries (limit %d): stale clients are never reclaimed", size, loginLimiterMaxEntries)
	}
}

// M7: /static/ must not hand out a listing, and a cached stylesheet with no
// content hash in its name has to revalidate.
func TestStaticHandlerServesOnlyFiles(t *testing.T) {
	srv := httptest.NewServer(StaticHandler())
	defer srv.Close()

	for _, path := range []string{"/static/", "/static"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body := readAllT(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (a listing leaks asset names): %s", path, resp.StatusCode, body)
		}
	}

	resp, err := http.Get(srv.URL + "/static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /static/style.css = %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "must-revalidate") {
		t.Errorf("Cache-Control = %q, want revalidation so a deploy is visible", cc)
	}
}

// L15: the response body must not carry package-prefixed error text.
func TestChangePasswordWeaknessMessageIsUserFacing(t *testing.T) {
	c := newClient(t, testHandler(t, newFakeStore()))
	c.login()
	c.get("/view/") // picks up the session bound CSRF token
	rec := c.post("/account/password", map[string]string{"currentPassword": testPassword, "newPassword": "short"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, leak := range []string{"mongo:", "auth:"} {
		if strings.Contains(body, leak) {
			t.Errorf("response leaks internals (%q): %s", leak, body)
		}
	}
	if !strings.Contains(body, fmt.Sprintf("at least %d characters", auth.MinPasswordLen)) {
		t.Errorf("response should state the minimum length: %s", body)
	}
}

// L16: the CSP must match what the templates actually use.
func TestSecurityHeadersCSPMatchesTheTemplates(t *testing.T) {
	rec := httptest.NewRecorder()
	SecurityHeaders(false)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`<p>ok</p>`))
	})).ServeHTTP(rec, httptest.NewRequest("GET", "/view/", nil))

	csp := rec.Header().Get("Content-Security-Policy")
	if strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP allows inline styles, but no template uses one: %s", csp)
	}
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP missing frame-ancestors: %s", csp)
	}
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS sent without the operator opting in: %q", got)
	}

	// ...but it must be there when the cookie is marked Secure, i.e. TLS.
	rec = httptest.NewRecorder()
	SecurityHeaders(true)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rec, httptest.NewRequest("GET", "/view/", nil))
	if got := rec.Header().Get("Strict-Transport-Security"); !strings.HasPrefix(got, "max-age=") {
		t.Errorf("HSTS with -cookie-secure = %q, want a max-age", got)
	}
}

// The templates must not need inline styles or scripts.
func TestEmbeddedTemplatesUseNoInlineStylesOrScripts(t *testing.T) {
	for _, name := range TemplateNames() {
		raw, err := templateFS.ReadFile("tmpl/" + name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		body := string(raw)
		if strings.Contains(body, "style=") || strings.Contains(body, "<script") || strings.Contains(body, "onclick=") {
			t.Errorf("%s uses inline styles or scripts, which the CSP forbids", name)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var out []byte
	for i > 0 {
		out = append([]byte{byte('0' + i%10)}, out...)
		i /= 10
	}
	return string(out)
}

func readAllT(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 512)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			return sb.String()
		}
	}
}

// L12: if crypto/rand is unavailable the CSRF secret used to fall back to a
// timestamp, which anyone who had seen the log line could reproduce. Refusing to
// build the handler is the honest answer; a nil store or nil templates would
// otherwise surface as a panic on the first request.
func TestNewRefusesToBuildABrokenHandler(t *testing.T) {
	templates := testTemplates(t)
	if _, err := New(nil, templates, testOptions()); err == nil {
		t.Error("New accepted a nil store")
	}
	if _, err := New(newFakeStore(), nil, testOptions()); err == nil {
		t.Error("New accepted nil templates")
	}
	h, err := New(newFakeStore(), templates, testOptions())
	if err != nil {
		t.Fatalf("New with a valid configuration: %v", err)
	}
	if h.opts.AuthSecret == "" {
		t.Error("an empty AuthSecret must be replaced by a generated one")
	}
}

// A conflict from the store (duplicate subscription) is something the operator can
// act on, so it must not surface as a 500 - and it must not leak the package
// prefixed error text into the body.
func TestDuplicateFeedIsAConflictNotACrash(t *testing.T) {
	store := newFakeStore()
	store.saveErr = fmt.Errorf("%w: %s is already subscribed", mongo.ErrDuplicateFeed, "https://example.com/a.xml")
	c := newClient(t, testHandler(t, store))
	c.login()
	c.get("/view/")

	rec := c.post("/save/1", map[string]string{
		"name": "dup", "url": "https://example.com/a.xml", "language": "en",
		"category": "tech", "subCategory": "", "siteUrl": "",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, leak := range []string{"mongo:", "handler:"} {
		if strings.Contains(body, leak) {
			t.Errorf("body leaks internals (%q): %s", leak, body)
		}
	}
	if !strings.Contains(strings.ToLower(body), "already") {
		t.Errorf("body = %q, want a message the operator can act on", body)
	}

	// An unexpected failure stays a 500 with a generic body.
	store.saveErr = errors.New("connection reset by peer")
	rec = c.post("/save/1", map[string]string{
		"name": "other", "url": "https://example.com/b.xml", "language": "en",
		"category": "", "subCategory": "", "siteUrl": "",
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "connection reset") {
		t.Errorf("an internal failure leaked to the client: %s", rec.Body.String())
	}
}
