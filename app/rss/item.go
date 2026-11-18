package rss

import (
	"bytes"
	"encoding/xml"
	"log"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// decodeXML unmarshals an XML document using our charset reader, so feeds that
// declare a non UTF-8 encoding do not fail to decode.
func decodeXML(data []byte, v any) error {
	p := xml.NewDecoder(bytes.NewReader(data))
	p.CharsetReader = charsetReader
	return p.Decode(v)
}

func newFeed(title, description, link string, image *Image) *Feed {
	out := new(Feed)
	out.Title = title
	out.Description = description
	out.Link = link
	out.Image = image
	out.Items = make([]*Item, 0, itemFetchLimit)
	out.ItemMap = make(map[string]struct{}, itemFetchLimit)
	return out
}

// appendItem adds an item to the feed unless it has no GUID or is a duplicate.
// It reports whether the item was added.
func appendItem(feed *Feed, item *Item) bool {
	if item.GUID == "" {
		log.Printf("rss: item %q has neither a GUID nor a link, ignoring", item.Title)
		return false
	}
	if _, ok := feed.ItemMap[item.GUID]; ok {
		log.Printf("rss: item %q has a duplicate GUID %q, ignoring", item.Title, item.GUID)
		return false
	}
	feed.Items = append(feed.Items, item)
	feed.ItemMap[item.GUID] = struct{}{}
	feed.Unread++
	return true
}

// guidOrLink falls back to the item link when a feed omits the GUID.
func guidOrLink(guid, link string) string {
	if strings.TrimSpace(guid) != "" {
		return guid
	}
	return link
}

// imageURL reads the channel image without assuming it exists.
func imageURL(image *Image) string {
	if image == nil {
		return ""
	}
	return image.Url
}

// parseDate parses a feed timestamp, returning the zero time (and logging)
// instead of failing an entire feed because one item has a broken date.
func parseDate(candidates ...string) time.Time {
	for _, c := range candidates {
		if strings.TrimSpace(c) == "" {
			continue
		}
		t, err := parseTime(c)
		if err != nil {
			log.Printf("rss: unparsable date %q, ignoring: %v", c, err)
			continue
		}
		return t
	}
	return time.Time{}
}

// enclosure picks the thumbnail for an item: the explicit enclosure first,
// then any media namespace entry, then the first image of the content, and
// finally the channel image.
func enclosure(explicit Enclosure, media []*Media, content, channelImage string) Enclosure {
	if explicit.Url != "" {
		return explicit
	}
	for _, m := range media {
		if m != nil && m.Url != "" {
			return Enclosure{Url: m.Url}
		}
	}
	if src := firstImageSrc(content); src != "" {
		return Enclosure{Url: src}
	}
	return Enclosure{Url: channelImage}
}

// firstImageSrc returns the src attribute of the first <img> tag in content.
func firstImageSrc(content string) string {
	if !strings.Contains(content, "<img") {
		return ""
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(content))
	if err != nil {
		log.Printf("rss: scanning item content for images: %v", err)
		return ""
	}
	src, _ := doc.Find("img").First().Attr("src")
	return src
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// mediaURLs flattens the media extension slices into one list.
func mediaURLs(groups ...[]Media) []*Media {
	var out []*Media
	for _, g := range groups {
		for i := range g {
			out = append(out, &g[i])
		}
	}
	return out
}
