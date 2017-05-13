package handler

import (
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/jelinden/rssfetcher/app/domain"
	"github.com/jelinden/rssfetcher/app/mongo"
	"github.com/jelinden/rssfetcher/app/rss"
	"gopkg.in/mgo.v2/bson"
)

var templates = template.Must(template.ParseGlob("public/tmpl/*"))

func ViewHandler(w http.ResponseWriter, r *http.Request) {
	feedList := mongo.GetFeeds()
	renderViewTemplate(w, "view", &feedList)
}

func EditHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Println("editing", r.URL.Path)
	feed, err := mongo.GetFeed(r.URL.Path[6:])
	if err != nil {
		feed = &domain.Feed{ID: bson.NewObjectIdWithTime(time.Now())}
	}
	renderTemplate(w, "edit", feed)
}

func SaveHandler(w http.ResponseWriter, r *http.Request) {
	feed, _ := mongo.GetFeed(r.URL.Path[6:])
	lang := r.FormValue("language")
	category := rss.Category{ID: bson.NewObjectIdWithTime(time.Now()), Name: r.FormValue("category")}
	name := r.FormValue("name")
	url := r.FormValue("url")
	siteURL := r.FormValue("siteUrl")
	mongo.SaveFeed(feed, lang, name, url, siteURL, category)
	http.Redirect(w, r, "/view/", http.StatusFound)
}

func renderTemplate(w http.ResponseWriter, tmpl string, f *domain.Feed) {
	err := templates.ExecuteTemplate(w, tmpl, f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func renderViewTemplate(w http.ResponseWriter, tmpl string, f *[]domain.Feed) {
	err := templates.ExecuteTemplate(w, tmpl, f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
