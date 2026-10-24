// Package balancer implements an HTTP load balancer: a pool of backends that
// can change at runtime, pluggable selection strategies, active health
// checks, per-backend circuit breakers, retries, per-client rate limiting and
// a live event stream.
package balancer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Options struct {
	Strategy         string
	BreakerThreshold int           // consecutive failures that open a breaker
	BreakerCooldown  time.Duration // how long a breaker stays open
	RateLimit        float64       // requests per second per client, 0 = off
	RateBurst        float64
	TrustForwarded   bool // take the client address from X-Forwarded-For
	Logf             func(format string, args ...any)
}

type BackendConfig struct {
	URL    string
	Weight int64
}

type Pool struct {
	backends atomic.Pointer[[]*Backend]
	strategy atomic.Pointer[strategyBox]
	mu       sync.Mutex // serialises membership changes
	nextID   int

	opts    Options
	limiter *RateLimiter
	hub     *Hub
	stats   counters
	started time.Time
	client  *http.Client
	logf    func(format string, args ...any)
}

type strategyBox struct{ Strategy }

type failureKey struct{}

func New(backends []BackendConfig, opts Options) (*Pool, error) {
	if len(backends) == 0 {
		return nil, errors.New("at least one backend is required")
	}
	if opts.Strategy == "" {
		opts.Strategy = "roundrobin"
	}
	if opts.BreakerThreshold <= 0 {
		opts.BreakerThreshold = 3
	}
	if opts.BreakerCooldown <= 0 {
		opts.BreakerCooldown = 5 * time.Second
	}
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	p := &Pool{
		opts:    opts,
		limiter: NewRateLimiter(opts.RateLimit, opts.RateBurst),
		hub:     NewHub(),
		started: time.Now(),
		client:  &http.Client{Timeout: 2 * time.Second},
		logf:    opts.Logf,
	}
	p.backends.Store(&[]*Backend{})
	if err := p.SetStrategy(opts.Strategy); err != nil {
		return nil, err
	}
	for _, bc := range backends {
		if _, err := p.add(bc, false); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *Pool) Hub() *Hub { return p.hub }

func (p *Pool) SetStrategy(name string) error {
	s, err := NewStrategy(name)
	if err != nil {
		return err
	}
	p.strategy.Store(&strategyBox{s})
	return nil
}

func (p *Pool) StrategyName() string { return p.strategy.Load().Name() }

func (p *Pool) Backends() []*Backend { return *p.backends.Load() }

func (p *Pool) Get(id string) *Backend {
	for _, b := range p.Backends() {
		if b.ID == id {
			return b
		}
	}
	return nil
}

// Add registers a backend and health-checks it before it takes traffic.
func (p *Pool) Add(bc BackendConfig) (*Backend, error) { return p.add(bc, true) }

func (p *Pool) add(bc BackendConfig, check bool) (*Backend, error) {
	u, err := url.Parse(bc.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid backend URL %q", bc.URL)
	}
	if bc.Weight <= 0 {
		bc.Weight = 1
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.Backends() {
		if b.URL.String() == u.String() {
			return nil, fmt.Errorf("backend %s is already in the pool", u)
		}
	}
	p.nextID++
	b := &Backend{ID: strconv.Itoa(p.nextID), URL: u}
	b.weight.Store(bc.Weight)
	b.breaker.init(p.opts.BreakerThreshold, p.opts.BreakerCooldown)
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
	if check {
		p.check(b)
	}
	next := append(slices.Clone(p.Backends()), b)
	p.backends.Store(&next)
	return b, nil
}

// Remove takes a backend out of the pool. In-flight requests finish normally.
func (p *Pool) Remove(id string) (*Backend, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	bs := p.Backends()
	i := slices.IndexFunc(bs, func(b *Backend) bool { return b.ID == id })
	if i < 0 {
		return nil, fmt.Errorf("no backend %q", id)
	}
	if len(bs) == 1 {
		return nil, errors.New("cannot remove the last backend")
	}
	next := slices.Delete(slices.Clone(bs), i, i+1)
	p.backends.Store(&next)
	return bs[i], nil
}

func (p *Pool) SetWeight(id string, w int64) error {
	b := p.Get(id)
	if b == nil {
		return fmt.Errorf("no backend %q", id)
	}
	if w < 0 || w > 100 {
		return errors.New("weight must be between 0 and 100")
	}
	b.weight.Store(w)
	return nil
}

func (p *Pool) SetDraining(id string, draining bool) error {
	b := p.Get(id)
	if b == nil {
		return fmt.Errorf("no backend %q", id)
	}
	b.draining.Store(draining)
	return nil
}

func (p *Pool) SetRateLimit(rate, burst float64) { p.limiter.Set(rate, burst) }

func (p *Pool) clientAddr(r *http.Request) string {
	if p.opts.TrustForwarded {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			return strings.TrimSpace(first)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func retryable(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return r.Body == nil || r.Body == http.NoBody
	}
	return false
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (p *Pool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ev := RequestEvent{Client: p.clientAddr(r), Method: r.Method, Path: r.URL.RequestURI()}
	finish := func(status int) {
		now := time.Now()
		ev.T, ev.Status = now.UnixMilli(), status
		ev.Ms = float64(now.Sub(start).Microseconds()) / 1000
		p.stats.requests.Add(1)
		p.stats.tick(now)
		if status >= 500 {
			p.stats.errors.Add(1)
		}
		p.hub.Publish(ev)
	}

	if !p.limiter.Allow(ev.Client, start) {
		p.stats.limited.Add(1)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Too many requests", http.StatusTooManyRequests)
		finish(http.StatusTooManyRequests)
		return
	}

	tried := map[*Backend]bool{}
	strategy := p.strategy.Load()
	for {
		now := time.Now()
		b := strategy.Pick(p.Backends(), ev.Client, func(b *Backend) bool {
			return !tried[b] && b.Available(now)
		})
		if b == nil {
			if len(tried) > 0 {
				http.Error(w, "All backends failed", http.StatusBadGateway)
				finish(http.StatusBadGateway)
			} else {
				http.Error(w, "No healthy backends available", http.StatusServiceUnavailable)
				finish(http.StatusServiceUnavailable)
			}
			return
		}
		tried[b] = true
		if !b.breaker.acquire(now) {
			continue // another request took the half-open trial first
		}
		ev.Backend = b.ID
		ev.Attempts++
		if ev.Attempts > 1 {
			p.stats.retries.Add(1)
		}

		var proxyErr error
		rec := &statusRecorder{ResponseWriter: w}
		ctx := context.WithValue(r.Context(), failureKey{}, &proxyErr)
		attemptStart := time.Now()
		b.active.Add(1)
		b.proxy.ServeHTTP(rec, r.WithContext(ctx))
		b.active.Add(-1)
		done := time.Now()

		if proxyErr == nil {
			b.observe(done.Sub(attemptStart))
			b.served.Add(1)
			ok := rec.status < 500
			if !ok {
				b.failed.Add(1)
			}
			if b.breaker.record(ok, done) {
				p.logf("backend %s breaker is now %s", b.URL, b.breaker.State(done))
			}
			finish(rec.status)
			return
		}
		if r.Context().Err() != nil {
			b.breaker.release()
			finish(499) // client closed the request
			return
		}
		b.failed.Add(1)
		if b.healthy.Swap(false) {
			p.logf("backend %s failed (%v), marked down", b.URL, proxyErr)
		}
		b.breaker.record(false, done)
		if !retryable(r) {
			http.Error(w, "Bad gateway", http.StatusBadGateway)
			finish(http.StatusBadGateway)
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
	if was := b.healthy.Swap(ok); was != ok {
		p.logf("backend %s is now %s", b.URL, map[bool]string{true: "UP", false: "DOWN"}[ok])
	}
}

// CheckAll health-checks every backend in parallel and waits for the results.
func (p *Pool) CheckAll() {
	var wg sync.WaitGroup
	for _, b := range p.Backends() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.check(b)
		}()
	}
	wg.Wait()
}

// Run health-checks immediately, then every interval, and drives the event
// hub, until ctx ends.
func (p *Pool) Run(ctx context.Context, interval time.Duration) {
	go p.hub.run(ctx.Done())
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
