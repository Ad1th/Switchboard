// Package balancer implements a small HTTP load balancer: a pool of
// backends, two selection strategies, active health checks and passive
// failure detection with retries.
package balancer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

type Strategy string

const (
	RoundRobin Strategy = "roundrobin"
	LeastConn  Strategy = "leastconn"
)

type Backend struct {
	URL    *url.URL
	proxy  *httputil.ReverseProxy
	alive  atomic.Bool
	active atomic.Int64
	served atomic.Int64
	failed atomic.Int64
}

func (b *Backend) Alive() bool { return b.alive.Load() }

type BackendStatus struct {
	URL    string `json:"url"`
	Alive  bool   `json:"alive"`
	Active int64  `json:"active"`
	Served int64  `json:"served"`
	Failed int64  `json:"failed"`
}

type Pool struct {
	backends []*Backend
	strategy Strategy
	next     atomic.Uint64
	client   *http.Client
	logf     func(format string, args ...any)
}

type failureKey struct{}

// New builds a pool from backend URLs. Every backend starts out dead until
// the first health check (see CheckAll) says otherwise.
func New(rawURLs []string, strategy Strategy) (*Pool, error) {
	if len(rawURLs) == 0 {
		return nil, errors.New("at least one backend is required")
	}
	if strategy != RoundRobin && strategy != LeastConn {
		return nil, fmt.Errorf("unknown strategy %q (want %q or %q)", strategy, RoundRobin, LeastConn)
	}

	p := &Pool{
		strategy: strategy,
		client:   &http.Client{Timeout: 2 * time.Second},
		logf:     log.Printf,
	}
	for _, raw := range rawURLs {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("invalid backend URL %q", raw)
		}
		b := &Backend{URL: u}
		b.proxy = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(u) // also points Host at the backend
				pr.SetXForwarded()
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				// Hand the failure to ServeHTTP, which decides whether to retry.
				if f, ok := r.Context().Value(failureKey{}).(*error); ok {
					*f = err
				}
			},
		}
		p.backends = append(p.backends, b)
	}
	return p, nil
}

// pick returns the next healthy backend that is not in tried, or nil.
func (p *Pool) pick(tried map[*Backend]bool) *Backend {
	n := len(p.backends)
	switch p.strategy {
	case LeastConn:
		var best *Backend
		for _, b := range p.backends {
			if tried[b] || !b.Alive() {
				continue
			}
			if best == nil || b.active.Load() < best.active.Load() {
				best = b
			}
		}
		return best
	default:
		start := int(p.next.Add(1)-1) % n
		for i := 0; i < n; i++ {
			b := p.backends[(start+i)%n]
			if !tried[b] && b.Alive() {
				return b
			}
		}
		return nil
	}
}

func retryable(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return r.Body == nil || r.Body == http.NoBody
	}
	return false
}

func (p *Pool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tried := map[*Backend]bool{}
	for {
		b := p.pick(tried)
		if b == nil {
			if len(tried) > 0 {
				http.Error(w, "All backends failed", http.StatusBadGateway)
			} else {
				http.Error(w, "No healthy backends available", http.StatusServiceUnavailable)
			}
			return
		}
		tried[b] = true

		var proxyErr error
		ctx := context.WithValue(r.Context(), failureKey{}, &proxyErr)

		b.active.Add(1)
		b.proxy.ServeHTTP(w, r.WithContext(ctx))
		b.active.Add(-1)

		if proxyErr == nil {
			b.served.Add(1)
			return
		}
		if r.Context().Err() != nil {
			return // client went away, not the backend's fault
		}
		b.failed.Add(1)
		b.alive.Store(false)
		p.logf("backend %s failed (%v), marked dead", b.URL, proxyErr)
		if !retryable(r) {
			http.Error(w, "Bad gateway", http.StatusBadGateway)
			return
		}
	}
}

func (p *Pool) check(b *Backend) {
	ok := false
	resp, err := p.client.Get(b.URL.JoinPath("health").String())
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		ok = resp.StatusCode == http.StatusOK
	}
	if was := b.alive.Swap(ok); was != ok {
		p.logf("backend %s is now %s", b.URL, map[bool]string{true: "UP", false: "DOWN"}[ok])
	}
}

// CheckAll health-checks every backend in parallel and waits for the results.
func (p *Pool) CheckAll() {
	var wg sync.WaitGroup
	for _, b := range p.backends {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.check(b)
		}()
	}
	wg.Wait()
}

// RunHealthChecks checks immediately, then every interval until ctx ends.
func (p *Pool) RunHealthChecks(ctx context.Context, interval time.Duration) {
	p.CheckAll()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.CheckAll()
		}
	}
}

func (p *Pool) Status() []BackendStatus {
	out := make([]BackendStatus, 0, len(p.backends))
	for _, b := range p.backends {
		out = append(out, BackendStatus{
			URL:    b.URL.String(),
			Alive:  b.Alive(),
			Active: b.active.Load(),
			Served: b.served.Load(),
			Failed: b.failed.Load(),
		})
	}
	return out
}

func (p *Pool) Strategy() Strategy { return p.strategy }
