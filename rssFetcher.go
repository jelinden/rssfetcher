package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/jelinden/rssFetcher/rss"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
	"html/template"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

var validPath = regexp.MustCompile("^/(edit|save|view)/([a-zA-Z0-9]*)$")
var templates = template.Must(template.ParseFiles("edit.html", "view.html"))
var mongoAddress = flag.String("address", "localhost", "mongo address")
var (
	addr = flag.Bool("addr", false, "find open address and print to final-port.txt")
)

func main() {
	flag.Parse()
	fmt.Println("mongoAddress " + *mongoAddress)
	session, err := mgo.Dial(*mongoAddress)
	if err != nil {
		log.Fatal("not connected " + err.Error())
		panic(err)
	}
	defer session.Close()
	session.SetMode(mgo.Monotonic, true)

	go doEvery(80*time.Second, getFeeds, session)
	flag.Parse()
	http.HandleFunc("/view/", makeHandler(viewHandler, session))
	http.HandleFunc("/edit/", makeHandler(editHandler, session))
	http.HandleFunc("/save/", makeHandler(saveHandler, session))
	if *addr {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatal(err)
		}
		err = ioutil.WriteFile("final-port.txt", []byte(l.Addr().String()), 0644)
		if err != nil {
			log.Fatal(err)
		}
		s := &http.Server{}
		s.Serve(l)
		return
	}

	http.ListenAndServe(":9200", nil)
}

func loadFeed(feedId string, session *mgo.Session) (*Feed, error) {
	c := session.DB("uutispuro").C("feed")
	result := Feed{}
	fmt.Println("loading " + feedId)
	if feedId == "" {
		return nil, errors.New("feedId was empty")
	}
	err := c.Find(bson.M{"_id": bson.ObjectIdHex(feedId)}).One(&result)
	if err != nil {
		return nil, err
	}
	feedAsJson, _ := json.Marshal(result)
	fmt.Println("loaded " + string(feedAsJson))
	return &result, nil
}

func makeHandler(fn func(http.ResponseWriter, *http.Request, string, *mgo.Session), session *mgo.Session) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := validPath.FindStringSubmatch(r.URL.Path)
		if m == nil {
			http.NotFound(w, r)
			return
		}
		fn(w, r, m[2], session)
	}
}

func viewHandler(w http.ResponseWriter, r *http.Request, feedId string, session *mgo.Session) {
	c := session.DB("uutispuro").C("feed")
	feedList := []Feed{}
	_ = c.Find(bson.M{}).All(&feedList)
	renderViewTemplate(w, "view", &feedList)
}

func editHandler(w http.ResponseWriter, r *http.Request, feedId string, session *mgo.Session) {
	feed, err := loadFeed(feedId, session)
	if err != nil {
		feed = &Feed{Id: bson.NewObjectId()}
	}
	renderTemplate(w, "edit", feed)
}

func saveHandler(w http.ResponseWriter, r *http.Request, feedId string, session *mgo.Session) {
	feed, _ := loadFeed(r.URL.Path[6:], session)
	c := session.DB("uutispuro").C("feed")
	lang, _ := strconv.Atoi(r.FormValue("language"))
	category := rss.Category{Name: r.FormValue("category")}

	if feed != nil {
		fmt.Println("url: " + feed.Url + r.URL.Path[6:])
		fmt.Println("update")
		feed := &Feed{
			Id:       feed.Id,
			Name:     r.FormValue("name"),
			Url:      r.FormValue("url"),
			Category: category,
			Language: lang}
		c.UpdateId(feed.Id, feed)
	} else {
		fmt.Println("insert")
		feed = &Feed{Id: bson.NewObjectId()}
		feed := &Feed{
			Id:       bson.ObjectIdHex(feedId),
			Name:     r.FormValue("name"),
			Url:      r.FormValue("url"),
			Category: category,
			Language: lang}

		c.Insert(&feed)
	}
	http.Redirect(w, r, "/view/", http.StatusFound)
}

func renderTemplate(w http.ResponseWriter, tmpl string, f *Feed) {
	err := templates.ExecuteTemplate(w, tmpl+".html", f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func renderViewTemplate(w http.ResponseWriter, tmpl string, f *[]Feed) {
	err := templates.ExecuteTemplate(w, tmpl+".html", f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func doEvery(d time.Duration, feeds func(*mgo.Session) []Feed, session *mgo.Session) {
	for _ = range time.Tick(d) {
		feedList := feeds(session)
		getNews(feedList, session)
	}
}

func getNews(feeds []Feed, session *mgo.Session) {
	for i := range feeds {
		items, er := rss.Fetch(feeds[i].Url)
		if er != nil {
			fmt.Println(er)
		} else {
			fmt.Println("feed " + feeds[i].Name + " " + feeds[i].Category.Name)
			c := session.DB("uutispuro").C("rss")
			for k, item := range items.Items {
				if k > 4 {
					break
				}

				item.Language = feeds[i].Language
				item.Category = feeds[i].Category
				item.Source = feeds[i].Name
				item.Language = feeds[i].Language
				if item.Date.After(time.Now()) {
					item.Date = time.Now()
				}
				result := rss.Item{}
				if len(item.ID) != 0 {
					err := c.Find(bson.M{"id": item.ID}).Select(bson.M{"_id": 1, "date": 1, "clicks": 1}).One(&result)
					if err == nil && len(result.Id) != 0 {
						item.Id = result.Id
						item.Date = result.Date
						item.Clicks = result.Clicks
						err2 := c.UpdateId(result.Id, &item)
						if err2 != nil {
							log.Println("updating rss with id failed " + err2.Error())
						}
					} else if err != nil && len(item.ID) != 0 {
						item.Id = bson.NewObjectId()
						err3 := c.Insert(&item)
						if err3 != nil {
							log.Println("inserting rss failed " + err3.Error())
						}
					}
					fmt.Println("  " + item.Date.Format("2006-01-02 15:04:05 -0700") + " " + item.Title)
				}
			}
			index := mgo.Index{
				Key:        []string{"id"},
				Unique:     true,
				DropDups:   true,
				Background: true,
				Sparse:     true,
			}
			c.EnsureIndex(index)

			indexFind := mgo.Index{
				Key:        []string{"language", "date"},
				Unique:     true,
				DropDups:   true,
				Background: true,
				Sparse:     true,
			}
			c.EnsureIndex(indexFind)
		}
	}
}

func getFeeds(session *mgo.Session) []Feed {
	c := session.DB("uutispuro").C("feed")
	feedList := []Feed{}
	_ = c.Find(bson.M{}).All(&feedList)
	return feedList
}

type Feed struct {
	Id       bson.ObjectId `json:"id" bson:"_id"`
	Name     string
	Url      string
	Category rss.Category
	Language int
}
