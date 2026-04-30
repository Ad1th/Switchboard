package main

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
)


var mu sync.Mutex

var backends = []string{
	"http://localhost:9001",
	"http://localhost:9002",
	"http://localhost:9003",
}

var current = 0

func getNextBackend() string {
	mu.Lock()
	defer mu.Unlock()

	url := backends[current]
	current = (current + 1) % len(backends)
	return url
}
func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		targetURL, _ := url.Parse(getNextBackend())

		fmt.Println("Routing to:", targetURL)

		proxy := httputil.NewSingleHostReverseProxy(targetURL)
		proxy.ServeHTTP(w, r)
	})

	fmt.Println("⚡ Switchboard running on port 8080")
	http.ListenAndServe(":8080", nil)
}