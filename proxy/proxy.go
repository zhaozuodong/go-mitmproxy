package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"github.com/armon/go-socks5"
	"github.com/haxii/fastproxy/bufiopool"
	"github.com/haxii/fastproxy/superproxy"
	log "github.com/sirupsen/logrus"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
)

type Options struct {
	Debug             int
	HttpAddr          string
	SocksAddr         string
	StreamLargeBodies int64 // 当请求或响应体大于此字节时，转为 stream 模式
	SslInsecure       bool
	CaRootPath        string
	Upstream          string
}

type Proxy struct {
	Opts    *Options
	Version string
	Addons  []Addon

	client          *http.Client
	server          *http.Server
	interceptor     *middle
	shouldIntercept func(req *http.Request) bool              // req is received by proxy.server
	upstreamProxy   func(req *http.Request) (*url.URL, error) // req is received by proxy.server, not client request

	socks5proxy  *socks5.Server
	socks5tunnel *superproxy.SuperProxy
	bufioPool    *bufiopool.Pool
}

// proxy.server req context key
var proxyReqCtxKey = new(struct{})

func NewProxy(opts *Options) (*Proxy, error) {
	if opts.StreamLargeBodies <= 0 {
		opts.StreamLargeBodies = 1024 * 1024 * 5 // default: 5mb
	}

	proxy := &Proxy{
		Opts:    opts,
		Version: "1.7.1",
		Addons:  make([]Addon, 0),
	}

	proxy.client = &http.Client{
		Transport: &http.Transport{
			Proxy:              proxy.realUpstreamProxy(),
			ForceAttemptHTTP2:  false, // disable http2
			DisableCompression: true,  // To get the original response from the server, set Transport.DisableCompression to true.
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: opts.SslInsecure,
				KeyLogWriter:       getTlsKeyLogWriter(),
			},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// 禁止自动重定向
			return http.ErrUseLastResponse
		},
	}

	proxy.server = &http.Server{
		Addr:    opts.HttpAddr,
		Handler: proxy,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			connCtx := newConnContext(c, proxy)
			for _, addon := range proxy.Addons {
				addon.ClientConnected(connCtx.ClientConn)
			}
			c.(*wrapClientConn).connCtx = connCtx
			return context.WithValue(ctx, connContextKey, connCtx)
		},
	}

	interceptor, err := newMiddle(proxy)
	if err != nil {
		return nil, err
	}
	proxy.interceptor = interceptor

	return proxy, nil
}

func (proxy *Proxy) AddAddon(addon Addon) {
	proxy.Addons = append(proxy.Addons, addon)
}

