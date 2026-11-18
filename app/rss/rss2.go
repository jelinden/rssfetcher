package rss

import (
	"encoding/xml"
	"fmt"
	"time"
)

func parseRSS2(data []byte) (*Feed, error) {
	feed := rss2_0Feed{}
	if err := decodeXML(data, &feed); err != nil {
		return nil, err
	}
	if feed.Channel == nil {
		return nil, fmt.Errorf("rss: no channel found in %s", snippet(data))
	}

	channel := feed.Channel
	out := newFeed(channel.Title, channel.Description, channel.Link, channel.Image.Image())
	// <ttl> is a suggestion in *minutes*; the scheduler uses it instead of
	// hammering the publisher on our own interval.
	if channel.MinsToLive > 0 {
		out.TTL = time.Duration(channel.MinsToLive) * time.Minute
	}

	for _, item := range channel.Items {
		if len(out.Items) >= itemFetchLimit {
			break
		}
		content := firstNonEmpty(item.ContentEncoded, item.Content)
		next := &Item{
			Title:     item.Title,
			Content:   content,
			Link:      item.Link,
			Date:      parseDate(item.Date, item.PubDate),
			GUID:      guidOrLink(item.GUID, item.Link),
			Read:      false,
			Enclosure: enclosure(item.Enclosure, mediaURLs(item.Media, item.Media2), content, imageURL(out.Image)),
		}
		appendItem(out, next)
	}

	return out, nil
}

type rss2_0Feed struct {
	XMLName xml.Name       `xml:"rss"`
	Channel *rss2_0Channel `xml:"channel"`
}

type rss2_0Channel struct {
	XMLName     xml.Name     `xml:"channel"`
	Title       string       `xml:"title"`
	Description string       `xml:"description"`
	Link        string       `xml:"link"`
	Image       rss2_0Image  `xml:"image"`
	Items       []rss2_0Item `xml:"item"`
	MinsToLive  int          `xml:"ttl"`
	SkipHours   []int        `xml:"skipHours>hour"`
	SkipDays    []string     `xml:"skipDays>day"`
}

type rss2_0Item struct {
	XMLName xml.Name `xml:"item"`
	Title   string   `xml:"title"`
	// Content is the RSS 2.0 description; ContentEncoded (from the content:
	// namespace) carries the full text and wins when present.
	Content        string    `xml:"description"`
	ContentEncoded string    `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
	Link           string    `xml:"link"`
	PubDate        string    `xml:"pubDate"`
	Date           string    `xml:"date"`
	GUID           string    `xml:"guid"`
	Enclosure      Enclosure `xml:"enclosure"`
	Media          []Media   `xml:"http://search.yahoo.com/mrss/ thumbnail"`
	Media2         []Media   `xml:"http://search.yahoo.com/mrss/ content"`
}

type rss2_0Image struct {
	XMLName xml.Name `xml:"image"`
	Title   string   `xml:"title"`
	Url     string   `xml:"url"`
	Height  int      `xml:"height"`
	Width   int      `xml:"width"`
}

func (i *rss2_0Image) Image() *Image {
	return &Image{
		Title:  i.Title,
		Url:    i.Url,
		Height: uint32(i.Height),
		Width:  uint32(i.Width),
	}
}
