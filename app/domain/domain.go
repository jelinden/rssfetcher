package domain

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jelinden/rssfetcher/app/rss"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type Feed struct {
	ID          bson.ObjectID    `json:"id" bson:"_id"`
	Name        string           `json:"feedTitle" bson:"feedTitle,omitempty"`
	URL         string           `json:"url" bson:"url,omitempty"`
	SiteURL     string           `json:"siteUrl" bson:"siteUrl,omitempty"`
	Category    rss.Category     `json:"category" bson:"category,omitempty"`
	SubCategory *rss.SubCategory `json:"subCategory" bson:"subCategory,omitempty"`
	Language    string           `json:"language" bson:"language,omitempty"`
	Removed     bool             `json:"removed" bson:"removed,omitempty"`

	// ETag and LastModified are the validators of the last successful fetch.
	// Sending them back as If-None-Match / If-Modified-Since turns a daily poll
	// of forty feeds into forty "nothing changed" answers, which is both the
	// polite and the cheap thing to do.
	ETag         string    `json:"etag" bson:"etag,omitempty"`
	LastModified string    `json:"lastModified" bson:"lastModified,omitempty"`
	FetchedAt    time.Time `json:"fetchedAt" bson:"fetchedAt,omitempty"`

	// TTL is what the feed itself asked for (<ttl> in RSS 2.0). The fetcher
	// skips a feed until this much time has passed since FetchedAt, so a pollite
	// global interval can still leave a slow moving publisher alone.
	TTL time.Duration `json:"ttl" bson:"ttl,omitempty"`

	// Result of the last fetch attempt, kept on the feed so the admin UI can
	// point at what is broken. Before this, a subscription could 404 for months
	// and look exactly like a working one on the page; the only trace was in the
	// server log. LastError is empty for a healthy feed.
	LastError   string    `json:"lastError" bson:"lastError,omitempty"`
	LastErrorAt time.Time `json:"lastErrorAt" bson:"lastErrorAt,omitempty"`
	// OKAt is the last attempt that worked, and Failures counts the consecutive
	// ones since then - enough to tell "newly broken" from "broken for a week".
	OKAt     time.Time `json:"okAt" bson:"okAt,omitempty"`
	Failures int       `json:"failures" bson:"failures,omitempty"`
	// LastItems is how many items the last fetch that got a body wrote. Re-fetching
	// a feed whose items are all known writes nothing, so this is a count of new
	// articles, not of the articles stored for the feed.
	LastItems int `json:"lastItems" bson:"lastItems,omitempty"`
}

// Failing reports whether the last attempt at this feed ended in an error.
func (f Feed) Failing() bool {
	return strings.TrimSpace(f.LastError) != ""
}

// NeverChecked reports a feed that has been added but not fetched yet, which the
// UI shows as "never checked" instead of pretending it is healthy.
func (f Feed) NeverChecked() bool {
	return f.OKAt.IsZero() && f.LastErrorAt.IsZero()
}

// EffectiveTTL is how long the fetcher waits after a good fetch of this feed: the
// publisher's own <ttl>, capped at maxTTL when that is set. A cap matters because
// plenty of feeds publish a ttl of hours or days, which a news reader cannot accept
// - and a publisher asking for a week gets the same courtesy whether or not the
// operator is watching.
func (f Feed) EffectiveTTL(maxTTL time.Duration) time.Duration {
	ttl := f.TTL
	if maxTTL > 0 && (ttl <= 0 || ttl > maxTTL) {
		return maxTTL
	}
	return ttl
}

// DueIn is how long until the feed should be fetched again, or zero when it is due
// now. It is the number the admin UI shows next to the fetch time, so a row that
// has not moved in an hour can be explained without reading the source.
func (f Feed) DueIn(now time.Time, maxTTL time.Duration) time.Duration {
	if f.FetchedAt.IsZero() {
		return 0
	}
	ttl := f.EffectiveTTL(maxTTL)
	if ttl <= 0 {
		return 0
	}
	// A FetchedAt in the future (an NTP correction after the process read the
	// clock) must not postpone the next poll by more than one ttl.
	at := f.FetchedAt
	if at.After(now) {
		at = now
	}
	d := at.Add(ttl).Sub(now)
	if d <= 0 {
		return 0
	}
	return d
}

// StatusAge is how long ago the current status was recorded, taking the more
// recent of the two timestamps. It is zero when nothing has been attempted, so
// the caller never renders "broken since 500000 hours ago".
func (f Feed) StatusAge(now time.Time) time.Duration {
	at := f.OKAt
	if f.LastErrorAt.After(at) {
		at = f.LastErrorAt
	}
	if at.IsZero() {
		return 0
	}
	d := now.Sub(at)
	if d < 0 {
		return 0
	}
	return d
}

// EffectiveSubCategory returns the sub category or nil when it is unset. Feeds
// stored before sub categories existed have no subCategory field at all, so the
// pointer can legitimately be nil — dereferencing it used to panic.
func (f Feed) EffectiveSubCategory() *rss.SubCategory {
	if f.SubCategory == nil || f.SubCategory.ID.IsZero() {
		return nil
	}
	return f.SubCategory
}

