package balancer

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func backendServer(name string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, name) })
	return httptest.NewServer(mux)
}

func get(t *testing.T, h http.Handler) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	body, _ := io.ReadAll(rec.Body)
	return rec.Code, string(body)
}

func newPool(t *testing.T, s Strategy, urls ...string) *Pool {
	t.Helper()
	p, err := New(urls, s)
	if err != nil {
		t.Fatal(err)
	}
	p.logf = t.Logf
	p.CheckAll()
	return p
}

func TestRoundRobinRotates(t *testing.T) {
	a, b := backendServer("a"), backendServer("b")
	defer a.Close()
	defer b.Close()
	p := newPool(t, RoundRobin, a.URL, b.URL)

	var got string
	for i := 0; i < 4; i++ {
		_, body := get(t, p)
		got += body
	}
	if got != "abab" {
		t.Fatalf("got %q, want abab", got)
	}
}

func TestDeadBackendSkippedByHealthCheck(t *testing.T) {
	a, b := backendServer("a"), backendServer("b")
	defer a.Close()
	b.Close()
	p := newPool(t, RoundRobin, a.URL, b.URL)

	for i := 0; i < 3; i++ {
		if _, body := get(t, p); body != "a" {
			t.Fatalf("got %q, want a", body)
		}
	}
}

func TestRetryOnMidFlightFailure(t *testing.T) {
	a, b := backendServer("a"), backendServer("b")
	defer a.Close()
	p := newPool(t, RoundRobin, a.URL, b.URL)
	b.Close() // dies after the health check said it was up

	for i := 0; i < 3; i++ {
		code, body := get(t, p)
		if code != 200 || body != "a" {
			t.Fatalf("got %d %q, want 200 a", code, body)
		}
	}
	if p.Status()[1].Alive {
		t.Fatal("failed backend should be marked dead")
	}
}

func TestNoHealthyBackends(t *testing.T) {
	a := backendServer("a")
	a.Close()
	p := newPool(t, RoundRobin, a.URL)
	if code, _ := get(t, p); code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", code)
	}
}

func TestLeastConnPrefersIdle(t *testing.T) {
	a, b := backendServer("a"), backendServer("b")
	defer a.Close()
	defer b.Close()
	p := newPool(t, LeastConn, a.URL, b.URL)
	p.backends[0].active.Store(5)
	if _, body := get(t, p); body != "b" {
		t.Fatalf("got %q, want b", body)
	}
}

func TestConcurrentRequests(t *testing.T) {
	a, b := backendServer("a"), backendServer("b")
	defer a.Close()
	defer b.Close()
	p := newPool(t, RoundRobin, a.URL, b.URL)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, _ := get(t, p); code != 200 {
				t.Errorf("got %d", code)
			}
		}()
	}
	wg.Wait()
}

func TestInvalidConfig(t *testing.T) {
	if _, err := New(nil, RoundRobin); err == nil {
		t.Fatal("expected error for no backends")
	}
	if _, err := New([]string{"not a url"}, RoundRobin); err == nil {
		t.Fatal("expected error for bad URL")
	}
	if _, err := New([]string{"http://x:1"}, "nope"); err == nil {
		t.Fatal("expected error for bad strategy")
	}
}
