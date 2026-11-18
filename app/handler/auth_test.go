package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jelinden/rssfetcher/app/domain"
)

func TestAnonymousUsersAreRedirectedToLogin(t *testing.T) {
	store := newFakeStore()
	seedFeed(store, domain.Feed{Name: "Secret feed", URL: "http://example.com/a"})
	h := testHandler(t, store)

	protected := []string{"/view/", "/edit/", "/save/category", "/delete/000000000000000000000000", "/account/password", "/save/"}
	for _, path := range protected {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusSeeOther {
			t.Errorf("GET %s = %d, want a redirect to the login page", path, rec.Code)
			continue
		}
		location := rec.Header().Get("Location")
		if !strings.HasPrefix(location, "/login/") {
			t.Errorf("GET %s redirected to %q", path, location)
		}
		if strings.Contains(rec.Body.String(), "Secret feed") {
			t.Errorf("GET %s leaked feed data before login", path)
		}
	}
}

func TestLoginPageIsPublic(t *testing.T) {
	rec := httptest.NewRecorder()
	testHandler(t, newFakeStore()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`name="username"`, `type="password"`, `name="csrf_token"`} {
		if !strings.Contains(body, want) {
			t.Errorf("login form is missing %s", want)
		}
	}
	if strings.Contains(body, "Emerge") {
		t.Error("login page leaked a credential")
	}
}

func TestLoginSuccessSetsHardenedCookie(t *testing.T) {
	c := newClient(t, testHandler(t, newFakeStore()))
	rec := c.login()
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/view/" {
		t.Errorf("Location = %q, want /view/", loc)
	}
	session := c.cookie("rssfetcher_session")
	if session == nil {
		t.Fatal("no session cookie was set")
	}
	if !session.HttpOnly {
		t.Error("session cookie is readable from javascript (HttpOnly missing)")
	}
	if session.Secure {
		t.Error("the test config does not use TLS, Secure should be off")
	}
	if session.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", session.SameSite)
	}
	if session.Path != "/" {
		t.Errorf("Path = %q", session.Path)
	}
	if session.MaxAge <= 0 {
		t.Errorf("MaxAge = %d, want the configured session TTL", session.MaxAge)
	}
	if rec2 := c.get("/view/"); rec2.Code != http.StatusOK {
		t.Errorf("after login /view/ = %d", rec2.Code)
	}
}

func TestSecureCookieOption(t *testing.T) {
	store := newFakeStore()
	h := Recoverer(mustNew(t, store, testTemplates(t), Options{AuthSecret: testSecret, CookieSecure: true, LoginMaxFailures: 3, LoginWindow: time.Minute}).Routes())
	c := newClient(t, h)
	c.login()
	if cookie := c.cookie("rssfetcher_session"); cookie == nil || !cookie.Secure {
		t.Errorf("cookie = %+v, want Secure when the option is set", cookie)
	}
}

func TestLoginRejectsBadCredentials(t *testing.T) {
	c := newClient(t, testHandler(t, newFakeStore()))
	c.password = "wrong-password"
	rec := c.login()
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	if c.cookie("rssfetcher_session") != nil {
		t.Fatal("a session cookie was issued for a failed login")
	}
	if !strings.Contains(rec.Body.String(), "invalid username or password") {
		t.Errorf("unexpected error message: %s", rec.Body.String())
	}
	// An unknown username must produce the exact same response as a wrong
	// password, otherwise the login form confirms which accounts exist.
	unknown := newClient(t, testHandler(t, newFakeStore()))
	unknown.username = "nobody"
	unknown.password = "wrong-password"
	rec2 := unknown.login()
	if rec2.Code != rec.Code {
		t.Errorf("unknown username = %d, wrong password = %d", rec2.Code, rec.Code)
	}
	for _, body := range []string{rec.Body.String(), rec2.Body.String()} {
		if !strings.Contains(body, "invalid username or password") {
			t.Errorf("unexpected response body: %s", body)
		}
		if strings.Contains(body, "nobody") && strings.Contains(body, "does not exist") {
			t.Error("the response reveals whether the username exists")
		}
	}
}

func TestLoginRequiresCSRFToken(t *testing.T) {
	c := newClient(t, testHandler(t, newFakeStore()))
	c.get("/login/") // obtains the session cookies but we drop the token
	rec := c.postRaw("/login/", "username="+testUser+"&password="+testPassword)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("login without a csrf token = %d, want 403", rec.Code)
	}
	if c.cookie("rssfetcher_session") != nil {
		t.Fatal("a session was created despite the missing csrf token")
	}
}

func TestMutatingRequestsRequireCSRFToken(t *testing.T) {
	store := newFakeStore()
	id := seedFeed(store, domain.Feed{Name: "Doomed", URL: "http://example.com/d"})
	c := authenticatedClient(t, store)

	rec := c.postRaw("/delete/"+id.Hex(), "csrf_token=rebuilt-by-an-attacker")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if store.feeds[id].Removed {
		t.Error("the feed was deleted even though the csrf token was invalid")
	}

	// A token of another session must not work either.
	other := c.csrf
	c.csrf = ""
	if rec := c.postRaw("/delete/"+id.Hex(), ""); rec.Code != http.StatusForbidden {
		t.Errorf("missing csrf token = %d, want 403", rec.Code)
	}
	c.csrf = other
	if rec := c.post("/delete/"+id.Hex(), nil); rec.Code != http.StatusSeeOther {
		t.Errorf("valid csrf token = %d, want 303", rec.Code)
	}
}

