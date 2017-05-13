package mongo

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jelinden/rssfetcher/app/domain"
	"github.com/jelinden/rssfetcher/app/rss"

	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

var mongoSession *mgo.Session

func InitMongo(mongoAddress string) {
	maxWait := time.Duration(5 * time.Second)
	var err error
	mongoSession, err = mgo.DialWithTimeout(mongoAddress, maxWait)
	if err != nil {
		fmt.Println("connection lost")
	}
	mongoSession.SetMode(mgo.Monotonic, true)
}

func SaveFeed(feed *domain.Feed, lang string, name string, url string, siteURL string, category rss.Category) {
	m := mongoSession.Clone()
	defer m.Close()
	c := m.DB("news").C("feedcollection")
	if feed != nil {
		fmt.Println("url: "+feed.URL, "updating")
		feed := &domain.Feed{
			ID:       feed.ID,
			Name:     name,
			URL:      url,
			SiteURL:  siteURL,
			Category: category,
			Language: lang}
		c.UpdateId(feed.ID, feed)
	} else {
		fmt.Println("inserting")
		feed := &domain.Feed{
			ID:       bson.NewObjectIdWithTime(time.Now()),
			Name:     name,
			URL:      url,
			SiteURL:  siteURL,
			Category: category,
			Language: lang}
		err := c.Insert(&feed)
		log.Println("insert failed", err)
	}
}

func GetFeed(feedID string) (*domain.Feed, error) {
	m := mongoSession.Clone()
	defer m.Close()
	c := m.DB("news").C("feedcollection")
	result := domain.Feed{}
	fmt.Println("loading " + feedID)
	if feedID == "" {
		return nil, errors.New("feedID was empty")
	}
	err := c.Find(bson.M{"_id": bson.ObjectIdHex(feedID)}).One(&result)
	if err != nil {
		return nil, err
	}
	feedAsJSON, _ := json.Marshal(result)
	fmt.Println("loaded " + string(feedAsJSON))
	return &result, nil
}

func GetNews(feeds []domain.Feed) {
	m := mongoSession.Clone()
	defer m.Close()
	c := m.DB("news").C("newscollection")
	log.Println("getting news")
	for i := range feeds {
		items, er := rss.Fetch(feeds[i].URL)
		if er != nil {
			fmt.Println(er)
		} else {
			fmt.Println("feed " + feeds[i].Name + " " + feeds[i].Category.Name)
			for k, item := range items.Items {
				if k > 4 {
					break
				}
				item.Title = strings.TrimSpace(item.Title)
				item.Link = strings.TrimSpace(item.Link)
				item.Content = strings.TrimSpace(item.Content)
				item.GUID = strings.TrimSpace(item.GUID)
				if item.Title != "" && item.Link != "" {
					item.Language = feeds[i].Language
					item.Category = feeds[i].Category
					item.Source = feeds[i].Name
					item.Language = feeds[i].Language

					if item.Date.After(time.Now()) {
						item.Date = time.Now()
					}
					result := rss.Item{}
					if len(item.GUID) != 0 {
						err := c.Find(bson.M{"rssGuid": item.GUID}).Select(bson.M{"_id": 1, "pubDate": 1, "clicks": 1}).One(&result)
						if err == nil && result.ID.Valid() {
							item.ID = result.ID
							if result.Date.Unix() > 0 {
								item.Date = result.Date
							}
							item.Clicks = result.Clicks
							err2 := c.UpdateId(result.ID, &item)
							if err2 != nil {
								log.Println("updating rss with id failed " + err2.Error())
							}
						} else if err != nil && len(item.GUID) != 0 {
							item.ID = bson.NewObjectId()
							err3 := c.Insert(&item)
							if err3 != nil {
								log.Println("inserting rss failed " + err3.Error())
							}
						}
						fmt.Println("  " + item.Date.String() + " " + item.Title)
					}
				}
			}
			index := mgo.Index{
				Key:        []string{"guid"},
				Unique:     true,
				DropDups:   true,
				Background: true,
				Sparse:     true,
			}
			c.EnsureIndex(index)

			indexFind := mgo.Index{
				Key:        []string{"language", "-pubDate"},
				Unique:     false,
				DropDups:   false,
				Background: true,
				Sparse:     true,
			}
			c.EnsureIndex(indexFind)
		}
	}
}

func GetFeeds() []domain.Feed {
	if mongoSession != nil {
		m := mongoSession.Clone()
		defer m.Close()
		c := m.DB("news").C("feedcollection")
		feedList := []domain.Feed{}
		_ = c.Find(bson.M{}).All(&feedList)
		return feedList
	}
	fmt.Println("mongoSession is nil")
	return nil
}
