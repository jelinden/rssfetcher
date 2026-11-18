package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jelinden/rssfetcher/app/rss"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// M5: this package holds the validation boundary that decides what the fetcher is
// allowed to download, and it had no tests at all.

func TestValidateHTTPURLRejectsEverythingThatIsNotAWebURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string // "" means accepted
	}{
		{"https", "https://example.com/feed.xml", ""},
		{"http", "http://example.com/feed.xml", ""},
		{"uppercase scheme", "HTTP://example.com/feed", ""}, // url.Parse lower-cases it
		{"with port", "https://example.com:8443/feed", ""},
		{"query string", "https://example.com/f?x=1&y=2", ""},
		{"ipv6 host", "http://[::1]:8080/feed", ""},
		{"empty", "", "url is required"},
		{"whitespace", "   ", "url is required"},
		{"file scheme", "file:///etc/passwd", `scheme "file"`},
		{"gopher", "gopher://127.0.0.1:70/rss", `scheme "gopher"`},
		{"ssh", "ssh://example.com/feed", `scheme "ssh"`},
		{"no scheme", "example.com/feed", `scheme ""`},
		{"relative", "/feeds/example.xml", `scheme ""`},
		{"missing host", "http://", "missing a host"},
		{"missing host with slash", "https:///", "missing a host"},
		{"control characters", "https://exa\nmple.com/feed", "invalid control character"},
		{"percent escape error", "https://example.com/%zz", "%zz"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHTTPURL(tc.raw, "url")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateHTTPURL(%q) = %v, want it accepted", tc.raw, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateHTTPURL(%q) = nil, want an error mentioning %q", tc.raw, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("validateHTTPURL(%q) = %v, want it to mention %q", tc.raw, err, tc.wantErr)
			}
		})
	}
}

// A URL whose authority is only "://" inside the path must still be rejected: the
// naive contains("://") check the first version used accepted "not-a-url://x".
func TestValidateDoesNotTreatSchemeLikeTextAsAURL(t *testing.T) {
	if err := (FeedInput{Name: "n", URL: "notaurl://example.com"}).Validate(); err == nil {
		t.Error("a URL with an unknown scheme was accepted")
	}
	if err := (FeedInput{Name: "n", URL: "https://example.com/a://b"}).Validate(); err != nil {
		t.Errorf("a legitimate path containing :// was rejected: %v", err)
	}
}

func TestValidateRequiresAName(t *testing.T) {
	in := FeedInput{URL: "https://example.com/feed"}
	if err := in.Validate(); err == nil || !strings.Contains(err.Error(), "name") {
		t.Errorf("Validate without a name = %v, want the missing name reported", err)
	}
	in.Name = "  "
	if err := in.Validate(); err == nil {
		t.Error("a whitespace only name was accepted")
	}
}

// siteUrl is optional, but when it is given it must be a web URL: the UI renders
// it as a link.
func TestValidateSiteURLOptionalButChecked(t *testing.T) {
	if err := (FeedInput{Name: "n", URL: "https://example.com/f", SiteURL: ""}).Validate(); err != nil {
		t.Errorf("an empty siteUrl must be optional: %v", err)
	}
	err := (FeedInput{Name: "n", URL: "https://example.com/f", SiteURL: "javascript:alert(1)"}).Validate()
	if err == nil {
		t.Fatal("a javascript: siteUrl was accepted")
	}
	if !strings.Contains(err.Error(), "siteUrl") {
		t.Errorf("err = %v, want it to name the field", err)
	}
}

func TestNormalizeTrimsAndDefaultsTheScheme(t *testing.T) {
	in := FeedInput{Name: "  A feed  ", URL: "  https://example.com/f  ", SiteURL: " example.com ", Language: " en "}
	in.Normalize()
	if in.Name != "A feed" || in.URL != "https://example.com/f" || in.Language != "en" {
		t.Errorf("Normalize = %+v", in)
	}
	if in.SiteURL != "https://example.com" {
		t.Errorf("siteUrl = %q, want the https default", in.SiteURL)
	}

	// An explicit scheme is never touched, and "://" inside a path is not a
	// scheme.
	in = FeedInput{SiteURL: "http://example.com"}
	in.Normalize()
	if in.SiteURL != "http://example.com" {
		t.Errorf("siteUrl = %q, want it kept", in.SiteURL)
	}
	in = FeedInput{SiteURL: "example.com/a://b"}
	in.Normalize()
	if in.SiteURL != "https://example.com/a://b" {
		t.Errorf("siteUrl = %q, want exactly one prefix", in.SiteURL)
	}
	in = FeedInput{SiteURL: ""}
	in.Normalize()
	if in.SiteURL != "" {
		t.Errorf("an empty siteUrl became %q, want it left empty", in.SiteURL)
	}
}

func TestEffectiveSubCategoryOnlyReturnsARealOne(t *testing.T) {
	id := bson.NewObjectID()
	tests := []struct {
		name string
		feed Feed
		want *rss.SubCategory
	}{
		{"nil", Feed{}, nil},
		{"zero id", Feed{SubCategory: &rss.SubCategory{SubCategory: "AI"}}, nil},
		{"set", Feed{SubCategory: &rss.SubCategory{ID: id, SubCategory: "AI"}}, &rss.SubCategory{ID: id, SubCategory: "AI"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The whole point: a stored feed from before sub categories existed
			// must not panic here.
			got := tc.feed.EffectiveSubCategory()
			if tc.want == nil {
				if got != nil {
					t.Errorf("EffectiveSubCategory = %+v, want nil", got)
				}
				return
			}
			if got == nil || got.SubCategory != tc.want.SubCategory {
				t.Errorf("EffectiveSubCategory = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestIDOrNew(t *testing.T) {
	in := FeedInput{}
	id := in.IDOrNew()
	if id.IsZero() {
		t.Error("IDOrNew returned a zero id")
	}
	if in.IDOrNew() == id {
		t.Error("IDOrNew returned the same id twice; every new feed needs its own")
	}
	existing := bson.NewObjectID()
	in.ID = existing
	if got := in.IDOrNew(); got != existing {
		t.Errorf("IDOrNew = %v, want the existing id kept", got)
	}
}

func TestSessionExpiry(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		session Session
		want    bool
	}{
		{"no expiry", Session{}, false},
		{"in the future", Session{ExpiresAt: now.Add(time.Hour)}, false},
		{"exactly now", Session{ExpiresAt: now}, true},
		{"in the past", Session{ExpiresAt: now.Add(-time.Second)}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.session.Expired(now); got != tc.want {
				t.Errorf("Expired = %v, want %v", got, tc.want)
			}
		})
	}
}

// The password hash must never leave the process through a JSON encoder: the
// admin UI serialises page data.
func TestAdminUserNeverSerialisesTheHash(t *testing.T) {
	user := AdminUser{Username: "admin", PasswordHash: "scrypt$1$2$3$secret"}
	encoded, err := jsonMarshal(user)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, "secret") || strings.Contains(encoded, "passwordHash") {
		t.Errorf("serialised admin user contains the hash: %s", encoded)
	}
	if !strings.Contains(encoded, "admin") {
		t.Errorf("serialised admin user lost the username: %s", encoded)
	}
}

func jsonMarshal(v any) (string, error) {
	data, err := json.Marshal(v)
	return string(data), err
}
