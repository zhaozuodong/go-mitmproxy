package main

import (
	"fmt"
	"github.com/lqqyt2423/go-mitmproxy/proxy"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"strings"
)

var (
	urls = []string{
		"/api/sns/v6/homefeed",
		"/api/sns/v3/user/info",
		"/api/sns/v3/user/info",
		"/api/sns/v1/user/followers",
		"/api/sns/v1/user/followings",
		"/api/sns/v1/note/faved",
		"/api/sns/v4/note/user/posted",
		"/api/sns/v4/note/user/posted",
		"/api/sns/v2/note/feed",
		"/api/sns/v3/note/videofeed",
		"/api/sns/v2/note/widgets",
		"/api/sns/v5/note/comment/list",
		"/api/sns/v10/search/notes",
	}
)

type ListeningRequest struct {
	proxy.BaseAddon
}

func (c *ListeningRequest) Response(f *proxy.Flow) {
	contentType := f.Response.Header.Get("Content-Type")
	if !strings.Contains(contentType, "json") {
		return
	}

	for _, url := range urls {
		if strings.Contains(f.Request.URL.String(), url) {
			fmt.Println(f.Request.URL.String())
			if !gjson.ValidBytes(f.Response.Body) {
				log.Info("json is err")
				continue
			}
			data := gjson.ParseBytes(f.Response.Body)
			fmt.Println(data.String())
		}
	}
}

func main() {
	opts := &proxy.Options{
		Addr:              ":9080",
		StreamLargeBodies: 1024 * 1024 * 1000,
	}

	p, err := proxy.NewProxy(opts)
	if err != nil {
		log.Fatal(err)
	}

	//p.SetUpstreamProxy(func(req *http.Request) (*url.URL, error) {
	//	return url.Parse("socks://127.0.0.1:8889")
	//})

	p.AddAddon(&ListeningRequest{})

	log.Fatal(p.Start())
}
