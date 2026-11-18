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
	Page
	Feeds         []Feed            `json:"feeds"`
	Categories    []rss.Category    `json:"categories"`
	SubCategories []rss.SubCategory `json:"subCategories"`
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
