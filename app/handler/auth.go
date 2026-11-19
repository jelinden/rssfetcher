package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jelinden/rssfetcher/app/auth"
	"github.com/jelinden/rssfetcher/app/domain"
	"github.com/jelinden/rssfetcher/app/mongo"
)

// Auth options of the admin UI.
type Options struct {
	// SessionTTL is the login cookie lifetime; it slides while in use.
	SessionTTL time.Duration
	// CookieName defaults to "rssfetcher_session".
	CookieName string
	// CookieSecure should be true whenever the UI is served over TLS. It is
	// off by default so http://localhost works during development.
	CookieSecure bool
	// AuthSecret signs the CSRF tokens. Empty means "generate one at startup",
	// which invalidates outstanding forms on every restart.
	AuthSecret string
	// TrustProxy makes the login throttle use X-Forwarded-For. Only enable it
	// when a reverse proxy in front of this server sets that header, otherwise
	// clients could trivially spoof their address to dodge the throttle.
	TrustProxy bool
	// MaxFormBytes bounds request bodies.
	MaxFormBytes int64
	// LoginMaxFailures/LoginWindow control the per-client brute force throttle.
	LoginMaxFailures int
	LoginWindow      time.Duration
	// RefetchTimeout bounds a "check now" request. It talks to a third party on
	// the operator's behalf, so it cannot be allowed to hold the request open
	// for as long as that publisher decides to stall. Zero means
	// DefaultRefetchTimeout.
	RefetchTimeout time.Duration
	// HonourFeedTTL and MaxFeedTTL describe the polling policy, so the feed list
	// can say "next poll in 4m" instead of looking stuck. They mirror
	// -honour-feed-ttl and -max-feed-ttl.
	HonourFeedTTL bool
	MaxFeedTTL    time.Duration
}

// DefaultRefetchTimeout is the budget of one manual feed check: long enough for
// a slow publisher to answer, short enough that the admin page always comes
// back. A whole-cycle fetch is bounded separately by -fetch-timeout.
const DefaultRefetchTimeout = 30 * time.Second

func (o *Options) normalize() {
	if o.SessionTTL <= 0 {
		o.SessionTTL = 12 * time.Hour
	}
	if o.CookieName == "" {
		o.CookieName = "rssfetcher_session"
	}
	if o.MaxFormBytes <= 0 {
		o.MaxFormBytes = 1 << 20
	}
	if o.LoginMaxFailures <= 0 {
		o.LoginMaxFailures = 5
	}
	if o.LoginWindow <= 0 {
		o.LoginWindow = 5 * time.Minute
	}
	if o.RefetchTimeout <= 0 {
		o.RefetchTimeout = DefaultRefetchTimeout
	}
}

const csrfCookieName = "rssfetcher_csrf"

type ctxKey int

const sessionCtxKey ctxKey = iota

func sessionFrom(ctx context.Context) (domain.Session, bool) {
	s, ok := ctx.Value(sessionCtxKey).(domain.Session)
	return s, ok && !s.ID.IsZero()
}

// loginLimiter throttles repeated failed logins per client.
// loginLimiterMaxEntries bounds the throttle table. Without a bound, every
// address that ever fails a login is remembered forever, and with -trust-proxy
// the address comes straight from X-Forwarded-For, which makes the page a
// memory-leak primitive reachable without credentials.
const loginLimiterMaxEntries = 4096

type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]*loginAttempts
	max      int
	window   time.Duration
}

type loginAttempts struct {
	count   int
	first   time.Time
	blocked time.Time
}

func newLoginLimiter(max int, window time.Duration) *loginLimiter {
	return &loginLimiter{attempts: map[string]*loginAttempts{}, max: max, window: window}
}

// allowed reports whether the key may attempt a login, and how long to wait if
// it may not. Failed attempts inside the window are counted; once
// LoginMaxFailures is reached the client is blocked with an exponential backoff.
func (l *loginLimiter) allowed(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, ok := l.attempts[key]
	if !ok {
		return true, 0
	}
	now := time.Now()
	if now.Sub(entry.first) > l.window {
		// The window has passed: start over.
		delete(l.attempts, key)
		return true, 0
	}
	if wait := time.Until(entry.blocked); wait > 0 {
		return false, wait
	}
	return true, 0
}

func (l *loginLimiter) failure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.pruneLocked(time.Now())
	now := time.Now()
	entry, ok := l.attempts[key]
	if !ok || now.Sub(entry.first) > l.window {
		entry = &loginAttempts{first: now}
		l.attempts[key] = entry
	}
	entry.count++
	if entry.count >= l.max {
		// Exponential backoff, capped at five minutes.
		backoff := time.Duration(1<<min(entry.count-l.max, 6)) * time.Second
		if backoff > 5*time.Minute {
			backoff = 5 * time.Minute
		}
		entry.blocked = now.Add(backoff)
	}
}

