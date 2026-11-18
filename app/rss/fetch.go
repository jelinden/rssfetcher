package rss

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// FeedRequest is one download: the URL plus the validators of the previous
// successful fetch, so the publisher can answer 304 instead of the whole body.
type FeedRequest struct {
	URL          string
	ETag         string
	LastModified string
}

// FeedResponse is what a download produced. NotModified is the interesting case
// for the caller: nothing to store, and the stored validators stay valid.
type FeedResponse struct {
	Feed *Feed

	NotModified  bool
	ETag         string
	LastModified string
}

// FetchFunc downloads a feed. It is injected into the store so tests can replace
// the network with a fixture server.
type FetchFunc func(ctx context.Context, req FeedRequest) (*FeedResponse, error)

// itemFetchLimit bounds how many items a parser collects before the store picks
// the newest MaxItemsPerFeed of them. Feeds with thousands of entries must not
// make us hold all of them in memory.
const itemFetchLimit = 200

const (
	fetchTimeout = 12 * time.Second
	// defaultUserAgent is sent with every request; many feeds reject the
	// default "Go-http-client/1.1".
	defaultUserAgent = "rssfetcher/1.0 (+https://github.com/jelinden/rssfetcher)"
)

// ErrBodyTooLarge is returned when a feed body is larger than MaxBodyBytes.
var ErrBodyTooLarge = errors.New("rss: response body too large")

// HTTPError reports a non 2xx response from a feed URL.
type HTTPError struct {
	Status int
	URL    string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("rss: got status %d for %s", e.Status, e.URL)
}

// ErrPrivateURL is returned for a host that resolves to a loopback, private,
// link local or unique local address while the guard is enabled.
var ErrPrivateURL = errors.New("rss: refusing to fetch a private address")

// allowPrivateURLs mirrors -allow-private-urls. It is a package level switch
// because rss.Fetch is passed around as a plain function value; the default is
// the safe one (guard on).
var allowPrivateURLs atomic.Bool

// SetAllowPrivateURLs turns the address guard off. Fetching from an internal
// host is a legitimate thing to want, so it is allowed - just not silently.
func SetAllowPrivateURLs(allow bool) { allowPrivateURLs.Store(allow) }

// AllowPrivateURLs reports the current policy.
func AllowPrivateURLs() bool { return allowPrivateURLs.Load() }

// checkPublicAddress validates the literal IP the transport is about to dial.
// Doing it here rather than on the URL string is what makes the check survive a
// hostname that resolves to 127.0.0.1.
func checkPublicAddress(ip string) error {
	addr := net.ParseIP(ip)
	if addr == nil {
		return fmt.Errorf("rss: %q is not an IP address", ip)
	}
	switch {
	case addr.IsLoopback(),
		addr.IsPrivate(),
		addr.IsLinkLocalUnicast(),
		addr.IsLinkLocalMulticast(),
		addr.IsUnspecified(),
		addr.IsInterfaceLocalMulticast():
		return fmt.Errorf("%w: %s", ErrPrivateURL, ip)
	}
	// 169.254.169.254 (cloud metadata) is link local and covered above; the
	// carrier grade NAT range and documentation ranges are not, but they are
	// not reachable primitives either, so they are left alone.
	return nil
}

// newTransport builds the HTTP transport. It is a function so the tests can
// construct one without touching the package level switch.
func newTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 10 * time.Second}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if err := checkHostAddresses(host); err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(host, port))
		},
		MaxIdleConnsPerHost:   4,
		TLSHandshakeTimeout:   7 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// checkHostAddresses resolves host and validates every address it returns.
// Checking the resolved addresses (not the name) is what stops
// "internal.example.com" from pointing at the metadata endpoint. A host that
// does not resolve reports its DNS error rather than the policy error, so a typo
// in a feed URL does not look like a blocked address.
func checkHostAddresses(host string) error {
	if AllowPrivateURLs() {
		return nil
	}
	if host == "" {
		return errors.New("rss: feed URL without a host")
	}
	if ip := net.ParseIP(host); ip != nil {
		return checkPublicAddress(ip.String())
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil {
		return fmt.Errorf("rss: resolving %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("rss: %s resolves to no address", host)
	}
	for _, a := range addrs {
		if err := checkPublicAddress(a.IP.String()); err != nil {
			return err
		}
	}
	return nil
}

var client = &http.Client{
	Timeout:   fetchTimeout,
	Transport: newTransport(),
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		// A redirect is a second chance to aim us somewhere else, so the scheme
		// and the address policy are re-checked for every hop.
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return fmt.Errorf("rss: redirect to unsupported scheme %q", req.URL.Scheme)
		}
		host := req.URL.Hostname()
		if host == "" {
			return errors.New("rss: redirect to a URL without a host")
		}
		if err := checkHostAddresses(host); err != nil {
			return fmt.Errorf("redirecting: %w", err)
		}
		return nil
	},
}

