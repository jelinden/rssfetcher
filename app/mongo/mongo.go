package mongo

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/jelinden/rssfetcher/app/domain"
	"github.com/jelinden/rssfetcher/app/rss"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type MongoRepository struct {
	Client *mongo.Client
}

var MongoClient MongoRepository

func InitMongoClient(mongoAddress string) *mongo.Client {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoAddress))
	if err != nil {
		log.Println("connection lost ", err)
	}
	err = client.Ping(ctx, nil)
	if err != nil {
		log.Println("mongo connection failed ", err)
	}
	return client
}

func SaveFeedItem(feed domain.Feed) {
	if feed.SubCategory.ID.IsZero() {
		feed.SubCategory = nil
	}
	c := MongoClient.Client.Database("news").Collection("feedcollection")

	log.Println("url: "+feed.URL, "updating, ID:", feed.ID.Hex())

	_, err := c.UpdateOne(context.Background(),
		bson.D{{Key: "_id", Value: feed.ID}},
		bson.D{{Key: "$set", Value: feed}})
	if err != nil {
		log.Println(err)
	}

}

func SaveFeed(feed *domain.Feed, lang string, name string, url string, siteURL string, category rss.Category, subCategory rss.SubCategory) {
	c := MongoClient.Client.Database("news").Collection("feedcollection")
	if feed != nil {
		log.Println("url: "+feed.URL, "updating, ID:", feed.ID)
		feed := &domain.Feed{
			ID:          feed.ID,
			Name:        name,
			URL:         url,
			SiteURL:     siteURL,
			Category:    category,
			SubCategory: &subCategory,
			Language:    lang}

		_, err := c.UpdateOne(context.Background(),
			bson.D{{Key: "_id", Value: feed.ID}},
			bson.D{{Key: "$set", Value: feed}})
		if err != nil {
			log.Println(err)
		}
	} else {
		log.Println("inserting", url, "ids", category.ID, subCategory.ID)
		feed := &domain.Feed{
			ID:          primitive.NewObjectID(),
			Name:        name,
			URL:         url,
			SiteURL:     siteURL,
			Category:    category,
			SubCategory: &subCategory,
			Language:    lang}
		_, err := c.InsertOne(context.Background(), &feed)
		if err != nil {
			log.Println("insert failed", err)
		}
	}
}

func SaveCategory(cat rss.Category) {
	c := MongoClient.Client.Database("news").Collection("categorycollection")
	category := rss.Category{}
	result := c.FindOne(context.Background(), bson.M{"categoryName": cat.Name})
	err := result.Decode(&category)
	if err != nil {
		log.Println("err", err)
	}
	if category.ID.IsZero() {
		_, err = c.InsertOne(context.Background(), cat)
		if err != nil {
			log.Println("err", err)
		}
	}
}

func SaveSubCategory(subCat rss.SubCategory) {
	c := MongoClient.Client.Database("news").Collection("subcategorycollection")
	subCategory := rss.SubCategory{}
	result := c.FindOne(context.Background(), bson.M{"subCategory": subCat.SubCategory})
	err := result.Decode(&subCategory)
	if err != nil {
		log.Println("err", err)
	}
	if subCategory.ID.IsZero() {
		c.InsertOne(context.Background(), subCat)
	}
}

func GetFeed(feedID string) (*domain.Feed, error) {
	c := MongoClient.Client.Database("news").Collection("feedcollection")
	log.Println("loading " + feedID)
	if feedID == "" {
		return nil, errors.New("feedID was empty")
	}
	feedId, err := primitive.ObjectIDFromHex(feedID)
	if err != nil {
		log.Println(err)
		return nil, err
	}
	result := c.FindOne(context.Background(), bson.M{"_id": feedId})
	var feed domain.Feed
	err = result.Decode(&feed)
	if err != nil {
		log.Println(err)
		return nil, err
	}
	feedAsJSON, _ := json.Marshal(feed)
	log.Println("loaded " + string(feedAsJSON))
	return &feed, nil
}

type feedStruct struct {
	RSSFeed domain.Feed
	Item    rss.Feed
}

