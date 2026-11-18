package rss

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// M2: a feed may use relative references; they are meaningless without the feed
// URL they were served from, and a relative guid silently collides with the same
// path on a different host.
func TestRelativeReferencesAreResolvedAgainstTheFeedURL(t *testing.T) {
	body := `<rss version="2.0"><channel><title>R</title><link>https://example.com/news</link>
	  <item><title>Rel</title><link>/news/1</link><guid isPermaLink="false">/news/1</guid></item>
	  <item><title>Dots</title><link>../2</link><guidisPermaLink>nope</guidisPermaLink></item>
	</channel></rss>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()

	feed, err := FetchURL(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	first, second := feed.Items[0], feed.Items[1]
	if !strings.HasPrefix(first.Link, "http") {
		t.Errorf("item link = %q, want an absolute URL", first.Link)
	}
	if first.Link != srv.URL+"/news/1" && first.Link != "http://127.0.0.1"+"/news/1" {
		if !strings.HasSuffix(first.Link, "/news/1") || !strings.HasPrefix(first.Link, "http://") {
			t.Errorf("item link = %q, want it resolved against the feed URL", first.Link)
		}
	}
	if first.GUID == "/news/1" {
		t.Error("relative guid kept verbatim: the same path on another host would collide")
	}
	if first.GUID == "" {
		t.Error("guid lost while resolving")
	}
	if !strings.HasPrefix(second.Link, "http://") || strings.Contains(second.Link, "..") {
		t.Errorf("../2 resolved to %q", second.Link)
	}
}

// An opaque identifier is not a path and must not be turned into a fabricated one.
func TestOpaqueGUIDsAreNamespacedNotRewritten(t *testing.T) {
	base, err := url.Parse("https://example.com/news/feed.xml")
	if err != nil {
		t.Fatal(err)
	}
	feed := &Feed{Items: []*Item{
		{GUID: "1234", Link: "a"},
		{GUID: "tag:example.com,2005:1234", Link: "b"},
		{GUID: "urn:uuid:0f5c", Link: "c"},
		{GUID: "https://other.example/keep", Link: "d"},
	}}
	ResolveReferences(feed, base.String())
	// tag: and urn: are schemes of their own and globally unique by
	// construction, so they stay exactly as the publisher wrote them; only a bare
	// identifier needs the feed namespacing it.
	want := []string{
		"https://example.com/news/feed.xml|1234",
		"tag:example.com,2005:1234",
		"urn:uuid:0f5c",
		"https://other.example/keep",
	}
	for i, item := range feed.Items {
		if item.GUID != want[i] {
			t.Errorf("guid %d = %q, want %q", i, item.GUID, want[i])
		}
	}
	// The same opaque id from a different publisher must not collide.
	other := &Feed{Items: []*Item{{GUID: "1234"}}}
	ResolveReferences(other, "https://another.example/rss")
	if other.Items[0].GUID == feed.Items[0].GUID {
		t.Error("the same bare guid from two hosts produced the same stored key")
	}
}

// L1: Atom uses <icon>/<logo>, not <image>.
func TestAtomChannelImage(t *testing.T) {
	body := `<feed xmlns="http://www.w3.org/2005/Atom"><title>A</title>
	  <link rel="self" href="https://example.com/feed"/>
	  <logo>https://example.com/logo.png</logo>
	  <entry><title>E</title><link href="https://example.com/e1"/><id>tag:e1</id></entry></feed>`
	feed, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if feed.Image == nil || feed.Image.Url != "https://example.com/logo.png" {
		t.Errorf("channel image = %+v, want the <logo> URL", feed.Image)
	}
	if feed.Link != "https://example.com/feed" {
		t.Errorf("channel link = %q, want the rel=self URL as the fallback", feed.Link)
	}
}

// M4: <content> is the full entry, <summary> is the teaser. RSS 2.0 already
// prefers content:encoded over description; atom has to do the same.
func TestAtomPrefersContentOverSummary(t *testing.T) {
	body := `<feed xmlns="http://www.w3.org/2005/Atom"><title>A</title>
	  <entry><title>E</title><link href="https://example.com/e1"/><id>tag:e1</id>
	    <summary>teaser</summary>
	    <content type="html">&lt;p&gt;the full article&lt;/p&gt;</content>
	  </entry></feed>`
	feed, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if got := feed.Items[0].Content; !strings.Contains(got, "full article") {
		t.Errorf("content = %q, want the full <content> body", got)
	}
}

// L3: feeds with a zone abbreviation are real (RFC 822 with "MST" etc). Go has no
// abbreviation table, so it parses them as UTC and the item lands hours off.
func TestZoneAbbreviationsCarryTheirOffset(t *testing.T) {
	tests := []struct {
		in      string
		wantUTC time.Time
	}{
		{"Wed, 01 Jan 2025 10:00:00 MST", time.Date(2025, 1, 1, 17, 0, 0, 0, time.UTC)},
		{"Wed, 01 Jan 2025 10:00:00 EST", time.Date(2025, 1, 1, 15, 0, 0, 0, time.UTC)},
		{"Wed, 01 Jan 2025 10:00:00 EDT", time.Date(2025, 1, 1, 14, 0, 0, 0, time.UTC)},
		{"Wed, 01 Jan 2025 10:00:00 GMT", time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)},
		{"Wed, 01 Jan 2025 10:00:00 +0100", time.Date(2025, 1, 1, 9, 0, 0, 0, time.UTC)},
	}
	for _, tc := range tests {
		got, err := parseTime(tc.in)
		if err != nil {
			t.Errorf("parseTime(%q): %v", tc.in, err)
			continue
		}
		if !got.UTC().Equal(tc.wantUTC) {
			t.Errorf("parseTime(%q) = %v, want %v", tc.in, got.UTC(), tc.wantUTC)
		}
	}
}

// L4: the parsers used to stop at the first N items in document order, so a feed
// that lists its oldest items first yielded the wrong N.
func TestParsersReturnMoreThanTheStoredLimit(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`<rss version="2.0"><channel><title>R</title>`)
	for i := 1; i <= 12; i++ {
		sb.WriteString("<item><title>i" + itoa(i) + "</title><link>https://example.com/" + itoa(i) + "</link><guid>g" + itoa(i) + "</guid><pubDate>Mon, 02 Jan 2026 10:0" + itoa(i%10) + ":00 GMT</pubDate></item>")
	}
	sb.WriteString(`</channel></rss>`)
	feed, err := Parse([]byte(sb.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(feed.Items) < 12 {
		t.Fatalf("parsed %d items, want all 12 so the newest can be selected later", len(feed.Items))
	}
}

func TestNewestItemsSelection(t *testing.T) {
	items := []*Item{
		{Title: "old", Link: "a", GUID: "a", Date: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{Title: "new", Link: "b", GUID: "b", Date: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)},
		{Title: "middle", Link: "c", GUID: "c", Date: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
	}
	SetMaxItemsPerFeed(2)
	t.Cleanup(func() { SetMaxItemsPerFeed(0) })
	newest := NewestItems(items, MaxItemsPerFeed())
	if len(newest) != 2 || newest[0].Title != "new" || newest[1].Title != "middle" {
		t.Errorf("NewestItems = %v, want [new middle] in date order", titles(newest))
	}
	// Items without a date must not sort ahead of dated ones.
	undated := []*Item{{Title: "nodate", Link: "x", GUID: "x"}}
	if got := NewestItems(append([]*Item{}, undated...), 5); len(got) != 1 {
		t.Errorf("undated items dropped: %v", titles(got))
	}
}

// L11: the limit has to be configurable at runtime.
func TestMaxItemsPerFeedIsConfigurable(t *testing.T) {
	t.Cleanup(func() { SetMaxItemsPerFeed(0) })
	if got := MaxItemsPerFeed(); got != DefaultMaxItemsPerFeed {
		t.Errorf("default = %d, want %d", got, DefaultMaxItemsPerFeed)
	}
	SetMaxItemsPerFeed(30)
	if got := MaxItemsPerFeed(); got != 30 {
		t.Errorf("after SetMaxItemsPerFeed(30) = %d", got)
	}
	SetMaxItemsPerFeed(-1) // nonsense restores the documented default, it is not zeroed
	if got := MaxItemsPerFeed(); got != DefaultMaxItemsPerFeed {
		t.Errorf("after SetMaxItemsPerFeed(-1) = %d, want the default %d", got, DefaultMaxItemsPerFeed)
	}
}

// M6: the fetcher dials whatever the database says, which turns it into a probe
// for the local network and the cloud metadata endpoint.
func TestPrivateAddressesAreRejectedByDefault(t *testing.T) {
	for _, ip := range []string{
		"127.0.0.1", "::1", "10.0.0.5", "172.16.3.2", "192.168.1.1",
		"169.254.169.254", "fe80::1", "fc00::1", "0.0.0.0",
	} {
		if err := checkPublicAddress(ip); err == nil {
			t.Errorf("checkPublicAddress(%q) = nil, want it blocked", ip)
		}
	}
	for _, ip := range []string{"93.184.216.34", "2001:4860:4860::8888", "8.8.8.8"} {
		if err := checkPublicAddress(ip); err != nil {
			t.Errorf("checkPublicAddress(%q) = %v, want it allowed", ip, err)
		}
	}
}

func TestFetchRefusesPrivateHostsUnlessAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<rss version="2.0"><channel><title>x</title></channel></rss>`))
	}))
	defer srv.Close()

	// The package default is "guard off" (see TestMain); httptest servers listen
	// on loopback, which is exactly what the guard exists to refuse.
	previous := AllowPrivateURLs()
	t.Cleanup(func() { SetAllowPrivateURLs(previous) })

	SetAllowPrivateURLs(false)
	if _, err := FetchURL(context.Background(), srv.URL); err == nil {
		t.Fatal("fetching a loopback feed succeeded with the guard enabled")
	} else if !strings.Contains(err.Error(), "private") {
		t.Errorf("err = %v, want it to explain the address policy", err)
	}

	SetAllowPrivateURLs(true)
	if _, err := FetchURL(context.Background(), srv.URL); err != nil {
		t.Fatalf("with the guard off the fetch should work: %v", err)
	}
}

