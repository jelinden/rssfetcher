package rss

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fetch is a single indirection point so the call sites below do not have to
// change when Fetch's signature changes.
var fetch = func(url string) (*Feed, error) { return FetchURL(context.Background(), url) }

func rss2Fixture(items int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Test</title><link>http://example.com</link><description>d</description>`)
	for i := 0; i < items; i++ {
		fmt.Fprintf(&b, `<item><title>item %d</title><link>http://example.com/%d</link><guid>guid-%d</guid><pubDate>Mon, 02 Jan 2006 15:04:05 MST</pubDate><description>desc %d</description></item>`, i, i, i, i)
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}

func TestParseRSS2Basic(t *testing.T) {
	feed, err := Parse([]byte(rss2Fixture(2)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if feed.Title != "Test" {
		t.Errorf("Title = %q, want %q", feed.Title, "Test")
	}
	if len(feed.Items) != 2 {
		t.Fatalf("len(Items) = %d, want 2", len(feed.Items))
	}
	if feed.Items[0].GUID != "guid-0" {
		t.Errorf("GUID = %q, want guid-0", feed.Items[0].GUID)
	}
}

// The parsers collect generously (up to itemFetchLimit) because they see the
// items in document order, and the store then selects the newest
// MaxItemsPerFeed by date. The old code disagreed with itself here: atom
// returned 6, rss1/rss2 returned everything, and mongo stored the first 5.
func TestParseLimitsItemsPerFeed(t *testing.T) {
	check := func(name string, feed *Feed, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(feed.Items) > itemFetchLimit {
			t.Errorf("%s: len(Items) = %d, want <= %d", name, len(feed.Items), itemFetchLimit)
		}
		stored := NewestItems(feed.Items, MaxItemsPerFeed())
		if len(stored) > MaxItemsPerFeed() {
			t.Errorf("%s: the store limit is not enforced: %d > %d", name, len(stored), MaxItemsPerFeed())
		}
	}
	feed2, err := Parse([]byte(rss2Fixture(itemFetchLimit + 5)))
	check("rss2", feed2, err)

	feed1, err := Parse([]byte(rss1Fixture(itemFetchLimit + 5)))
	check("rss1", feed1, err)

	feedAtom, err := Parse([]byte(atomFixture(itemFetchLimit + 5)))
	check("atom", feedAtom, err)
}

func rss1Fixture(items int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/">
<channel rdf:about="http://example.com"><title>Test</title><link>http://example.com</link></channel>`)
	for i := 0; i < items; i++ {
		fmt.Fprintf(&b, `<item rdf:about="http://example.com/%d"><title>item %d</title><link>http://example.com/%d</link><description>desc %d</description></item>`, i, i, i, i)
	}
	b.WriteString(`</rdf:RDF>`)
	return b.String()
}

func atomFixture(items int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom"><title>Test</title><link rel="alternate" href="http://example.com"/>`)
	for i := 0; i < items; i++ {
		fmt.Fprintf(&b, `<entry><title>item %d</title><link href="http://example.com/%d"/><id>atom-%d</id><updated>2006-01-02T15:04:05Z</updated></entry>`, i, i, i)
	}
	b.WriteString(`</feed>`)
	return b.String()
}

func TestParseAtomUsesAlternateLink(t *testing.T) {
	body := `<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
 <title>t</title>
 <link rel="self" href="http://example.com/feed.atom"/>
 <link rel="alternate" href="http://example.com/"/>
 <updated>2006-01-02T15:04:05Z</updated>
 <entry>
  <title>e</title><id>1</id>
  <link rel="alternate" href="http://example.com/post"/>
  <link rel="edit" href="http://example.com/edit/1"/>
  <published>2006-01-02T15:04:05Z</published>
  <summary>s</summary>
 </entry>
</feed>`

	feed, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if feed.Link != "http://example.com/" {
		t.Errorf("feed.Link = %q, want http://example.com/", feed.Link)
	}
	if feed.Items[0].Link != "http://example.com/post" {
		t.Errorf("item.Link = %q, want http://example.com/post", feed.Items[0].Link)
	}
}

// content:encoded is the de-facto full text of RSS 2.0 feeds.
func TestParseRSS2PrefersContentEncoded(t *testing.T) {
	body := `<?xml version="1.0"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/">
<channel><title>t</title><link>l</link><description>d</description>
<item><title>i</title><link>http://example.com/1</link><guid>g1</guid>
<description></description>
<content:encoded><![CDATA[<p>full body</p>]]></content:encoded>
</item>
</channel></rss>`

	feed, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(feed.Items[0].Content, "full body") {
		t.Errorf("item.Content = %q, want it to contain the content:encoded text", feed.Items[0].Content)
	}
}

func TestFetchRejectsUnsuccessfulStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, rss2Fixture(1))
	}))
	defer srv.Close()

	if _, err := fetch(srv.URL); err == nil {
		t.Fatal("Fetch(500) = nil error, want an error")
	}
}

func TestFetchRejectsOversizedBody(t *testing.T) {
	huge := rss2Fixture(1)
	huge = strings.Replace(huge, "<title>Test</title>", "<title>"+strings.Repeat("x", 6<<20)+"</title>", 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The client is expected to abort the read once it hits the limit, so
		// this write may legitimately fail.
		_, _ = io.WriteString(w, huge)
	}))
	defer srv.Close()

	if _, err := fetch(srv.URL); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("Fetch of a %d byte body err = %v, want ErrBodyTooLarge", len(huge), err)
	}
}

func TestFetchRejectsNonXMLBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<html><body>nothing here</body></html>")
	}))
	defer srv.Close()

	if _, err := fetch(srv.URL); err == nil {
		t.Fatal("Fetch of an HTML page succeeded, want an error")
	}
}

// charsetReader is documented to accept any io.Reader, but it type asserts the
// input to io.ByteReader for ISO-8859-1 and panics otherwise.
func TestCharsetReaderAcceptsPlainReader(t *testing.T) {
	plain := strings.NewReader("caf\xe9")

	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("charsetReader panicked for a non-io.ByteReader input: %v", r)
			}
		}()
		var out interface{ Read([]byte) (int, error) }
		out, err = charsetReader("ISO-8859-1", plain)
		if err != nil {
			return
		}
		buf := make([]byte, 32)
		n, err := out.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatalf("Read: %v", err)
		}
		if got := string(buf[:n]); got != "café" {
			t.Errorf("decoded = %q, want %q", got, "café")
		}
	}()
	if err != nil {
		t.Fatalf("charsetReader: %v", err)
	}
}

func TestParseISO88591DeclaredCharset(t *testing.T) {
	body := "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>\n" +
		"<rss version=\"2.0\"><channel><title>caf\xe9</title><link>l</link><description>d</description>\n" +
		"<item><title>i</title><link>http://example.com/1</link><guid>g1</guid></item>\n" +
		"</channel></rss>"

	feed, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if feed.Title != "café" {
		t.Errorf("Title = %q, want %q", feed.Title, "café")
	}
}