func (proxy *Proxy) Start() error {
	addr := proxy.server.Addr
	if addr == "" {
		addr = ":http"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go proxy.startSocksProxy()
	go proxy.interceptor.start()
	log.Infof("http proxy start listen at %v\n", proxy.server.Addr)

	pln := &wrapListener{
		Listener: ln,
		proxy:    proxy,
	}
	return proxy.server.Serve(pln)
}

func (proxy *Proxy) startSocksProxy() {
	if proxy.Opts.SocksAddr != "" {
		proxyHost, proxyPort, err := net.SplitHostPort(proxy.Opts.HttpAddr)
		if err != nil {
			log.Errorf("parse proxy addr err:  %v\n", err.Error())
			return
		}
		if proxyHost == "" {
			proxyHost = "0.0.0.0"
		}
		port, _ := strconv.ParseInt(proxyPort, 10, 64)
		proxy.socks5tunnel, _ = superproxy.NewSuperProxy(proxyHost, uint16(port), superproxy.ProxyTypeHTTP, "", "", "")
		proxy.bufioPool = bufiopool.New(4096, 4096)
		socks5Config := &socks5.Config{
			Dial: proxy.httpTunnelDialer,
		}
		socks5proxy, err := socks5.New(socks5Config)
		if err != nil {
			log.Errorf("socks5 proxy start err:  %v\n", err.Error())
			return
		}
		log.Infof("socks5 proxy start listen at %v\n", proxy.Opts.SocksAddr)
		proxy.socks5proxy = socks5proxy
		proxy.socks5proxy.ListenAndServe("tcp", proxy.Opts.SocksAddr)
	}
}

func (proxy *Proxy) httpTunnelDialer(ctx context.Context, network, addr string) (net.Conn, error) {
	return proxy.socks5tunnel.MakeTunnel(nil, nil, proxy.bufioPool, addr)
}

func (proxy *Proxy) Close() error {
	err := proxy.server.Close()
	proxy.interceptor.close()
	return err
}

func (proxy *Proxy) Shutdown(ctx context.Context) error {
	err := proxy.server.Shutdown(ctx)
	proxy.interceptor.close()
	return err
}

func (proxy *Proxy) ServeHTTP(res http.ResponseWriter, req *http.Request) {
	if req.Method == "CONNECT" {
		proxy.handleConnect(res, req)
		return
	}

	log := log.WithFields(log.Fields{
		"in":     "Proxy.ServeHTTP",
		"url":    req.URL,
		"method": req.Method,
	})

	if !req.URL.IsAbs() || req.URL.Host == "" {
		if len(proxy.Addons) == 0 {
			res.WriteHeader(400)
			io.WriteString(res, "此为代理服务器，不能直接发起请求")
			return
		}
		for _, addon := range proxy.Addons {
			addon.AccessProxyServer(req, res)
		}
		return
	}

	reply := func(response *Response, body io.Reader) {
		if response.Header != nil {
			for key, value := range response.Header {
				for _, v := range value {
					res.Header().Add(key, v)
				}
			}
		}
		if response.close {
			res.Header().Add("Connection", "close")
		}
		res.WriteHeader(response.StatusCode)

		if body != nil {
			_, err := io.Copy(res, body)
			if err != nil {
				logErr(log, err)
			}
		}
		if response.BodyReader != nil {
			_, err := io.Copy(res, response.BodyReader)
			if err != nil {
				logErr(log, err)
			}
		}
		if response.Body != nil && len(response.Body) > 0 {
			_, err := res.Write(response.Body)
			if err != nil {
				logErr(log, err)
			}
		}
	}

	// when addons panic
	defer func() {
		if err := recover(); err != nil {
			log.Warnf("Recovered: %v\n", err)
		}
	}()

	f := newFlow()
	f.Request = newRequest(req)
	f.ConnContext = req.Context().Value(connContextKey).(*ConnContext)
	defer f.finish()

	f.ConnContext.FlowCount = f.ConnContext.FlowCount + 1

	rawReqUrlHost := f.Request.URL.Host
	rawReqUrlScheme := f.Request.URL.Scheme

	// trigger addon event Requestheaders
	for _, addon := range proxy.Addons {
		addon.Requestheaders(f)
		if f.Response != nil {
			reply(f.Response, nil)
			return
		}
	}

	// Read request body
	var reqBody io.Reader = req.Body
	if !f.Stream {
		reqBuf, r, err := readerToBuffer(req.Body, proxy.Opts.StreamLargeBodies)
		reqBody = r
		if err != nil {
			log.Error(err)
			res.WriteHeader(502)
			return
		}

		if reqBuf == nil {
			log.Warnf("request body size >= %v\n", proxy.Opts.StreamLargeBodies)
			f.Stream = true
		} else {
			f.Request.Body = reqBuf

			// trigger addon event Request
			for _, addon := range proxy.Addons {
				addon.Request(f)
				if f.Response != nil {
					reply(f.Response, nil)
					return
				}
			}
			reqBody = bytes.NewReader(f.Request.Body)
		}
	}

	for _, addon := range proxy.Addons {
		reqBody = addon.StreamRequestModifier(f, reqBody)
	}

	proxyReqCtx := context.WithValue(context.Background(), proxyReqCtxKey, req)
	proxyReq, err := http.NewRequestWithContext(proxyReqCtx, f.Request.Method, f.Request.URL.String(), reqBody)
	if err != nil {
		log.Error(err)
		res.WriteHeader(502)
		return
	}

	for key, value := range f.Request.Header {
		for _, v := range value {
			proxyReq.Header.Add(key, v)
		}
	}

	f.ConnContext.initHttpServerConn()

	useSeparateClient := f.UseSeparateClient
	if !useSeparateClient {
		if rawReqUrlHost != f.Request.URL.Host || rawReqUrlScheme != f.Request.URL.Scheme {
			useSeparateClient = true
		}
	}

	var proxyRes *http.Response
	if useSeparateClient {
		proxyRes, err = proxy.client.Do(proxyReq)
	} else {
		proxyRes, err = f.ConnContext.ServerConn.client.Do(proxyReq)
	}
	if err != nil {
		logErr(log, err)
		res.WriteHeader(502)
		return
	}

	if proxyRes.Close {
		f.ConnContext.closeAfterResponse = true
	}

	defer proxyRes.Body.Close()

	f.Response = &Response{
		StatusCode: proxyRes.StatusCode,
		Header:     proxyRes.Header,
		close:      proxyRes.Close,
	}

	// trigger addon event Responseheaders
	for _, addon := range proxy.Addons {
		addon.Responseheaders(f)
		if f.Response.Body != nil {
			reply(f.Response, nil)
			return
		}
	}

	// Read response body
	var resBody io.Reader = proxyRes.Body
	if !f.Stream {
		resBuf, r, err := readerToBuffer(proxyRes.Body, proxy.Opts.StreamLargeBodies)
		resBody = r
		if err != nil {
			log.Error(err)
			res.WriteHeader(502)
			return
		}
		if resBuf == nil {
			log.Warnf("response body size >= %v\n", proxy.Opts.StreamLargeBodies)
			f.Stream = true
		} else {
			f.Response.Body = resBuf

			// trigger addon event Response
			for _, addon := range proxy.Addons {
				addon.Response(f)
			}
		}
	}
	for _, addon := range proxy.Addons {
		resBody = addon.StreamResponseModifier(f, resBody)
	}

	reply(f.Response, resBody)
}

func (proxy *Proxy) handleConnect(res http.ResponseWriter, req *http.Request) {
	log := log.WithFields(log.Fields{
		"in":   "Proxy.handleConnect",
		"host": req.Host,
	})

	shouldIntercept := proxy.shouldIntercept == nil || proxy.shouldIntercept(req)
	f := newFlow()
	f.Request = newRequest(req)
	f.ConnContext = req.Context().Value(connContextKey).(*ConnContext)
	f.ConnContext.Intercept = shouldIntercept
	defer f.finish()

	// trigger addon event Requestheaders
	for _, addon := range proxy.Addons {
		addon.Requestheaders(f)
	}

	var conn net.Conn
	var err error
	if shouldIntercept {
		log.Debugf("begin intercept %v", req.Host)
		conn, err = proxy.interceptor.dial(req)
	} else {
		log.Debugf("begin transpond %v", req.Host)
		conn, err = proxy.getUpstreamConn(req)
	}
	if err != nil {
		log.Error(err)
		res.WriteHeader(502)
		return
	}
	defer conn.Close()

	cconn, _, err := res.(http.Hijacker).Hijack()
	if err != nil {
		log.Error(err)
		res.WriteHeader(502)
		return
	}

	// cconn.(*net.TCPConn).SetLinger(0) // send RST other than FIN when finished, to avoid TIME_WAIT state
	// cconn.(*net.TCPConn).SetKeepAlive(false)
	defer cconn.Close()

	_, err = io.WriteString(cconn, "HTTP/1.1 200 Connection Established\r\n\r\n")
	if err != nil {
		log.Error(err)
		return
	}

	f.Response = &Response{
		StatusCode: 200,
		Header:     make(http.Header),
	}

	// trigger addon event Responseheaders
	for _, addon := range proxy.Addons {
		addon.Responseheaders(f)
	}
	defer func(f *Flow) {
		// trigger addon event Response
		for _, addon := range proxy.Addons {
			addon.Response(f)
		}
	}(f)

	transfer(log, conn, cconn)
}

func (proxy *Proxy) GetCertificate() x509.Certificate {
	return proxy.interceptor.ca.RootCert
}

func (proxy *Proxy) SetShouldInterceptRule(rule func(req *http.Request) bool) {
	proxy.shouldIntercept = rule
}

func (proxy *Proxy) SetUpstreamProxy(fn func(req *http.Request) (*url.URL, error)) {
	proxy.upstreamProxy = fn
}

func (proxy *Proxy) realUpstreamProxy() func(*http.Request) (*url.URL, error) {
	return func(cReq *http.Request) (*url.URL, error) {
		req := cReq.Context().Value(proxyReqCtxKey).(*http.Request)
		return proxy.getUpstreamProxyUrl(req)
	}
}

func (proxy *Proxy) getUpstreamProxyUrl(req *http.Request) (*url.URL, error) {
	if proxy.upstreamProxy != nil {
		return proxy.upstreamProxy(req)
	}
	if len(proxy.Opts.Upstream) > 0 {
		return url.Parse(proxy.Opts.Upstream)
	}
	cReq := &http.Request{URL: &url.URL{Scheme: "https", Host: req.Host}}
	return http.ProxyFromEnvironment(cReq)
}

func (proxy *Proxy) getUpstreamConn(req *http.Request) (net.Conn, error) {
	proxyUrl, err := proxy.getUpstreamProxyUrl(req)
	if err != nil {
		return nil, err
	}
	var conn net.Conn
	if proxyUrl != nil {
		conn, err = getProxyConn(proxyUrl, req.Host)
	} else {
		conn, err = (&net.Dialer{}).DialContext(context.Background(), "tcp", req.Host)
	}
	return conn, err
}
