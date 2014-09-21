package main

import (
	"fmt"
	"github.com/jelinden/rssFetcher/rss"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
	"log"
	"time"
)

func main() {
	session, err := mgo.Dial("localhost")
	if err != nil {
		panic(err)
	}
	defer session.Close()
	session.SetMode(mgo.Monotonic, true)

	doEvery(2*time.Minute, getFeeds, session)
}

func doEvery(d time.Duration, feeds func() []Feed, session *mgo.Session) {
	for _ = range time.Tick(d) {
		getNews(feeds(), session)
	}
}

func getNews(feeds []Feed, session *mgo.Session) {
	for i := range feeds {
		items, err := rss.Fetch(feeds[i].Url)
		if err != nil {
			fmt.Println(err)
		} else {
			fmt.Println("feed " + feeds[i].Name + " " + feeds[i].Category.Name)
			c := session.DB("uutispuro").C("feed")
			for k, item := range items.Items {
				if k > 2 {
					break
				}

				item.Language = feeds[i].Language
				item.Category = feeds[i].Category
				item.Source = feeds[i].Name
				item.Language = feeds[i].Language

				result := rss.Item{}
				err := c.Find(bson.M{"id": item.ID}).Select(bson.M{"id": 1}).One(&result)
				if err != nil && len(result.ID) != 0 {
					err = c.UpdateId(bson.M{"id": result.ID}, &item)
					if err != nil {
						log.Fatal(err)
					}
				} else if err != nil && len(item.ID) != 0 {
					err = c.Insert(&item)
					if err != nil {
						log.Fatal(err)
					}
				}
				fmt.Println("  " + item.Date.Format("2006-01-02 15:04:05 -0700") + " " + item.Title)
			}
			index := mgo.Index{
				Key:        []string{"id"},
				Unique:     true,
				DropDups:   true,
				Background: true,
				Sparse:     true,
			}
			c.EnsureIndex(index)
		}
	}
}

func getFeeds() []Feed {
	return []Feed{
		Feed{"Kauppalehti", "http://rss.kauppalehti.fi/rss/etusivun_uutiset.jsp", rss.Category{"Talous"}, 1},
		Feed{"Digitoday", "http://www.digitoday.fi/feeds/Digitoday-Bisnes.xml", rss.Category{"IT ja digi"}, 1},
		Feed{"The Independent", "http://rss.feedsportal.com/c/266/f/3511/index.rss", rss.Category{"Talous"}, 2},
		Feed{"MailOnline", "http://www.dailymail.co.uk/money/index.rss", rss.Category{"Talous"}, 2},
		Feed{"MTV", "http://www.mtv.fi/api/feed/rss/urheilu_f1", rss.Category{"Urheilu"}, 1},
		Feed{"MTV", "http://www.mtv.fi/api/feed/rss/viihde_uusimmat_100", rss.Category{"Viihde"}, 1},
		Feed{"Iltalehti", "http://www.iltalehti.fi/rss/digi.xml", rss.Category{"IT ja media"}, 1},
		Feed{"Iltasanomat", "http://www.iltasanomat.fi/rss/kotimaa.xml", rss.Category{"Kotimaa"}, 1},
		Feed{"BBC", "http://feeds.bbci.co.uk/news/health/rss.xml", rss.Category{"Terveys"}, 2},
		Feed{"Yle", "http://yle.fi/uutiset/rss/uutiset.rss?osasto=talous", rss.Category{"Talous"}, 1},
	}
}

type Feed struct {
	Name     string
	Url      string
	Category rss.Category
	Language int
}
