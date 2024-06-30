package domain

import (
	"github.com/jelinden/rssfetcher/app/rss"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type Feed struct {
	ID          primitive.ObjectID `json:"id" bson:"_id"`
	Name        string             `json:"feedTitle" bson:"feedTitle,omitempty"`
	URL         string             `json:"url" bson:"url,omitempty"`
	SiteURL     string             `json:"siteUrl" bson:"siteUrl,omitempty"`
	Category    rss.Category       `json:"category" bson:"category,omitempty"`
	SubCategory *rss.SubCategory   `json:"subCategory" bson:"subCategory,omitempty"`
	Language    string             `json:"language" bson:"language,omitempty"`
	Removed     bool               `json:"removed" bson:"removed,omitempty"`
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