func GetNews(feeds []domain.Feed) {
	if len(feeds) > 0 {
		log.Println("getting news")
		c := make(chan *feedStruct)
		go getNewsFeeds(feeds, c)
		counter := 0
		t := time.Now()
		for i := range c {
			counter++
			if i != nil {
				saveNewsItems(i.Item, i.RSSFeed)
				log.Println("feed", i.RSSFeed.Name, i.RSSFeed.Category.Name, time.Since(t).Seconds(), "s")
			}
			if counter == len(feeds) {
				close(c)
			}
		}
		log.Println("got all and closed the channel")
	}
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

func saveNewsItems(items rss.Feed, feed domain.Feed) {
	collection := MongoClient.Client.Database("news").Collection("newscollection")
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
			if !feed.SubCategory.ID.IsZero() {
				item.SubCategory = feed.SubCategory
			} else {
				item.SubCategory = nil
			}
			item.Source = feed.Name
			item.Language = feed.Language

			if item.Date.After(time.Now()) {
				item.Date = time.Now()
			}
			result := rss.Item{}
			if len(item.GUID) > 3 {
				item.GUID = getGUID(item.GUID)
				r := collection.FindOne(context.Background(), bson.M{"rssGuid": item.GUID})
				err := r.Decode(&result)
				if err == nil {
					item.ID = result.ID
					if result.Date.Unix() > 0 {
						item.Date = result.Date
					}
					item.Clicks = result.Clicks

					_, err2 := collection.UpdateOne(context.Background(), bson.M{"_id": item.ID}, bson.D{{Key: "$set", Value: item}})
					if err2 != nil {
						log.Println("updating rss with id failed " + err2.Error())
					}
				} else if len(item.GUID) != 0 {
					item.ID = primitive.NewObjectID()
					_, err3 := collection.InsertOne(context.Background(), &item)
					if err3 != nil {
						log.Println("inserting rss failed " + err3.Error())
					}
				}
			}
		}
	}
}

func getGUID(guid string) string {
	return strings.Replace(strings.Replace(guid, "http://", "", 1), "https://", "", 1)
}

func GetFeeds(args ...bool) []domain.Feed {
	var query = make(map[string]interface{})
	if len(args) == 1 && args[0] {
		query = bson.M{"removed": bson.M{"$ne": true}}
	} else {
		query = bson.M{}
	}
	c := MongoClient.Client.Database("news").Collection("feedcollection")
	feedList := []domain.Feed{}

	cursor, err := c.Find(context.Background(), query)
	if err != nil {
		log.Println(err)
		return nil
	}
	if err = cursor.All(context.Background(), &feedList); err != nil {
		log.Println(err)
		return nil
	}
	return feedList
}

func GetCategories() []rss.Category {
	c := MongoClient.Client.Database("news").Collection("categorycollection")
	categoryList := []rss.Category{}
	cursor, err := c.Find(context.Background(), bson.M{})
	if err != nil {
		log.Println(err)
		return nil
	}
	if err := cursor.All(context.Background(), &categoryList); err != nil {
		log.Println(err)
		return nil
	}
	return categoryList
}

func GetSubCategories() []rss.SubCategory {
	c := MongoClient.Client.Database("news").Collection("subcategorycollection")
	subCategoryList := []rss.SubCategory{}
	cursor, err := c.Find(context.Background(), bson.M{})
	if err != nil {
		log.Println(err)
		return nil
	}
	if err = cursor.All(context.Background(), &subCategoryList); err != nil {
		log.Println(err)
	}
	return subCategoryList
}

func GetCategory(name string) rss.Category {

	c := MongoClient.Client.Database("news").Collection("categorycollection")
	category := rss.Category{}
	result := c.FindOne(context.Background(), bson.M{"categoryName": name})
	result.Decode(&category)
	return category
}

func GetSubCategory(name string) rss.SubCategory {

	c := MongoClient.Client.Database("news").Collection("subcategorycollection")
	subCategory := rss.SubCategory{}
	result := c.FindOne(context.Background(), bson.M{"subCategory": name})
	result.Decode(&subCategory)
	return subCategory
}
