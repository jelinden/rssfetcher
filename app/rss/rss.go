package rss

import (
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func Parse(data []byte) (*Feed, error) {
	if strings.Contains(string(data), "<rss") {
		return parseRSS2(data)
	} else if strings.Contains(string(data), "xmlns=\"http://purl.org/rss/1.0/\"") {
		return parseRSS1(data)
	} else {
		return parseAtom(data)
	}
}

type FetchFunc func() (resp *http.Response, err error)

var client = &http.Client{
	Timeout: time.Second * 12,
	Transport: &http.Transport{
		Dial: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 10 * time.Second,
		}).Dial,
		TLSHandshakeTimeout:   7 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

func Fetch(url string) (*Feed, error) {
	return fetchWithClient(url)
}

func fetchWithClient(url string) (*Feed, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	out, err := Parse(body)
	if err != nil {
		return nil, err
	}
	if out.Link == "" {
		out.Link = url
	}
	out.UpdateURL = url
	return out, nil
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
}

type Image struct {
	Title  string
	Url    string
	Height uint32
	Width  uint32
}

type Item struct {
	ID          primitive.ObjectID `json:"id" bson:"_id"`
	Title       string             `json:"rssTitle" bson:"rssTitle"`
	Content     string             `json:"rssDesc" bson:"rssDesc"`
	Link        string             `json:"rssLink" bson:"rssLink"`
	Date        time.Time          `json:"pubDate" bson:"pubDate"`
	GUID        string             `json:"rssGuid" bson:"rssGuid"`
	Read        bool               `json:"read" bson:"read"`
	Enclosure   Enclosure          `json:"enclosure" bson:"enclosure"`
	Category    Category           `json:"category" bson:"category"`
	SubCategory *SubCategory       `json:"subCategory" bson:"subCategory"`
	Language    string             `json:"language" bson:"language"`
	Source      string             `json:"rssSource" bson:"rssSource"`
	Clicks      int                `json:"clicks" bson:"clicks"`
}

type Category struct {
	ID        primitive.ObjectID `json:"id" bson:"_id"`
	Name      string             `json:"categoryName" bson:"categoryName"`
	StyleName string             `json:"styleName" bson:"styleName"`
	EnName    string             `json:"enName" bson:"enName"`
}

type SubCategory struct {
	ID          primitive.ObjectID `json:"id" bson:"_id"`
	SubCategory string             `json:"subCategory" bson:"subCategory"`
	EnName      string             `json:"enName" bson:"enName"`
}

type Enclosure struct {
	Url  string `xml:"url,attr"`
	Type string `xml:"type,attr"`
}

type Media struct {
	Url string `xml:"url,attr"`
}
