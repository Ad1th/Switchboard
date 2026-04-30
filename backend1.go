package main

import (
	"fmt"
	"net/http"
)

func main() {
	port := "9001"

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Hello from backend %s\n", port)
	})

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(200)
	fmt.Fprintln(w, "OK")
})

	fmt.Println("Backend running on port", port)
	http.ListenAndServe(":"+port, nil)
}
