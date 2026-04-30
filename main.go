package main

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

type Backend struct {
	URL   *url.URL
	Alive bool
}

var mu sync.Mutex
var current = 0

var backends = []*Backend{}

func getNextBackend() *Backend {
	mu.Lock()
	defer mu.Unlock()

	for i := 0; i < len(backends); i++ {
		b := backends[current]
		current = (current + 1) % len(backends)

		if b.Alive {
			return b
		}
	}
	return nil
}

func healthCheck() {
	for {
		for _, b := range backends {
			resp, err := http.Get(b.URL.String() + "/health")

			mu.Lock()
			if err != nil || resp.StatusCode != 200 {
				b.Alive = false
			} else {
				b.Alive = true
			}
			mu.Unlock()
		}
		time.Sleep(3 * time.Second)
	}
}

func main() {
	backendURLs := []string{
		"http://localhost:9001",
		"http://localhost:9002",
		"http://localhost:9003",
	}

	for _, b := range backendURLs {
		u, _ := url.Parse(b)
		backends = append(backends, &Backend{
			URL:   u,
			Alive: true,
		})
	}

	go healthCheck()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		backend := getNextBackend()

		if backend == nil {
			http.Error(w, "No healthy backends available", http.StatusServiceUnavailable)
			return
		}

		fmt.Println("Routing to:", backend.URL)

		proxy := httputil.NewSingleHostReverseProxy(backend.URL)
		proxy.ServeHTTP(w, r)
	})

	fmt.Println("⚡ Switchboard running on port 8080")
	http.ListenAndServe(":8080", nil)
}