// Package platformhttp owns bounded connection reuse for platform API clients.
// It never retries requests or changes authentication, redirect or job policy.
package platformhttp

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const (
	MaxConnectionAge = 10 * time.Minute
	IdleConnTimeout  = 90 * time.Second
	FailureThreshold = 3
	maxErrorSample   = 4096
)

type Options struct {
	BaseTransport    *http.Transport
	MaxAge           time.Duration
	IdleTimeout      time.Duration
	FailureThreshold int
	Logf             func(string, ...any)
	Now              func() time.Time
}

type generation struct {
	pool   *http.Transport
	born   time.Time
	failed int
	remote string
	reason string
}

// Transport rotates the whole pool before a request once its age is reached.
// Unlike idle eviction alone, this also stops new HTTP/2 streams from selecting
// an old, continuously busy connection. Existing requests are allowed to finish.
type Transport struct {
	mu          sync.Mutex
	current     *generation
	passthrough http.RoundTripper
	template    *http.Transport
	origin      string
	maxAge      time.Duration
	threshold   int
	logf        func(string, ...any)
	now         func() time.Time
}

// WithTransport preserves client policy in a private value copy. An explicit
// non-standard RoundTripper is left intact (test fixtures or caller custody).
func WithTransport(baseURL string, client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	copy := *client
	switch base := client.Transport.(type) {
	case nil:
		copy.Transport = New(baseURL, Options{})
	case *http.Transport:
		copy.Transport = New(baseURL, Options{BaseTransport: base})
	case *Transport:
		if base.template != nil && base.origin != configuredOrigin(baseURL) {
			copy.Transport = New(baseURL, Options{BaseTransport: base.template, MaxAge: base.maxAge, FailureThreshold: base.threshold, Logf: base.logf, Now: base.now})
		}
	}
	return &copy
}

// New builds a private pool without changing HTTP client policy. Opaque caller
// transports remain owned by that caller and cannot be aged by this helper.
func New(baseURL string, opts Options) *Transport {
	base := opts.BaseTransport
	if base == nil {
		var standard bool
		base, standard = http.DefaultTransport.(*http.Transport)
		if !standard {
			// A caller-installed opaque default is an explicit policy boundary,
			// not permission to substitute an unrestricted network transport.
			return &Transport{origin: configuredOrigin(baseURL), passthrough: http.DefaultTransport}
		}
	}
	template := base.Clone()
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = IdleConnTimeout
	}
	if template.IdleConnTimeout > 0 && template.IdleConnTimeout < opts.IdleTimeout {
		opts.IdleTimeout = template.IdleConnTimeout
	}
	template.IdleConnTimeout = opts.IdleTimeout
	if opts.MaxAge <= 0 {
		opts.MaxAge = MaxConnectionAge
	}
	if opts.FailureThreshold <= 0 {
		opts.FailureThreshold = FailureThreshold
	}
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	t := &Transport{template: template, origin: configuredOrigin(baseURL), maxAge: opts.MaxAge, threshold: opts.FailureThreshold, logf: opts.Logf, now: opts.Now}
	t.current = t.fresh("initial")
	return t
}

