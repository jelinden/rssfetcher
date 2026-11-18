package rss

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// MaxBodyBytes is the largest feed body we are willing to read. A hostile or
// misbehaving server must not be able to make us allocate arbitrarily much
// memory. It applies to the decompressed stream, so a gzip bomb is bounded too.
const MaxBodyBytes = 5 << 20

// DefaultMaxItemsPerFeed is how many items the store keeps per feed. The parsers
// collect more than that (itemFetchLimit, see fetch.go) and the newest ones are
// selected by date, so a feed that lists its oldest items first does not win.
const DefaultMaxItemsPerFeed = 25

// maxItems is the runtime value behind MaxItemsPerFeed, set from
// -max-items-per-feed. An atomic keeps a concurrent fetch and a test honest
// without a mutex in the hot path.
var maxItems atomic.Int64

func init() { maxItems.Store(DefaultMaxItemsPerFeed) }

// MaxItemsPerFeed is the current per feed storage limit.
func MaxItemsPerFeed() int {
	if n := int(maxItems.Load()); n > 0 {
		return n
	}
	return DefaultMaxItemsPerFeed
}

// SetMaxItemsPerFeed changes the limit; zero or negative restores the default.
func SetMaxItemsPerFeed(n int) {
	if n <= 0 {
		n = DefaultMaxItemsPerFeed
	}
	maxItems.Store(int64(n))
}

// NewestItems returns at most n items sorted newest first. Items without a date
// sort after the dated ones, so a feed that publishes no timestamps at all is
// left in the order the publisher chose.
func NewestItems(items []*Item, n int) []*Item {
	if n <= 0 || len(items) == 0 {
		return nil
	}
	sorted := make([]*Item, len(items))
	copy(sorted, items)

	hasDates := false
	for _, item := range sorted {
		if item != nil && !item.Date.IsZero() {
			hasDates = true
			break
		}
	}
	if hasDates {
		sort.SliceStable(sorted, func(i, j int) bool {
			a, b := sorted[i], sorted[j]
			switch {
			case a == nil:
				return false
			case b == nil:
				return true
			case a.Date.IsZero():
				return false
			case b.Date.IsZero():
				return true
			default:
				return a.Date.After(b.Date)
			}
		})
	}
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	return sorted
}

// Parse sniffs the document type and decodes it with the matching parser.
func Parse(data []byte) (*Feed, error) {
	head := data
	if len(head) > 4096 {
		head = head[:4096]
	}
	trimmed := strings.TrimLeft(string(head), " \t\r\n")
	if !strings.HasPrefix(trimmed, "<") {
		return nil, fmt.Errorf("rss: body is not XML: %s", snippet(data))
	}
	switch {
	case strings.Contains(trimmed, "<rss"):
		return parseRSS2(data)
	case strings.Contains(trimmed, `xmlns="http://purl.org/rss/1.0/"`),
		strings.Contains(trimmed, `xmlns='http://purl.org/rss/1.0/'`):
		return parseRSS1(data)
	case strings.Contains(trimmed, "<feed"):
		return parseAtom(data)
	default:
		return nil, fmt.Errorf("rss: unknown feed format: %s", snippet(data))
	}
}

// snippet returns a short, loggable preview of a feed body. The previous code
// formatted entire documents into error messages.
func snippet(data []byte) string {
	const max = 200
	s := string(data)
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}

type Feed struct {
	Nickname    string
	Title       string
	Description string
	Link        string
	UpdateURL   string
	Image       *Image
	Items       []*Item
	ItemMap     map[string]struct{}
	Refresh     time.Time
	Unread      uint32

	// TTL is what the publisher asked for (<ttl> is in minutes, and
	// <sy:updatePeriod> in Atom). The scheduler uses it to back off; feeds that
	// do not say keep the configured interval.
	TTL time.Duration
}

type Image struct {
	Title  string
	Url    string
	Height uint32
	Width  uint32
}

type Item struct {
	ID          bson.ObjectID `json:"id" bson:"_id"`
	Title       string        `json:"rssTitle" bson:"rssTitle"`
	Content     string        `json:"rssDesc" bson:"rssDesc"`
	Link        string        `json:"rssLink" bson:"rssLink"`
	Date        time.Time     `json:"pubDate" bson:"pubDate"`
	GUID        string        `json:"rssGuid" bson:"rssGuid"`
	Read        bool          `json:"read" bson:"read"`
	Enclosure   Enclosure     `json:"enclosure" bson:"enclosure"`
	Category    Category      `json:"category" bson:"category"`
	SubCategory *SubCategory  `json:"subCategory" bson:"subCategory"`
	Language    string        `json:"language" bson:"language"`
	Source      string        `json:"rssSource" bson:"rssSource"`
	Clicks      int           `json:"clicks" bson:"clicks"`
}

type Category struct {
	ID        bson.ObjectID `json:"id" bson:"_id"`
	Name      string        `json:"categoryName" bson:"categoryName"`
	StyleName string        `json:"styleName" bson:"styleName"`
	EnName    string        `json:"enName" bson:"enName"`
}

type SubCategory struct {
	ID          bson.ObjectID `json:"id" bson:"_id"`
	SubCategory string        `json:"subCategory" bson:"subCategory"`
	EnName      string        `json:"enName" bson:"enName"`
}

type Enclosure struct {
	Url  string `xml:"url,attr"`
	Type string `xml:"type,attr"`
}

type Media struct {
	Url string `xml:"url,attr"`
}