// pruneLocked drops entries whose window (and any backoff) has passed. It is
// called from failure(), where the map grows, and is cheap because the table is
// bounded: at most one pass per login attempt.
func (l *loginLimiter) pruneLocked(now time.Time) {
	for key, entry := range l.attempts {
		// The zero `blocked` is "no backoff", and it is before everything, so
		// the check has to be on the active backoff, not on "not in the future".
		if now.Sub(entry.first) > l.window && !entry.blocked.After(now) {
			delete(l.attempts, key)
		}
	}
	// Everything was still fresh and the table is full: keep the newest half so
	// a burst from many addresses cannot grow it without limit.
	if len(l.attempts) > loginLimiterMaxEntries {
		byAge := make([]*loginAttempts, 0, len(l.attempts))
		keys := make([]string, 0, len(l.attempts))
		for key, entry := range l.attempts {
			byAge = append(byAge, entry)
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			return byAge[i].first.Before(byAge[j].first)
		})
		for _, key := range keys[:len(keys)/2] {
			delete(l.attempts, key)
		}
	}
}

func (l *loginLimiter) success(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}

// clientIP returns the address used for throttling.
func (h *Handler) clientIP(r *http.Request) string {
	if h.opts.TrustProxy {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			first := strings.TrimSpace(strings.Split(forwarded, ",")[0])
			if ip := net.ParseIP(first); ip != nil {
				return first
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// authenticate resolves the session cookie into a context value. It never
// rejects a request; requireAuth and the handlers decide what to do with an
// anonymous one.
func (h *Handler) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(h.opts.CookieName)
		if err != nil || cookie.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		session, extended, err := h.store.Session(r.Context(), cookie.Value, h.opts.SessionTTL)
		if err != nil {
			if !errors.Is(err, mongo.ErrSessionExpired) {
				log.Printf("http: resolving session: %v", err)
			}
			h.clearSessionCookie(w)
			next.ServeHTTP(w, r)
			return
		}
		// The cookie has its own expiry, so a session that was slid forward in
		// the database is only useful if the browser is told about it.
		if extended {
			h.setSessionCookie(w, cookie.Value, time.Until(session.ExpiresAt))
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionCtxKey, session)))
	})
}

