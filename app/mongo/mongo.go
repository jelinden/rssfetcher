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

func SaveFeed(feed *domain.Feed, lang string, name string, url string, siteURL string, category rss.Category, subCategory rss.SubCategory) {
	m := mongoSession.Clone()
	defer m.Close()
	c := m.DB("news").C("feedcollection")
	if feed != nil && feed.ID.Valid() {
		log.Println("url: "+feed.URL, "updating, ID:", feed.ID)
		feed := &domain.Feed{
			ID:          feed.ID,
			Name:        name,
			URL:         url,
			SiteURL:     siteURL,
			Category:    category,
			SubCategory: subCategory,
			Language:    lang}
		c.UpdateId(feed.ID, feed)
	} else {
		log.Println("inserting", url, "ids", category.ID, subCategory.ID)
		feed := &domain.Feed{
			ID:          bson.NewObjectId(),
			Name:        name,
			URL:         url,
			SiteURL:     siteURL,
			Category:    category,
			SubCategory: subCategory,
			Language:    lang}
		err := c.Insert(&feed)
		log.Println("insert failed", err)
	}
}

func SaveCategory(cat rss.Category) {
	m := mongoSession.Clone()
	defer m.Close()
	c := m.DB("news").C("categorycollection")
	category := rss.Category{}
	c.Find(bson.M{"categoryName": cat.Name}).One(&category)
	if category.ID == "" {
		c.Insert(cat)
	}
}

func SaveSubCategory(subCat rss.SubCategory) {
	m := mongoSession.Clone()
	defer m.Close()
	c := m.DB("news").C("subcategorycollection")
	subCategory := rss.SubCategory{}
	c.Find(bson.M{"subCategory": subCat.SubCategory}).One(&subCategory)
	if subCategory.ID == "" {
		c.Insert(subCat)
	}
}

func GetFeed(feedID string) (*domain.Feed, error) {
	m := mongoSession.Clone()
	defer m.Close()
	c := m.DB("news").C("feedcollection")
	result := domain.Feed{}
	log.Println("loading " + feedID)
	if feedID == "" {
		return nil, errors.New("feedID was empty")
	}
	err := c.Find(bson.M{"_id": bson.ObjectIdHex(feedID)}).One(&result)
	if err != nil {
		return nil, err
	}
	feedAsJSON, _ := json.Marshal(result)
	log.Println("loaded " + string(feedAsJSON))
	return &result, nil
}

type feedStruct struct {
	RSSFeed domain.Feed
	Item    rss.Feed
}

func GetNews(feeds []domain.Feed) {
	log.Println("getting news")
	m := mongoSession.Clone()
	defer m.Close()
	collection := m.DB("news").C("newscollection")
	c := make(chan *feedStruct)
	go getNewsFeeds(feeds, c)
	counter := 0
	for i := range c {
		counter++
		if i != nil {
			t := time.Now()
			saveNewsItems(i.Item, i.RSSFeed, *collection)
			log.Println("feed "+i.RSSFeed.Name+" "+i.RSSFeed.Category.Name, time.Now().Sub(t).Seconds())
		}
		if counter == len(feeds) {
			close(c)
		}
	}
	log.Println("got all and closed the channel")
}

func getNewsFeeds(feeds []domain.Feed, c chan *feedStruct) {
	for i := range feeds {
		go getNewsFeed(feeds, c, i)
	}
}

func getNewsFeed(feeds []domain.Feed, c chan *feedStruct, i int) {
	item, err := rss.Fetch(feeds[i].URL)
	if err != nil {
		log.Println("err", feeds[i].URL, err)
		c <- nil
	} else {
		items := feedStruct{RSSFeed: feeds[i], Item: *item}
		c <- &items
	}
}
func saveNewsItems(items rss.Feed, feed domain.Feed, collection mgo.Collection) {
	for k, item := range items.Items {
		if k > 4 {
			break
		}
		item.Title = strings.TrimSpace(item.Title)
		item.Link = strings.TrimSpace(item.Link)
		item.Content = strings.TrimSpace(item.Content)
		item.GUID = strings.TrimSpace(item.GUID)
		if item.Title != "" && item.Link != "" {
			item.Language = feed.Language
			item.Category = feed.Category
			item.SubCategory = feed.SubCategory
			item.Source = feed.Name
			item.Language = feed.Language

			if item.Date.After(time.Now()) {
				item.Date = time.Now()
			}
			result := rss.Item{}
			if len(item.GUID) != 0 {
				err := collection.Find(bson.M{"rssLink": item.Link}).Select(bson.M{"_id": 1, "pubDate": 1, "clicks": 1}).One(&result)
				if err == nil && result.ID.Valid() {
					item.ID = result.ID
					if result.Date.Unix() > 0 {
						item.Date = result.Date
					}
					item.Clicks = result.Clicks
					err2 := collection.UpdateId(item.ID, &item)
					if err2 != nil {
						log.Println("updating rss with id failed " + err2.Error())
					}
				} else if err != nil && len(item.GUID) != 0 {
					item.ID = bson.NewObjectId()
					err3 := collection.Insert(&item)
					if err3 != nil {
						log.Println("inserting rss failed " + err3.Error())
					}
				}
				//fmt.Println("  " + item.Date.String() + " " + item.Title)
			}
		}
	}
}

func GetFeeds() []domain.Feed {
	m := mongoSession.Clone()
	defer m.Close()
	c := m.DB("news").C("feedcollection")
	feedList := []domain.Feed{}
	_ = c.Find(bson.M{}).All(&feedList)
	return feedList
}

func GetCategories() []rss.Category {
	m := mongoSession.Clone()
	defer m.Close()
	c := m.DB("news").C("categorycollection")
	categoryList := []rss.Category{}
	_ = c.Find(bson.M{}).All(&categoryList)
	return categoryList
}

func GetSubCategories() []rss.SubCategory {
	m := mongoSession.Clone()
	defer m.Close()
	c := m.DB("news").C("subcategorycollection")
	subCategoryList := []rss.SubCategory{}
	_ = c.Find(bson.M{}).All(&subCategoryList)
	return subCategoryList
}

func GetCategory(name string) rss.Category {
	m := mongoSession.Clone()
	defer m.Close()
	c := m.DB("news").C("categorycollection")
	category := rss.Category{}
	_ = c.Find(bson.M{"categoryName": name}).One(&category)
	return category
}

func GetSubCategory(name string) rss.SubCategory {
	m := mongoSession.Clone()
	defer m.Close()
	c := m.DB("news").C("subcategorycollection")
	subCategory := rss.SubCategory{}
	_ = c.Find(bson.M{"subCategory": name}).One(&subCategory)
	return subCategory
}