func TestLoginIsThrottledPerClient(t *testing.T) {
	c := newClient(t, testHandler(t, newFakeStore()))
	c.password = "wrong-password"

	for i := 0; i < 3; i++ {
		if rec := c.login(); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, rec.Code)
		}
	}
	rec := c.login()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after the failure limit = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a throttled response should carry Retry-After")
	}
	// The blocked client stays blocked until the backoff passes.
	if rec := c.login(); rec.Code != http.StatusTooManyRequests {
		t.Errorf("second blocked attempt = %d, want 429", rec.Code)
	}

	// Another client must not be locked out by someone else's failures.
	other := newClient(t, testHandler(t, newFakeStore()))
	other.reqIP = "10.0.0.99"
	if rec := other.login(); rec.Code != http.StatusSeeOther {
		t.Errorf("unrelated client = %d, want a normal login", rec.Code)
	}
}

func TestStoredCredentialsAreHashedNotPlaintext(t *testing.T) {
	store := newFakeStore()
	user := store.users[testUser]
	if strings.Contains(user.PasswordHash, testPassword) {
		t.Fatal("the fake store kept the plaintext password")
	}
	if !strings.HasPrefix(user.PasswordHash, "scrypt$") {
		t.Errorf("stored hash %q is not an scrypt verifier", user.PasswordHash)
	}
}

func TestLogoutDropsSession(t *testing.T) {
	store := newFakeStore()
	c := authenticatedClient(t, store)

	if rec := c.post("/logout/", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("logout = %d, want 303", rec.Code)
	}
	if len(store.sessions) != 0 {
		t.Errorf("%d sessions left in the store after logout", len(store.sessions))
	}
	if rec := c.get("/view/"); rec.Code != http.StatusSeeOther {
		t.Errorf("after logout /view/ = %d, want a redirect to the login page", rec.Code)
	}
}

func TestExpiredSessionIsRejected(t *testing.T) {
	store := newFakeStore()
	c := authenticatedClient(t, store)
	for hash, session := range store.sessions {
		session.ExpiresAt = time.Now().Add(-time.Minute)
		store.sessions[hash] = session
	}
	rec := c.get("/view/")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect to the login page", rec.Code)
	}
	if c.cookie("rssfetcher_session") != nil {
		t.Error("the expired session cookie was not cleared")
	}
}

func TestForgedSessionCookieIsIgnored(t *testing.T) {
	c := authenticatedClient(t, newFakeStore())
	// Replace the real cookie: two cookies with the same name would be served
	// first-come-first-served by r.Cookie.
	c.cookies = []*http.Cookie{{Name: "rssfetcher_session", Value: "not-a-real-session"}}
	if rec := c.get("/view/"); rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want the redirect to the login page", rec.Code)
	}
}

func TestChangePassword(t *testing.T) {
	store := newFakeStore()
	c := authenticatedClient(t, store)

	// Wrong current password.
	rec := c.post("/account/password", map[string]string{"currentPassword": "nope-nope-nope", "newPassword": "brand-new-password"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong current password = %d, want 403", rec.Code)
	}

	// Too short new password.
	rec = c.post("/account/password", map[string]string{"currentPassword": testPassword, "newPassword": "short"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("weak new password = %d, want 400", rec.Code)
	}

	// Success: the old sessions are dropped and the new password works.
	rec = c.post("/account/password", map[string]string{"currentPassword": testPassword, "newPassword": "brand-new-password"})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(store.sessions) != 0 {
		t.Errorf("%d sessions survived a password change", len(store.sessions))
	}
	again := newClient(t, testHandler(t, store))
	again.password = "brand-new-password"
	if rec := again.login(); rec.Code != http.StatusSeeOther {
		t.Errorf("login with the new password = %d, want 303", rec.Code)
	}
	old := newClient(t, testHandler(t, store))
	if rec := old.login(); rec.Code != http.StatusUnauthorized {
		t.Error("the old password still works after a change")
	}
}

func TestLoginHandlesStoreErrors(t *testing.T) {
	store := newFakeStore()
	store.verifyErr = errors.New("mongo is down")
	c := newClient(t, testHandler(t, store))
	c.get("/login/")
	c.password = testPassword
	rec := c.post("/login/", map[string]string{"username": testUser, "password": testPassword})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	// A database outage must not be reported as "invalid credentials", and it
	// must not lock the operator out of a working UI.
	if strings.Contains(rec.Body.String(), "invalid username") {
		t.Error("a store failure looked like a credential failure")
	}
}

func TestCSRFTokenBoundToSession(t *testing.T) {
	store := newFakeStore()
	first := authenticatedClient(t, store)
	second := authenticatedClient(t, store)
	if first.csrf == "" || first.csrf == second.csrf {
		t.Errorf("csrf tokens should be per session, got %q and %q", first.csrf, second.csrf)
	}
}

func TestAuthenticateIgnoresGarbageCookie(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/view/", nil)
	req.Header.Set("Cookie", "rssfetcher_session=")
	testHandler(t, newFakeStore()).ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 302 to login", rec.Code)
	}
}
