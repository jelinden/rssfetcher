package domain

import (
	"github.com/jelinden/rssfetcher/app/rss"
	"gopkg.in/mgo.v2/bson"
)

type Feed struct {
	ID          bson.ObjectId   `json:"id" bson:"_id"`
	Name        string          `json:"feedTitle" bson:"feedTitle"`
	URL         string          `json:"url" bson:"url"`
	SiteURL     string          `json:"siteUrl" bson:"siteUrl"`
	Category    rss.Category    `json:"category" bson:"category"`
	SubCategory rss.SubCategory `json:"subCategory" bson:"subCategory"`
	Language    string          `json:"language" bson:"language"`
}

type ViewPage struct {
	Feeds         []Feed
	Categories    []rss.Category
	SubCategories []rss.SubCategory
}

type EditPage struct {
	Feed          Feed
	Categories    []rss.Category
	SubCategories []rss.SubCategory
}