func (h *Handler) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := sessionFrom(r.Context()); !ok {
			h.clearSessionCookie(w)
			http.Redirect(w, r, "/login/?"+url.Values{"next": {safeNext(r.URL.Path)}}.Encode(), http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// csrf protects every mutating request. Logged in clients present a token
// derived from their session id; the login form uses a double submitted cookie
// because no session exists yet.
func (h *Handler) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		// Parse explicitly: an oversized or malformed body is a client error,
		// not a CSRF failure, and reporting 403 here would be misleading.
		if err := r.ParseForm(); err != nil {
			http.Error(w, "request body is too large or malformed", http.StatusBadRequest)
			return
		}
		submitted := strings.TrimSpace(r.FormValue("csrf_token"))
		if submitted == "" {
			submitted = r.Header.Get("X-CSRF-Token")
		}
		if !h.validCSRF(r, submitted) {
			http.Error(w, "invalid or missing CSRF token, reload the page and try again", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) validCSRF(r *http.Request, submitted string) bool {
	if submitted == "" {
		return false
	}
	if session, ok := sessionFrom(r.Context()); ok {
		return auth.ValidCSRFToken(h.opts.AuthSecret, session.ID.Hex(), submitted)
	}
	nonce, err := r.Cookie(csrfCookieName)
	if err != nil || nonce.Value == "" {
		return false
	}
	return auth.ValidCSRFToken(h.opts.AuthSecret, nonce.Value, submitted)
}

// csrfToken returns the token for the current request, issuing the pre-auth
// cookie when needed.
func (h *Handler) csrfToken(w http.ResponseWriter, r *http.Request) string {
	if session, ok := sessionFrom(r.Context()); ok {
		token, err := auth.CSRFToken(h.opts.AuthSecret, session.ID.Hex())
		if err != nil {
			log.Printf("http: deriving csrf token: %v", err)
			return ""
		}
		return token
	}
	nonce := ""
	if cookie, err := r.Cookie(csrfCookieName); err == nil {
		nonce = cookie.Value
	}
	if nonce == "" {
		var err error
		if nonce, err = auth.NewToken(); err != nil {
			log.Printf("http: csrf nonce: %v", err)
			return ""
		}
		http.SetCookie(w, &http.Cookie{
			Name:     csrfCookieName,
			Value:    nonce,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   h.opts.CookieSecure,
			MaxAge:   int(h.opts.SessionTTL.Seconds()),
		})
	}
	token, err := auth.CSRFToken(h.opts.AuthSecret, nonce)
	if err != nil {
		log.Printf("http: deriving csrf token: %v", err)
		return ""
	}
	return token
}

func (h *Handler) LoginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := sessionFrom(r.Context()); ok {
		http.Redirect(w, r, "/view/", http.StatusSeeOther)
		return
	}
	page := domain.LoginPage{
		Page: domain.Page{
			Title:     "Log in",
			BodyClass: "login",
			Flash:     r.URL.Query().Get("flash"),
			CSRF:      h.csrfToken(w, r),
		},
		Next: safeNext(r.URL.Query().Get("next")),
	}
	if r.Method == http.MethodGet {
		h.render(w, "login", page)
		return
	}
	h.login(w, r, &page)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request, page *domain.LoginPage) {
	key := h.clientIP(r)
	if ok, wait := h.limiter.allowed(key); !ok {
		page.Err = "too many failed logins, try again later"
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))
		h.renderStatus(w, http.StatusTooManyRequests, "login", page)
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	if username == "" || password == "" {
		page.Err = "username and password are required"
		page.Username = username
		h.renderStatus(w, http.StatusBadRequest, "login", page)
		return
	}

	user, err := h.store.VerifyAdminPassword(r.Context(), username, password)
	switch {
	case errors.Is(err, mongo.ErrInvalidCredentials):
		// One generic message: never reveal whether the username exists.
		h.limiter.failure(key)
		log.Printf("http: failed login for %q from %s", username, key)
		page.Err = "invalid username or password"
		page.Username = username
		h.renderStatus(w, http.StatusUnauthorized, "login", page)
		return
	case err != nil:
		h.fail(w, r, "verifying credentials", err)
		return
	}

	token, session, err := h.store.CreateSession(r.Context(), user.ID, user.Username, h.opts.SessionTTL)
	if err != nil {
		h.fail(w, r, "creating session", err)
		return
	}
	h.limiter.success(key)
	h.setSessionCookie(w, token, h.opts.SessionTTL)
	// The pre-auth nonce is dead now that a session exists.
	http.SetCookie(w, &http.Cookie{Name: csrfCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	log.Printf("http: login %s from %s (session %s)", user.Username, key, session.ID.Hex())
	http.Redirect(w, r, page.Next, http.StatusSeeOther)
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(h.opts.CookieName); err == nil && cookie.Value != "" {
		if err := h.store.DeleteSession(r.Context(), cookie.Value); err != nil {
			log.Printf("http: deleting session: %v", err)
		}
	}
	h.clearSessionCookie(w)
	http.Redirect(w, r, "/login/", http.StatusSeeOther)
}

func (h *Handler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	session, ok := sessionFrom(ctx)
	if !ok {
		http.Redirect(w, r, "/login/", http.StatusSeeOther)
		return
	}
	current := r.FormValue("currentPassword")
	next := r.FormValue("newPassword")
	if err := h.store.ChangePassword(ctx, session.Username, current, next); err != nil {
		switch {
		case errors.Is(err, mongo.ErrInvalidCredentials):
			http.Error(w, "current password is not correct", http.StatusForbidden)
		case errors.Is(err, mongo.ErrWeakPassword):
			// The store error carries package prefixes; say what the user can
			// act on and log the detail.
			log.Printf("http: weak password for %q: %v", session.Username, err)
			http.Error(w, fmt.Sprintf("the new password was rejected: it must be at least %d characters", auth.MinPasswordLen), http.StatusBadRequest)
		default:
			h.fail(w, r, "changing password", err)
		}
		return
	}
	// All sessions were dropped, including this one.
	h.clearSessionCookie(w)
	http.SetCookie(w, &http.Cookie{Name: csrfCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	log.Printf("http: password changed for %s", session.Username)
	http.Redirect(w, r, "/login/?"+url.Values{"flash": {"password changed, log in again"}}.Encode(), http.StatusSeeOther)
}

// setSessionCookie issues (or renews) the login cookie. lifetime is the TTL the
// session was created or extended with, so the cookie tracks the document - except
// below the one minute floor below, where the database stays authoritative and the
// cookie simply outlives a session that is already gone.
func (h *Handler) setSessionCookie(w http.ResponseWriter, token string, lifetime time.Duration) {
	if lifetime < time.Minute {
		// A sub-minute cookie is worse than useless: it would be dropped while
		// the user is filling in a form.
		lifetime = time.Minute
	}
	http.SetCookie(w, &http.Cookie{
		Name:     h.opts.CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.opts.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(lifetime.Seconds()),
		Expires:  time.Now().Add(lifetime),
	})
}

func (h *Handler) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     h.opts.CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   h.opts.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
}

// safeNext keeps the redirect target inside this app. The dangerous forms are
// "//host" (protocol relative) and "/\host": url.Parse reports the latter as a
// relative path with an empty Host, but browsers normalise the backslash and
// follow it as //host - so both have to be rejected by looking at the raw
// string, not at the parsed URL.
func safeNext(path string) string {
	path = strings.TrimSpace(path)
	if len(path) < 2 || path[0] != '/' {
		return "/view/"
	}
	// A backslash anywhere is a smell this app has no use for; query strings may
	// legitimately contain "/" (e.g. /edit/<id>) so only the leading pair counts.
	if path[1] == '/' || path[1] == '\\' || strings.ContainsRune(path, '\\') {
		return "/view/"
	}
	if u, err := url.Parse(path); err != nil || u.IsAbs() || u.Host != "" || u.Scheme != "" {
		return "/view/"
	}
	return path
}
