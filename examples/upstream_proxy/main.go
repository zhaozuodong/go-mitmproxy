package main

import (
	"github.com/lqqyt2423/go-mitmproxy/proxy"
	log "github.com/sirupsen/logrus"
	"net/http"
	"net/url"
	"strings"
)

type ListeningRequest struct {
	proxy.BaseAddon
}

func (c *ListeningRequest) Response(f *proxy.Flow) {
	contentType := f.Response.Header.Get("Content-Type")
	if !strings.Contains(contentType, "json") {
		return
	}
	log.Info(f.Request.URL.String())
	body, _ := f.Response.DecodedBody()
	log.Info(string(body))
}

func main() {
	opts := &proxy.Options{
		HttpAddr:          ":9080",
		SocksAddr:         ":9089",
		StreamLargeBodies: 1024 * 1024 * 5,
	}

	p, err := proxy.NewProxy(opts)
	if err != nil {
		log.Fatal(err)
	}

	p.SetUpstreamProxy(func(req *http.Request) (*url.URL, error) {
		return url.Parse("socks://127.0.0.1:8889")
	})

	p.AddAddon(&ListeningRequest{})

	log.Fatal(p.Start())
}
