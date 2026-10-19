// Command backend is a tiny demo server for trying out Switchboard.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

func main() {
	port := flag.String("port", "9001", "port to listen on")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "OK")
	})
	// /?delay=500 sleeps 500ms, handy for watching least-connections work.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if ms, err := strconv.Atoi(r.URL.Query().Get("delay")); err == nil && ms > 0 {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
		fmt.Fprintf(w, "Hello from backend %s\n", *port)
	})

	srv := &http.Server{
		Addr:              ":" + *port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Println("Backend running on port", *port)
	log.Fatal(srv.ListenAndServe())
}
