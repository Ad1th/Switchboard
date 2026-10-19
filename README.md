# Switchboard

A small reverse proxy and load balancer in Go, built to understand how this stuff works instead of using NGINX blindly.

client → switchboard → backend(s)

## Features

- round robin or least-connections balancing
- active health checks (parallel, 2s timeout) plus passive detection: a backend that fails mid-request is marked dead immediately
- automatic retry on the next backend for idempotent requests (GET/HEAD/OPTIONS)
- `X-Forwarded-*` headers, `Host` rewritten to the backend
- JSON config file or flags
- status dashboard at `/_switchboard/` and JSON at `/_switchboard/status.json`
- graceful shutdown

## Run

```sh
go run ./cmd/backend -port 9001 &
go run ./cmd/backend -port 9002 &
go run ./cmd/backend -port 9003 &

go run ./cmd/switchboard            # defaults: :8080, the three backends above
curl localhost:8080                 # hit it repeatedly
open http://localhost:8080/_switchboard/
```

Kill a backend and keep curling: no request fails, and the dashboard shows it DOWN.

## Config

```sh
go run ./cmd/switchboard -config switchboard.example.json
go run ./cmd/switchboard -strategy leastconn -backends http://a:9001,http://b:9001
```

Flags override the file. See `switchboard.example.json`.

## Test

```sh
go test -race ./...
```