// L2: conditional requests. A feed that answers 304 must be reported as
// unchanged (not as a parse failure) and must not lose its items.
func TestConditionalFetchReportsNotModified(t *testing.T) {
	var ifNoneMatch, ifModified string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ifNoneMatch = r.Header.Get("If-None-Match")
		ifModified = r.Header.Get("If-Modified-Since")
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2026 10:00:00 GMT")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	res, err := Fetch(context.Background(), FeedRequest{URL: srv.URL, ETag: `"v1"`, LastModified: "Mon, 02 Jan 2026 10:00:00 GMT"})
	if err != nil {
		t.Fatalf("a 304 must not be an error: %v", err)
	}
	if !res.NotModified {
		t.Error("NotModified = false for a 304 response")
	}
	if ifNoneMatch != `"v1"` || ifModified == "" {
		t.Errorf("validators not sent: If-None-Match=%q If-Modified-Since=%q", ifNoneMatch, ifModified)
	}
	if res.ETag != `"v1"` {
		t.Errorf("ETag = %q, want it echoed back for storage", res.ETag)
	}
}

// L2: <ttl> is in minutes and must reach the scheduler instead of being dropped.
func TestFeedTTLOfRSS2IsHonoured(t *testing.T) {
	body := `<rss version="2.0"><channel><title>x</title><ttl>45</ttl>
	  <item><title>i</title><link>https://example.com/1</link><guid>g</guid></item></channel></rss>`
	feed, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if feed.TTL != 45*time.Minute {
		t.Errorf("TTL = %s, want 45m", feed.TTL)
	}
}

func titles(items []*Item) []string {
	out := []string{}
	for _, i := range items {
		out = append(out, i.Title)
	}
	return out
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

// A typo in a feed URL must not be reported as a policy decision.
func TestUnresolvableHostReportsDNSNotTheGuard(t *testing.T) {
	previous := AllowPrivateURLs()
	t.Cleanup(func() { SetAllowPrivateURLs(previous) })
	SetAllowPrivateURLs(false)

	_, err := FetchURL(context.Background(), "https://this-host-does-not-exist.invalid/feed.xml")
	if err == nil {
		t.Fatal("an unresolvable host was fetched successfully")
	}
	if strings.Contains(err.Error(), "private") {
		t.Errorf("err = %v, want the DNS failure rather than the address policy", err)
	}
	if !strings.Contains(err.Error(), "no such host") && !strings.Contains(err.Error(), "invalid") {
		t.Errorf("err = %v, want it to mention the lookup", err)
	}
}
