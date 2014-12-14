package rss

import (
	"gopkg.in/mgo.v2/bson"
	"io/ioutil"
	"net/http"
	"strings"
	"time"
)

func Parse(data []byte) (*Feed, error) {

	if strings.Contains(string(data), "<rss") {
		return parseRSS2(data)
	} else if strings.Contains(string(data), "xmlns=\"http://purl.org/rss/1.0/\"") {
		return parseRSS1(data)
	} else {
		return parseAtom(data)
	}

	panic("Unreachable.")
}

type FetchFunc func() (resp *http.Response, err error)

func Fetch(url string) (*Feed, error) {
	return FetchByClient(url, http.DefaultClient)
}

func FetchByClient(url string, client *http.Client) (*Feed, error) {
	fetchFunc := func() (resp *http.Response, err error) {
		return client.Get(url)
	}
	return FetchByFunc(fetchFunc, url)
}

func FetchByFunc(fetchFunc FetchFunc, url string) (*Feed, error) {
	resp, err := fetchFunc()
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
	Id        bson.ObjectId `json:"id" bson:"_id"`
	Title     string
	Content   string
	Link      string
	Date      time.Time
	ID        string
	Read      bool
	Enclosure Enclosure
	Category  Category
	Language  int
	Source    string
	Clicks    int
	Liked     int
	Unliked   int
}

type Category struct {
	Name      string
	StyleName string
	EnName    string
}

type Enclosure struct {
	Url  string `xml:"url,attr"`
	Type string `xml:"type,attr"`
}

type Media struct {
	Url string `xml:"url,attr"`
}
