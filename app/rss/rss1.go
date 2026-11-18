package rss

import (
	"encoding/xml"
	"fmt"
)

func parseRSS1(data []byte) (*Feed, error) {
	feed := rss1_0Feed{}
	if err := decodeXML(data, &feed); err != nil {
		return nil, err
	}
	if feed.Channel == nil {
		return nil, fmt.Errorf("rss: no channel found in %s", snippet(data))
	}

	channel := feed.Channel
	out := newFeed(channel.Title, channel.Description, channel.Link, channel.Image.Image())

	for _, item := range feed.Items {
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
			Enclosure: enclosure(item.Enclosure, mediaURLs(item.Media), content, channel.Image.Url),
		}
		appendItem(out, next)
	}

	return out, nil
}

type rss1_0Feed struct {
	XMLName xml.Name       `xml:"RDF"`
	Channel *rss1_0Channel `xml:"channel"`
	Items   []rss1_0Item   `xml:"item"`
}

type rss1_0Channel struct {
	XMLName     xml.Name    `xml:"channel"`
	Title       string      `xml:"title"`
	Description string      `xml:"description"`
	Link        string      `xml:"link"`
	Image       rss1_0Image `xml:"image"`
	MinsToLive  int         `xml:"ttl"`
	SkipHours   []int       `xml:"skipHours>hour"`
	SkipDays    []string    `xml:"skipDays>day"`
}

type rss1_0Item struct {
	XMLName        xml.Name  `xml:"item"`
	Title          string    `xml:"title"`
	Content        string    `xml:"description"`
	ContentEncoded string    `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
	Link           string    `xml:"link"`
	PubDate        string    `xml:"pubDate"`
	Date           string    `xml:"date"`
	GUID           string    `xml:"guid"`
	Enclosure      Enclosure `xml:"enclosure"`
	Media          []Media   `xml:"http://search.yahoo.com/mrss/ content"`
}

type rss1_0Image struct {
	XMLName xml.Name `xml:"image"`
	Title   string   `xml:"title"`
	Url     string   `xml:"url"`
	Height  int      `xml:"height"`
	Width   int      `xml:"width"`
}

func (i *rss1_0Image) Image() *Image {
	return &Image{
		Title:  i.Title,
		Url:    i.Url,
		Height: uint32(i.Height),
		Width:  uint32(i.Width),
	}
}
