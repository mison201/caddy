// Package proxy is middleware that proxies HTTP requests.
package proxy

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mholt/caddy/caddyhttp/httpserver"
)

// Proxy represents a middleware instance that can proxy requests.
type Proxy struct {
	Next      httpserver.Handler
	Upstreams []Upstream
}

// Upstream manages a pool of proxy upstream hosts.
type Upstream interface {
	// The path this upstream host should be routed on
	From() string

	// Selects an upstream host to be routed to. It
	// should return a suitable upstream host, or nil
	// if no such hosts are available.
	Select(*http.Request) *UpstreamHost

	// Checks if subpath is not an ignored path
	AllowedPath(string) bool

	// Gets how long to try selecting upstream hosts
	// in the case of cascading failures.
	GetTryDuration() time.Duration

	// Gets how long to wait between selecting upstream
	// hosts in the case of cascading failures.
	GetTryInterval() time.Duration
}

// UpstreamHostDownFunc can be used to customize how Down behaves.
type UpstreamHostDownFunc func(*UpstreamHost) bool

// UpstreamHost represents a single proxy upstream
type UpstreamHost struct {
	Conns             int64 // must be first field to be 64-bit aligned on 32-bit systems
	MaxConns          int64
	Name              string // hostname of this upstream host
	UpstreamHeaders   http.Header
	DownstreamHeaders http.Header
	FailTimeout       time.Duration
	CheckDown         UpstreamHostDownFunc
	WithoutPathPrefix string
	ReverseProxy      *ReverseProxy
	Fails             int32
	Unhealthy         bool
}

// Down checks whether the upstream host is down or not.
// Down will try to use uh.CheckDown first, and will fall
// back to some default criteria if necessary.
func (uh *UpstreamHost) Down() bool {
	if uh.CheckDown == nil {
		// Default settings
		return uh.Unhealthy || uh.Fails > 0
	}
	return uh.CheckDown(uh)
}

// Full checks whether the upstream host has reached its maximum connections
func (uh *UpstreamHost) Full() bool {
	return uh.MaxConns > 0 && uh.Conns >= uh.MaxConns
}

// Available checks whether the upstream host is available for proxying to
func (uh *UpstreamHost) Available() bool {
	return !uh.Down() && !uh.Full()
}

// ServeHTTP satisfies the httpserver.Handler interface.
func (p Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) (int, error) {
	// start by selecting most specific matching upstream config
	upstream := p.match(r)
	if upstream == nil {
		return p.Next.ServeHTTP(w, r)
	}

	// this replacer is used to fill in header field values
	replacer := httpserver.NewReplacer(r, nil, "")

	// outreq is the request that makes a roundtrip to the backend
	outreq := createUpstreamRequest(r)

	// record and replace outreq body
	body, err := newBufferedBody(outreq.Body)
	if err != nil {
		return http.StatusBadRequest, errors.New("failed to read downstream request body")
	}
	if body != nil {
		outreq.Body = body
	}

	// The keepRetrying function will return true if we should
	// loop and try to select another host, or false if we
	// should break and stop retrying.
	start := time.Now()
	keepRetrying := func() bool {
		// if we've tried long enough, break
		if time.Since(start) >= upstream.GetTryDuration() {
			return false
		}
		// otherwise, wait and try the next available host
		time.Sleep(upstream.GetTryInterval())
		return true
	}

	var backendErr error
	for {
		// since Select() should give us "up" hosts, keep retrying
		// hosts until timeout (or until we get a nil host).
		host := upstream.Select(r)
		if host == nil {
			if backendErr == nil {
				backendErr = errors.New("no hosts available upstream")
			}
			if !keepRetrying() {
				break
			}
			continue
		}
		if rr, ok := w.(*httpserver.ResponseRecorder); ok && rr.Replacer != nil {
			rr.Replacer.Set("upstream", host.Name)
		}

		proxy := host.ReverseProxy

		// a backend's name may contain more than just the host,
		// so we parse it as a URL to try to isolate the host.
		if nameURL, err := url.Parse(host.Name); err == nil {
			outreq.Host = nameURL.Host
			if proxy == nil {
				proxy = NewSingleHostReverseProxy(nameURL, host.WithoutPathPrefix, http.DefaultMaxIdleConnsPerHost)
			}

			// use upstream credentials by default
			if outreq.Header.Get("Authorization") == "" && nameURL.User != nil {
				pwd, _ := nameURL.User.Password()
				outreq.SetBasicAuth(nameURL.User.Username(), pwd)
			}
		} else {
			outreq.Host = host.Name
		}
		if proxy == nil {
			return http.StatusInternalServerError, errors.New("proxy for host '" + host.Name + "' is nil")
		}

		// set headers for request going upstream
		if host.UpstreamHeaders != nil {
			// modify headers for request that will be sent to the upstream host
			mutateHeadersByRules(outreq.Header, host.UpstreamHeaders, replacer)
			if hostHeaders, ok := outreq.Header["Host"]; ok && len(hostHeaders) > 0 {
				outreq.Host = hostHeaders[len(hostHeaders)-1]
			}
		}

		// prepare a function that will update response
		// headers coming back downstream
		var downHeaderUpdateFn respUpdateFn
		if host.DownstreamHeaders != nil {
			downHeaderUpdateFn = createRespHeaderUpdateFn(host.DownstreamHeaders, replacer)
		}

		// rewind request body to its beginning
		if err := body.rewind(); err != nil {
			return http.StatusInternalServerError, errors.New("unable to rewind downstream request body")
		}

		// tell the proxy to serve the request
		atomic.AddInt64(&host.Conns, 1)
		backendErr = proxy.ServeHTTP(w, outreq, downHeaderUpdateFn)
		atomic.AddInt64(&host.Conns, -1)

		// if no errors, we're done here
		if backendErr == nil {
			return 0, nil
		}

		if _, ok := backendErr.(httpserver.MaxBytesExceeded); ok {
			return http.StatusRequestEntityTooLarge, backendErr
		}

		// failover; remember this failure for some time if
		// request failure counting is enabled
		timeout := host.FailTimeout
		if timeout > 0 {
			atomic.AddInt32(&host.Fails, 1)
			go func(host *UpstreamHost, timeout time.Duration) {
				time.Sleep(timeout)
				atomic.AddInt32(&host.Fails, -1)
			}(host, timeout)
		}

		// if we've tried long enough, break
		if !keepRetrying() {
			break
		}
	}

	return http.StatusBadGateway, backendErr
}