// FeedInput is the (user supplied) payload of the edit form.
type FeedInput struct {
	ID          bson.ObjectID
	Name        string
	URL         string
	SiteURL     string
	Language    string
	Category    rss.Category
	SubCategory rss.SubCategory
}

// Normalize trims the form values and defaults the scheme of siteUrl, so that
// "example.com" becomes "https://example.com" instead of being stored as a
// broken link.
func (in *FeedInput) Normalize() {
	in.Name = strings.TrimSpace(in.Name)
	in.URL = strings.TrimSpace(in.URL)
	in.SiteURL = strings.TrimSpace(in.SiteURL)
	in.Language = strings.TrimSpace(in.Language)
	if in.SiteURL != "" && !hasScheme(in.SiteURL) {
		in.SiteURL = "https://" + in.SiteURL
	}
}

// hasScheme reports whether a URL already carries a scheme of its own. The
// previous check was strings.Contains(raw, "://"), which accepted
// "example.com/a://b" as "already absolute" and stored it as a relative link the
// UI then rendered against this server.
func hasScheme(raw string) bool {
	i := strings.Index(raw, "://")
	if i <= 0 {
		return false
	}
	for n, r := range raw[:i] {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case n > 0 && (r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	return true
}

func (in FeedInput) IDOrNew() bson.ObjectID {
	if in.ID.IsZero() {
		return bson.NewObjectID()
	}
	return in.ID
}

// Validate rejects empty and non http(s) URLs. The fetcher downloads whatever
// is stored here, so a feed URL that points at an internal service or a file
// would turn the fetcher into an SSRF primitive.
func (in FeedInput) Validate() error {
	if strings.TrimSpace(in.Name) == "" {
		return errors.New("feed name is required")
	}
	if err := validateHTTPURL(in.URL, "url"); err != nil {
		return err
	}
	if strings.TrimSpace(in.SiteURL) != "" {
		if err := validateHTTPURL(in.SiteURL, "siteUrl"); err != nil {
			return err
		}
	}
	return nil
}

func validateHTTPURL(raw, field string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("%s is required", field)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL: %w", field, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s must be an http or https URL, got scheme %q", field, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%s is missing a host", field)
	}
	return nil
}

// Page holds the fields every template needs: the shared header renders the
// signed-in user and the CSRF token, so no page may be rendered without it.
type Page struct {
	Title     string `json:"title"`
	BodyClass string `json:"bodyClass"`
	Flash     string `json:"flash"`
	Err       string `json:"err"`
	CSRF      string `json:"csrf"`
	Username  string `json:"username"`
}

type ViewPage struct {
	// Now and the polling policy, so a feed that has not moved can explain that
	// the publisher asked for it (a row fetched an hour ago looks broken otherwise).
	Now       time.Time     `json:"now"`
	HonourTTL bool          `json:"honourTtl"`
	MaxTTL    time.Duration `json:"maxTtl"`
	Page
	Feeds         []Feed            `json:"feeds"`
	Categories    []rss.Category    `json:"categories"`
	SubCategories []rss.SubCategory `json:"subCategories"`

	// Broken is how many active subscriptions failed their last fetch, and
	// StatusFilter is the query narrows the list to them ("broken") or empty.
	// In a list of seventy feeds, the summary line is what makes a dead
	// publisher impossible to miss.
	Broken       int    `json:"broken"`
	StatusFilter string `json:"statusFilter"`
	// Total is the number of feeds before the filter, so the summary can say
	// "1 broken of 71" instead of leaving the operator to count.
	Total int `json:"total"`
}

type EditPage struct {
	Page
	Feed          Feed              `json:"feed"`
	Categories    []rss.Category    `json:"categories"`
	SubCategories []rss.SubCategory `json:"subCategories"`
}

// LoginPage is the login form. Next is a validated, path-only redirect target.
type LoginPage struct {
	Page
	Next string `json:"next"`
}

// AdminUser is an account allowed into the admin UI. Only the scrypt verifier
// of the password is stored, never the password itself.
type AdminUser struct {
	ID           bson.ObjectID `json:"id" bson:"_id"`
	Username     string        `json:"username" bson:"username"`
	PasswordHash string        `json:"-" bson:"passwordHash"`
	CreatedAt    time.Time     `json:"createdAt" bson:"createdAt"`
	UpdatedAt    time.Time     `json:"updatedAt" bson:"updatedAt"`
}

// Session is a server side login session. TokenHash is the SHA-256 of the
// cookie value, so a database leak cannot be replayed as a cookie. The CSRF
// token is derived from the session id (see app/auth), so it is never stored.
type Session struct {
	ID        bson.ObjectID `json:"id" bson:"_id"`
	TokenHash string        `json:"-" bson:"tokenHash"`
	UserID    bson.ObjectID `json:"userId" bson:"userId"`
	Username  string        `json:"username" bson:"username"`
	CreatedAt time.Time     `json:"createdAt" bson:"createdAt"`
	ExpiresAt time.Time     `json:"expiresAt" bson:"expiresAt"`
}

// Expired reports whether the session is past its expiry (the TTL index only
// cleans up lazily).
func (s Session) Expired(now time.Time) bool {
	return !s.ExpiresAt.IsZero() && !now.Before(s.ExpiresAt)
}
