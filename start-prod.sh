#!bin/bash
git pull
go build
killall rssFetcher
sleep 1
nohup ./rssFetcher -env prod -address localhost:28517 > rssFetcher.log 2>&1&
ps aux|grep rssFetcher
