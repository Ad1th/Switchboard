# Switchboard

---

Switchboard

A small reverse proxy + load balancer I built in Go to understand how this stuff actually works (instead of just using NGINX blindly).

⸻

what it does

- listens on localhost:8080
- forwards requests to multiple backend servers
- rotates between them (round robin)
- skips a backend if it goes down

basically:

client → switchboard → backend(s)

nothing fancy, just the core idea working.

⸻

how to run

1. start a few backend servers

you’ll need 2–3 simple servers running on different ports.

example:

package main
import (
"fmt"
"net/http"
)
func main() {
port := "9001"
http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
fmt.Fprintf(w, "hello from %s\n", port)
})
http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
w.WriteHeader(200)
})
http.ListenAndServe(":"+port, nil)
}

run this on:

- 9001
- 9002
- 9003

⸻

2. start switchboard

go run main.go

⸻

3. test it

curl localhost:8080

hit it multiple times — you should see responses switch between servers.

⸻

health checks

switchboard periodically calls:

/health

if a backend:

- stops responding
- or returns non-200

it gets marked as dead and skipped automatically.

⸻

why i made this

mostly to understand:

- how reverse proxies actually forward requests
- how load balancing works under the hood
- how to handle shared state safely (mutex, etc.)

also just wanted to build something a bit closer to “real infra” instead of another CRUD app.

⸻

tech

- Go (net/http, httputil)
- goroutines
- mutex for safe routing

⸻

things i might add later

- better balancing (least connections)
- retry logic if a backend fails
- config file instead of hardcoding servers
- CLI (switchboard start etc.)

⸻

quick run

# start backends

go run backend1.go
go run backend2.go
go run backend3.go

# start proxy

go run main.go

# test

curl localhost:8080

⸻

that’s it — simple, but it works

⸻