// match finds the best match for a proxy config based on r.
func (p Proxy) match(r *http.Request) Upstream {
	var u Upstream
	var longestMatch int
	for _, upstream := range p.Upstreams {
		basePath := upstream.From()
		if !httpserver.Path(r.URL.Path).Matches(basePath) || !upstream.AllowedPath(r.URL.Path) {
			continue
		}
		if len(basePath) > longestMatch {
			longestMatch = len(basePath)
			u = upstream
		}
	}
	return u
}

// createUpstremRequest shallow-copies r into a new request
// that can be sent upstream.
//
// Derived from reverseproxy.go in the standard Go httputil package.
func createUpstreamRequest(r *http.Request) *http.Request {
	outreq := new(http.Request)
	*outreq = *r // includes shallow copies of maps, but okay
	// We should set body to nil explicitly if request body is empty.
	// For server requests the Request Body is always non-nil.
	if r.ContentLength == 0 {
		outreq.Body = nil
	}

	// Restore URL Path if it has been modified
	if outreq.URL.RawPath != "" {
		outreq.URL.Opaque = outreq.URL.RawPath
	}

	// Remove hop-by-hop headers to the backend. Especially
	// important is "Connection" because we want a persistent
	// connection, regardless of what the client sent to us. This
	// is modifying the same underlying map from r (shallow
	// copied above) so we only copy it if necessary.
	var copiedHeaders bool
	for _, h := range hopHeaders {
		if outreq.Header.Get(h) != "" {
			if !copiedHeaders {
				outreq.Header = make(http.Header)
				copyHeader(outreq.Header, r.Header)
				copiedHeaders = true
			}
			outreq.Header.Del(h)
		}
	}

	if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		// If we aren't the first proxy, retain prior
		// X-Forwarded-For information as a comma+space
		// separated list and fold multiple headers into one.
		if prior, ok := outreq.Header["X-Forwarded-For"]; ok {
			clientIP = strings.Join(prior, ", ") + ", " + clientIP
		}
		outreq.Header.Set("X-Forwarded-For", clientIP)
	}

	return outreq
}

func createRespHeaderUpdateFn(rules http.Header, replacer httpserver.Replacer) respUpdateFn {
	return func(resp *http.Response) {
		mutateHeadersByRules(resp.Header, rules, replacer)
	}
}

func mutateHeadersByRules(headers, rules http.Header, repl httpserver.Replacer) {
	for ruleField, ruleValues := range rules {
		if strings.HasPrefix(ruleField, "+") {
			for _, ruleValue := range ruleValues {
				headers.Add(strings.TrimPrefix(ruleField, "+"), repl.Replace(ruleValue))
			}
		} else if strings.HasPrefix(ruleField, "-") {
			headers.Del(strings.TrimPrefix(ruleField, "-"))
		} else if len(ruleValues) > 0 {
			headers.Set(ruleField, repl.Replace(ruleValues[len(ruleValues)-1]))
		}
	}
}
