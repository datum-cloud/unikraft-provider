package activator

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// proxyRequest reverse-proxies r to target, writing the response to w.
// target must be an absolute URL, e.g. "http://host:port".
func proxyRequest(w http.ResponseWriter, r *http.Request, target string) {
	u, err := url.Parse(target)
	if err != nil {
		http.Error(w, "invalid upstream target", http.StatusInternalServerError)
		return
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = u.Scheme
			req.URL.Host = u.Host
			req.Host = u.Host

			// Remove activator-injected function identity headers before forwarding.
			req.Header.Del("X-Datum-Function-Name")
			req.Header.Del("X-Datum-Function-Namespace")

			// Standard hop-by-hop cleanup is handled by httputil.ReverseProxy
			// automatically; we only need to manage our own headers here.
		},
		Transport: &http.Transport{
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}

	proxy.ServeHTTP(w, r)
}
