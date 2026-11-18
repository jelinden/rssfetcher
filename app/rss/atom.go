package rss

import (
	"encoding/xml"
	"time"
)

func parseAtom(data []byte) (*Feed, error) {
	feed := atomFeed{}
	if err := decodeXML(data, &feed); err != nil {
		return nil, err
	}

	out := newFeed(feed.Title, feed.Description, alternateLink(feed.Links), feed.channelImage())
	out.Refresh = time.Now().Add(10 * time.Minute)

	for _, item := range feed.Items {
		if len(out.Items) >= itemFetchLimit {
			break
		}
		// <content> is the entry body, <summary> the teaser: preferring the
		// summary throws the article away (and is the inverse of the RSS 2.0
		// rule, where content:encoded wins over description).
		content := firstNonEmpty(item.Content3, item.Content, item.InnerXML)
		link := alternateLink(item.Links)
		next := &Item{
			Title:     item.Title,
			Content:   content,
			Link:      link,
			Date:      parseDate(item.Date, item.Updated),
			GUID:      guidOrLink(item.GUID, link),
			Read:      false,
			Enclosure: enclosure(item.Enclosure, mediaURLs(item.Media, item.MediaContent), content, imageURL(out.Image)),
		}
		appendItem(out, next)
	}

	return out, nil
}

// alternateLink returns the "alternate" (i.e. human readable) link of an atom
// feed or entry. Feeds commonly list rel="self" first, so simply taking the
// first or last <link> yields the wrong URL.
func alternateLink(links []atomLink) string {
	fallback := ""
	for _, l := range links {
		if l.Href == "" {
			continue
		}
		if l.Rel == "" || l.Rel == "alternate" {
			return l.Href
		}
		if fallback == "" {
			fallback = l.Href
		}
	}
	return fallback
}

type atomFeed struct {
	XMLName xml.Name `xml:"feed"`
	Title   string   `xml:"title"`
	// Atom has no description element; the subtitle is the closest match.
	Description string     `xml:"subtitle"`
	Links       []atomLink `xml:"link"`
	// Atom's channel images are <logo> (bigger, for the site) and <icon> (a
	// favicon). There is no <image> element, which is what the old struct
	// matched - so the channel image was always empty.
	Logo    string     `xml:"logo"`
	Icon    string     `xml:"icon"`
	Items   []atomItem `xml:"entry"`
	Updated string     `xml:"updated"`
}

// channelImage prefers the logo and falls back to the icon.
func (a atomFeed) channelImage() *Image {
	url := a.Logo
	if url == "" {
		url = a.Icon
	}
	if url == "" {
		return &Image{}
	}
	return &Image{Url: url}
}

type atomItem struct {
	XMLName xml.Name `xml:"entry"`
	Title   string   `xml:"title"`
	// Content is the atom summary; Content3 is the full atom:content element.
	Content   string     `xml:"summary"`
	Content3  string     `xml:"content"`
	Links     []atomLink `xml:"link"`
	Date      string     `xml:"published"`
	Updated   string     `xml:"updated"`
	GUID      string     `xml:"id"`
	Enclosure Enclosure  `xml:"enclosure"`
	// InnerXML holds the raw entry XML and is used as a last resort to find a
	// thumbnail image.
	InnerXML string `xml:",innerxml"`
	// Both media:thumbnail and media:content carry a url; feeds use one or the
	// other (or only the latter), so collect both.
	Media        []Media `xml:"http://search.yahoo.com/mrss/ thumbnail"`
	MediaContent []Media `xml:"http://search.yahoo.com/mrss/ content"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}
