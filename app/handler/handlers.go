package handler

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"

	"github.com/jelinden/rssfetcher/app/domain"
	"github.com/jelinden/rssfetcher/app/mongo"
	"github.com/jelinden/rssfetcher/app/rss"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

var templates = template.Must(template.ParseGlob("public/tmpl/*"))

func ViewHandler(w http.ResponseWriter, r *http.Request) {
	feedList := mongo.GetFeeds(false)
	categoryList := mongo.GetCategories()
	subCategoryList := mongo.GetSubCategories()
	viewPage := domain.ViewPage{}
	viewPage.Feeds = feedList
	viewPage.Categories = categoryList
	viewPage.SubCategories = subCategoryList
	renderViewTemplate(w, "view", &viewPage)
}

func EditHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Println("editing", r.URL.Path)
	feed, err := mongo.GetFeed(r.URL.Path[6:])
	editPage := domain.EditPage{}
	if err != nil {
		feed = &domain.Feed{ID: primitive.NewObjectID()}
	}
	if feed.SubCategory == nil {
		feed.SubCategory = &rss.SubCategory{SubCategory: ""}
	}
	editPage.Feed = *feed
	editPage.Categories = mongo.GetCategories()
	editPage.SubCategories = mongo.GetSubCategories()
	renderTemplate(w, "edit", editPage)
}

func RemoveHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/delete/")
	feed, err := mongo.GetFeed(id)
	if err != nil {
		log.Println("error getting", id, err)
	} else {
		feed.Removed = true
		log.Println("saving", feed.ID.Hex(), feed.Name, feed.Removed)
		mongo.SaveFeedItem(*feed)
	}
	http.Redirect(w, r, "/view/", http.StatusFound)
}

func SaveHandler(w http.ResponseWriter, r *http.Request) {
	feed, err := mongo.GetFeed(r.URL.Path[6:])
	if err != nil {
		log.Println("error getting feed", err.Error())
	}
	lang := r.FormValue("language")
	category := mongo.GetCategory(r.FormValue("category"))
	if category.ID.IsZero() {
		category.ID = primitive.NewObjectID()
	}
	subCategory := mongo.GetSubCategory(r.FormValue("subCategory"))
	if subCategory.ID.IsZero() {
		subCategory.ID = primitive.NewObjectID()
	}
	name := r.FormValue("name")
	url := r.FormValue("url")
	siteURL := r.FormValue("siteUrl")
	mongo.SaveFeed(feed, lang, name, url, siteURL, category, subCategory)
	http.Redirect(w, r, "/view/", http.StatusFound)
}

func SaveCategoryHandler(w http.ResponseWriter, r *http.Request) {
	category := rss.Category{ID: primitive.NewObjectID(),
		Name:      r.FormValue("categoryName"),
		EnName:    r.FormValue("enName"),
		StyleName: r.FormValue("styleName")}
	mongo.SaveCategory(category)
	http.Redirect(w, r, "/view/", http.StatusFound)
}

func SaveSubCategoryHandler(w http.ResponseWriter, r *http.Request) {
	subCategory := rss.SubCategory{ID: primitive.NewObjectID(),
		SubCategory: r.FormValue("subCategory"),
	}
	mongo.SaveSubCategory(subCategory)
	http.Redirect(w, r, "/view/", http.StatusFound)
}

func renderTemplate(w http.ResponseWriter, tmpl string, f domain.EditPage) {
	err := templates.ExecuteTemplate(w, tmpl, f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func renderViewTemplate(w http.ResponseWriter, tmpl string, f *domain.ViewPage) {
	err := templates.ExecuteTemplate(w, tmpl, f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
