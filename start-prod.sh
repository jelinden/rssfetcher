#!bin/bash
git pull
go build
killall rssfetcher
sleep 1
nohup ./rssfetcher -env prod -address localhost:28517 > rssFetcher.log 2>&1&
ps aux|grep rssfetcher
