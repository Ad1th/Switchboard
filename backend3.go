package main

import (
	"fmt"
	"net/http"
)

func main() {
	port := "9003"

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Hello from backend %s\n", port)
	})

	fmt.Println("Backend running on port", port)
	http.ListenAndServe(":"+port, nil)
}