// Fetch downloads and parses one feed, sending the stored validators when we
// have them.
func Fetch(ctx context.Context, req FeedRequest) (*FeedResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", defaultUserAgent)
	request.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml;q=0.9, text/xml;q=0.8, */*;q=0.5")
	if req.ETag != "" {
		request.Header.Set("If-None-Match", req.ETag)
	}
	if req.LastModified != "" {
		request.Header.Set("If-Modified-Since", req.LastModified)
	}

	resp, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() {
		// Drain so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
	}()

	out := &FeedResponse{ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}
	switch {
	case resp.StatusCode == http.StatusNotModified:
		// Nothing changed at the publisher: keep the caller's stored items and
		// validators. This is not an error.
		out.NotModified = true
		return out, nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, &HTTPError{Status: resp.StatusCode, URL: req.URL}
	}

	// Read one byte more than the limit so oversized bodies can be reported
	// instead of silently truncated into a parse error.
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(MaxBodyBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxBodyBytes {
		return nil, fmt.Errorf("%w: %d bytes limit", ErrBodyTooLarge, MaxBodyBytes)
	}

	feed, err := Parse(body)
	if err != nil {
		return nil, err
	}
	ResolveReferences(feed, req.URL)
	if feed.Link == "" {
		feed.Link = req.URL
	}
	feed.UpdateURL = req.URL
	out.Feed = feed
	return out, nil
}

// FetchURL is the plain download without conditional headers.
func FetchURL(ctx context.Context, rawURL string) (*Feed, error) {
	res, err := Fetch(ctx, FeedRequest{URL: rawURL})
	if err != nil {
		return nil, err
	}
	return res.Feed, nil
}

// ResolveReferences turns the relative references of a feed into absolute URLs.
//
// Two bugs hide in not doing this: the admin UI renders <a href="/news/1">, which
// then points at *this* server instead of the article; and a relative guid such
// as "/news/1" is identical on every site, so items from different publishers
// collapse into one stored document.
func ResolveReferences(feed *Feed, baseURL string) {
	if feed == nil {
		return
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Host == "" {
		return
	}
	feed.Link = resolve(base, feed.Link)
	feed.UpdateURL = resolve(base, feed.UpdateURL)
	if feed.Image != nil {
		feed.Image.Url = resolve(base, feed.Image.Url)
	}
	for _, item := range feed.Items {
		item.Link = resolve(base, item.Link)
		item.Enclosure.Url = resolve(base, item.Enclosure.Url)
		item.GUID = resolveGUID(base, item.GUID)
	}
}

func resolve(base *url.URL, ref string) string {
	if ref = strings.TrimSpace(ref); ref == "" {
		return ref
	}
	if isAbsolute(ref) {
		return ref
	}
	refURL, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return base.ResolveReference(refURL).String()
}

// resolveGUID turns a guid into something that is unique per publisher.
//
// A guid that is a relative reference ("/news/1") is identical on every site that
// uses that path, so it is resolved into an absolute URL. Anything else is an
// opaque identifier (tag:, urn:, a database key, a bare number): it stays as the
// publisher wrote it but is namespaced with the feed URL, which is what stops two
// publishers both using id "1234" from collapsing into one stored item.
func resolveGUID(base *url.URL, guid string) string {
	guid = strings.TrimSpace(guid)
	if guid == "" || isAbsolute(guid) {
		return guid
	}
	if strings.HasPrefix(guid, "/") || strings.HasPrefix(guid, "./") || strings.HasPrefix(guid, "../") {
		return resolve(base, guid)
	}
	return base.Scheme + "://" + base.Host + base.Path + "|" + guid
}

func isAbsolute(ref string) bool {
	u, err := url.Parse(ref)
	return err == nil && u.IsAbs()
}