func configuredOrigin(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u == nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func (t *Transport) fresh(reason string) *generation {
	return &generation{pool: t.template.Clone(), born: t.now(), reason: reason}
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.passthrough != nil {
		return t.passthrough.RoundTrip(req)
	}
	t.mu.Lock()
	g := t.current
	var retired *generation
	retiredIP := "unknown"
	if t.now().Sub(g.born) >= t.maxAge {
		retired = g
		retiredIP = g.remoteIP()
		t.current = t.fresh("max_connection_age")
		g = t.current
	}
	t.mu.Unlock()
	if retired != nil {
		retired.pool.CloseIdleConnections()
		t.redial("max_connection_age", retiredIP)
	}
	tracked := req.URL.Scheme+"://"+req.URL.Host == t.origin

	// The trace composes with an existing caller trace. Keep only an IP, never
	// a URL/query, device token, response body or peer-supplied error message.
	var peerMu sync.Mutex
	peer := "unknown"
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		ip := RemoteIP(info.Conn.RemoteAddr())
		peerMu.Lock()
		peer = ip
		peerMu.Unlock()
		if tracked {
			t.mu.Lock()
			g.remote = ip
			t.mu.Unlock()
		}
		if tracked && !info.Reused {
			t.logf("platform API fresh dial remote_ip=%s reason=%s", ip, g.reason)
		}
	}}
	request := req.Clone(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := g.pool.RoundTrip(request)
	peerMu.Lock()
	ip := peer
	peerMu.Unlock()
	if err != nil {
		if tracked && req.Context().Err() == nil {
			t.observe(g, true, "network_failure", ip)
		}
		return resp, err
	}
	if !tracked {
		return resp, nil // Redirects/secondary hosts retain caller policy.
	}
	failed := resp.StatusCode == http.StatusNotFound || (resp.StatusCode >= 500 && resp.StatusCode < 600)
	rotated := false
	if failed {
		rotated = t.observe(g, true, "http_"+strconv.Itoa(resp.StatusCode), ip)
	} else if resp.StatusCode < 400 || resp.StatusCode == 401 || resp.StatusCode == 403 {
		t.observe(g, false, "", ip)
	}
	// Authorization-refusal bodies are deliberately never read or retained by
	// this layer. Other error bodies are sampled only while the caller reads;
	// there is no prefetch, additional read, content substitution or retry.
	if resp.StatusCode >= 400 && resp.StatusCode != 401 && resp.StatusCode != 403 && resp.Body != nil {
		resp.Body = &observedBody{ReadCloser: resp.Body, finish: func(sample []byte, complete bool) {
			if !complete && len(sample) == 0 {
				return
			}
			mismatch := !platformError(sample)
			reason := "upstream mismatch"
			if !complete && mismatch && !json.Valid(sample) {
				reason = "unclassified_error_body"
			}
			if failed {
				if mismatch && rotated {
					t.redial(reason, ip)
				}
				return // A response contributes at most one failure.
			}
			t.observe(g, mismatch, reason, ip)
		}}
	}
	return resp, nil
}

// observe ignores late responses from retired pools, so one old in-flight
// request cannot reset or rotate the replacement generation's failure streak.
func (t *Transport) observe(g *generation, failed bool, reason, remote string) bool {
	t.mu.Lock()
	if t.current != g {
		t.mu.Unlock()
		return false
	}
	if !failed {
		g.failed = 0
		t.mu.Unlock()
		return false
	}
	g.failed++
	if g.failed < t.threshold {
		t.mu.Unlock()
		return false
	}
	t.current = t.fresh(reason)
	t.mu.Unlock()
	g.pool.CloseIdleConnections()
	t.redial(reason, remote)
	return true
}

func (g *generation) remoteIP() string {
	if g.remote == "" {
		return "unknown"
	}
	return g.remote
}

func (t *Transport) redial(reason, remote string) {
	t.logf("platform API %s: re-dialing remote_ip=%s", reason, remote)
}

// LogRedial is shared with upgraded WebSocket lifecycle rotation, which cannot
// use an HTTP RoundTripper after its handshake.
func LogRedial(reason string, remote net.Addr) {
	log.Printf("platform API %s: re-dialing remote_ip=%s", reason, RemoteIP(remote))
}

func (t *Transport) CloseIdleConnections() {
	if t.passthrough != nil {
		if closer, ok := t.passthrough.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
		return
	}
	t.mu.Lock()
	pool := t.current.pool
	t.mu.Unlock()
	pool.CloseIdleConnections()
}

// RemoteIP reports the actual connected TCP peer, including a configured proxy
// if present. Unparseable addresses never become untrusted diagnostic text.
func RemoteIP(addr net.Addr) string {
	if addr == nil {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "unknown"
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "unknown"
	}
	return ip.String()
}

func platformError(body []byte) bool {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return false
	}
	var message string
	if json.Unmarshal(envelope.Error, &message) == nil {
		return message != ""
	}
	var rpc struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
	}
	return json.Unmarshal(envelope.Error, &rpc) == nil && rpc.Code != nil && rpc.Message != ""
}

type observedBody struct {
	io.ReadCloser
	mu       sync.Mutex
	sample   []byte
	complete bool
	finished bool
	finish   func([]byte, bool)
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	if !b.finished && len(b.sample) <= maxErrorSample {
		remaining := maxErrorSample + 1 - len(b.sample)
		b.sample = append(b.sample, p[:min(n, remaining)]...)
	}
	if err == io.EOF {
		b.complete = len(b.sample) <= maxErrorSample
	}
	b.mu.Unlock()
	if err == io.EOF {
		b.done()
	}
	return n, err
}

func (b *observedBody) Close() error {
	err := b.ReadCloser.Close()
	b.done()
	return err
}

func (b *observedBody) done() {
	b.mu.Lock()
	if b.finished {
		b.mu.Unlock()
		return
	}
	b.finished = true
	sample, complete := b.sample, b.complete
	b.sample = nil
	b.mu.Unlock()
	b.finish(sample, complete)
}
