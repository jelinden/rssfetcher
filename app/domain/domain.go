package domain

import (
	"github.com/jelinden/rssfetcher/app/rss"
	"gopkg.in/mgo.v2/bson"
)

type Feed struct {
	ID       bson.ObjectId `json:"id" bson:"_id"`
	Name     string        `json:"feedTitle" bson:"feedTitle"`
	URL      string        `json:"url" bson:"url"`
	SiteURL  string        `json:"siteUrl" bson:"siteUrl"`
	Category rss.Category  `json:"category" bson:"category"`
	Language string        `json:"language" bson:"language"`
}
