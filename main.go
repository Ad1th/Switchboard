package main

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
)

func main() {
	target, _ := url.Parse("http://localhost:9001")

	proxy := httputil.NewSingleHostReverseProxy(target)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("Routing request to backend...")
		proxy.ServeHTTP(w, r)
	})

	fmt.Println("Switchboard running on port 8080")
	http.ListenAndServe(":8080", nil)
}