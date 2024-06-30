#!bin/bash
go build && killall rssfetcher
sleep 1
go build rssfetcher.go && 
nohup ./rssfetcher -env prod -address "mongodb://$MONGO_USER:$MONGO_PASSWORD@192.168.0.1:27017/news" > rssFetcher.log 2>&1&
ps aux|grep rssfetcher
