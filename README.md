# Switchboard

A reverse proxy and load balancer in Go, built to understand how this stuff works instead of using NGINX blindly. Standard library only.

client → switchboard → backend(s)

## Try it

Start four backends, each in its own terminal:

```sh
go run ./cmd/backend -port 9001 -name api-1
go run ./cmd/backend -port 9002 -name api-2 -latency 20
go run ./cmd/backend -port 9003 -name api-3 -latency 40 -jitter 30
go run ./cmd/backend -port 9004 -name api-4
```

Then the balancer:

```sh
go run ./cmd/switchboard -loadgen -backends http://localhost:9001,http://localhost:9002,http://localhost:9003,http://localhost:9004
```

- http://localhost:8080/ goes through the balancer. Each backend serves its own page (name, colour, port, process ID, requests served), so reloading shows the balancer moving between backends.
- http://localhost:8080/_switchboard/ is the status page.

For a one-command version, `go run ./cmd/switchboard -demo 4` runs four backends inside the process instead.

## Features

**Balancing.** Five strategies, switchable at runtime:

| Strategy | How it picks |
| --- | --- |
| `roundrobin` | each backend in turn |
| `weighted` | nginx-style smooth weighted round robin |
| `leastconn` | fewest requests in flight, relative to weight |
| `p2c` | power of two choices: two random backends, keep the one with the lower smoothed latency × queue depth |
| `hash` | consistent hashing on the client address, so a client sticks to one backend and only nearby clients move when backends change |

**Failure handling.**
- active health checks on `/health` (parallel, 2s timeout)
- a circuit breaker per backend: opens after consecutive failures (transport errors or 5xx), lets one trial request through after a cooldown, closes when it succeeds
- a backend that drops a connection is marked down at once, and GET/HEAD/OPTIONS requests are retried on another backend
- draining: a backend stops taking new requests while in-flight ones finish

**Traffic control.** Per-client token-bucket rate limiting, adjustable at runtime.

**Observability.** Per-backend p50/p95/p99 and smoothed latency, request rates, a live event stream (server-sent events), Prometheus metrics at `/_switchboard/metrics`, JSON status at `/_switchboard/status.json`.

**Runtime admin.** Add, remove, reweight and drain backends, change strategy and rate limit without restarting. The admin API has no authentication, so don't expose it to the internet.

## Status page

One page at `/_switchboard/`, updated live:

- strategy, generated traffic (with `-loadgen` or `-demo`) and rate limit controls
- a button that sends one request to `/` and shows which backend answered
- a backends table: status (UP, DOWN, OPEN, HALF-OPEN, DRAINING), weight, in-flight, served, failed, p50/p95/p99, share of the last 10 seconds of traffic, and drain, fault injection and remove actions
- requests per second by backend and latency over the last minute
- a sample of recent requests, with every error kept

Fault injection works on any backend started with `cmd/backend` or `-demo`: added latency, jitter, error rate, or "down" (fail health checks and drop connections). Faults can only be changed from the same machine, never through the proxy.

## Backend flags

```
-port        port to listen on (default 9001)
-name        name shown on its page and in Switchboard (default backend-PORT)
-latency     added latency in ms
-jitter      random extra latency in ms
-error-rate  fraction of requests answered with 500
```

## Config

```sh
go run ./cmd/switchboard -config switchboard.example.json
go run ./cmd/switchboard -strategy p2c -backends api-1=http://localhost:9001,api-2=http://localhost:9002
```

Flags override the file. Backends can be plain URLs, `name=url` on the command line, or `{"url": ..., "name": ..., "weight": n}` in the file. A backend's name defaults to the one it reports in the `X-Backend-Name` header of `/health`.

## Layout

```
cmd/switchboard     the proxy
cmd/backend         demo backend with its own page
internal/balancer   pool, strategies, breaker, rate limiter, events, metrics
internal/web        admin API and status page (embedded, no build step)
internal/demo       demo backend with fault injection
internal/loadgen    traffic generator
```

## Test

```sh
go test -race ./...
```
