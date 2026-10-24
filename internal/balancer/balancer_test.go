package balancer

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func backendServer(name string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, name) })
	return httptest.NewServer(mux)
}

func get(t *testing.T, h http.Handler, remote string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	if remote != "" {
		req.RemoteAddr = remote
	}
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	return rec.Code, string(body)
}

func newPool(t *testing.T, opts Options, urls ...string) *Pool {
	t.Helper()
	var bcs []BackendConfig
	for _, u := range urls {
		bcs = append(bcs, BackendConfig{URL: u})
	}
	opts.Logf = t.Logf
	p, err := New(bcs, opts)
	if err != nil {
		t.Fatal(err)
	}
	p.CheckAll()
	return p
}

func servers(t *testing.T, names ...string) []string {
	t.Helper()
	var urls []string
	for _, n := range names {
		s := backendServer(n)
		t.Cleanup(s.Close)
		urls = append(urls, s.URL)
	}
	return urls
}

func sequence(t *testing.T, p *Pool, n int) string {
	t.Helper()
	var got string
	for range n {
		_, body := get(t, p, "")
		got += body
	}
	return got
}

func TestRoundRobinRotates(t *testing.T) {
	p := newPool(t, Options{}, servers(t, "a", "b")...)
	if got := sequence(t, p, 4); got != "abab" {
		t.Fatalf("got %q, want abab", got)
	}
}

func TestWeightedIsSmooth(t *testing.T) {
	p := newPool(t, Options{Strategy: "weighted"}, servers(t, "a", "b", "c")...)
	p.SetWeight("1", 5)
	// nginx smooth weighted round robin for weights 5,1,1
	if got := sequence(t, p, 7); got != "aabacaa" {
		t.Fatalf("got %q, want aabacaa", got)
	}
}

func TestDeadBackendSkippedByHealthCheck(t *testing.T) {
	urls := servers(t, "a")
	dead := backendServer("b")
	dead.Close()
	p := newPool(t, Options{}, urls[0], dead.URL)
	if got := sequence(t, p, 3); got != "aaa" {
		t.Fatalf("got %q, want aaa", got)
	}
}

func TestRetryOnMidFlightFailure(t *testing.T) {
	a := backendServer("a")
	defer a.Close()
	b := backendServer("b")
	p := newPool(t, Options{}, a.URL, b.URL)
	b.Close() // dies after the health check said it was up

	for range 3 {
		if code, body := get(t, p, ""); code != 200 || body != "a" {
			t.Fatalf("got %d %q, want 200 a", code, body)
		}
	}
	if p.Get("2").healthy.Load() {
		t.Fatal("failed backend should be marked down")
	}
	if p.Snapshot().Retries == 0 {
		t.Fatal("expected a retry to be counted")
	}
}

func TestNoHealthyBackends(t *testing.T) {
	a := backendServer("a")
	a.Close()
	p := newPool(t, Options{}, a.URL)
	if code, _ := get(t, p, ""); code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", code)
	}
}

func TestLeastConnPrefersIdle(t *testing.T) {
	p := newPool(t, Options{Strategy: "leastconn"}, servers(t, "a", "b")...)
	p.Get("1").active.Store(5)
	if _, body := get(t, p, ""); body != "b" {
		t.Fatalf("got %q, want b", body)
	}
}

func TestP2CAvoidsSlowBackend(t *testing.T) {
	p := newPool(t, Options{Strategy: "p2c"}, servers(t, "a", "b")...)
	p.Get("1").observe(500 * time.Millisecond)
	p.Get("2").observe(5 * time.Millisecond)
	if got := sequence(t, p, 5); got != "bbbbb" {
		t.Fatalf("got %q, want all b", got)
	}
}

func TestHashIsStickyAndStable(t *testing.T) {
	p := newPool(t, Options{Strategy: "hash"}, servers(t, "a", "b", "c")...)
	clients := map[string]string{}
	for i := range 50 {
		addr := fmt.Sprintf("10.0.0.%d:1234", i)
		_, first := get(t, p, addr)
		_, again := get(t, p, addr)
		if first != again {
			t.Fatalf("client %s moved from %s to %s", addr, first, again)
		}
		clients[addr] = first
	}
	// Removing c must only move the clients that were on c.
	if _, err := p.Remove("3"); err != nil {
		t.Fatal(err)
	}
	for addr, was := range clients {
		if _, now := get(t, p, addr); was != "c" && now != was {
			t.Fatalf("client %s moved from %s to %s though its backend stayed", addr, was, now)
		}
	}
}

func TestBreakerOpensAndRecovers(t *testing.T) {
	fail := true
	var mu sync.Mutex
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/health" && fail {
			w.WriteHeader(500)
		}
	}))
	defer s.Close()
	p := newPool(t, Options{BreakerThreshold: 2, BreakerCooldown: 50 * time.Millisecond}, s.URL)

	get(t, p, "")
	get(t, p, "")
	if code, _ := get(t, p, ""); code != http.StatusServiceUnavailable {
		t.Fatalf("breaker should be open, got %d", code)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	time.Sleep(60 * time.Millisecond)
	if code, _ := get(t, p, ""); code != 200 {
		t.Fatalf("half-open trial should pass, got %d", code)
	}
	if st := p.Get("1").breaker.State(time.Now()); st != BreakerClosed {
		t.Fatalf("breaker is %s, want closed", st)
	}
}

func TestRateLimitPerClient(t *testing.T) {
	p := newPool(t, Options{RateLimit: 1, RateBurst: 2}, servers(t, "a")...)
	codes := []int{}
	for range 3 {
		code, _ := get(t, p, "1.1.1.1:1")
		codes = append(codes, code)
	}
	if fmt.Sprint(codes) != "[200 200 429]" {
		t.Fatalf("got %v", codes)
	}
	if code, _ := get(t, p, "2.2.2.2:1"); code != 200 {
		t.Fatalf("other client should not be limited, got %d", code)
	}
}

func TestDrainAndRuntimeMembership(t *testing.T) {
	urls := servers(t, "a", "b", "c")
	p := newPool(t, Options{}, urls[:2]...)
	p.SetDraining("1", true)
	if got := sequence(t, p, 3); got != "bbb" {
		t.Fatalf("got %q, want bbb while a drains", got)
	}
	if _, err := p.Add(BackendConfig{URL: urls[2]}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Add(BackendConfig{URL: urls[2]}); err == nil {
		t.Fatal("duplicate backend should be rejected")
	}
	p.SetDraining("1", false)
	if got := sequence(t, p, 3); len(got) != 3 || got == "bbb" {
		t.Fatalf("new backend should take traffic, got %q", got)
	}
}

func TestConcurrentRequests(t *testing.T) {
	for _, s := range StrategyNames() {
		p := newPool(t, Options{Strategy: s}, servers(t, "a", "b")...)
		var wg sync.WaitGroup
		for range 50 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if code, _ := get(t, p, ""); code != 200 {
					t.Errorf("%s: got %d", s, code)
				}
			}()
		}
		wg.Wait()
	}
}

func TestInvalidConfig(t *testing.T) {
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("expected error for no backends")
	}
	if _, err := New([]BackendConfig{{URL: "not a url"}}, Options{}); err == nil {
		t.Fatal("expected error for bad URL")
	}
	if _, err := New([]BackendConfig{{URL: "http://x:1"}}, Options{Strategy: "nope"}); err == nil {
		t.Fatal("expected error for bad strategy")
	}
}
