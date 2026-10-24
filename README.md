# Switchboard

A reverse proxy and load balancer in Go, built to understand how this stuff works instead of using NGINX blindly. It comes with a live control panel styled as an old telephone exchange: every request is a patch cord from a caller to the backend line that answered it.

client → switchboard → backend(s)

## Try it

```sh
go run ./cmd/switchboard -demo 5
open http://localhost:8080/_switchboard/
```

`-demo 5` starts five demo backends inside the process and a traffic generator. In the control panel, pull the traffic lever, turn the routing dial, and click any jack to inject latency, errors or a dead line, then watch the breakers trip and the calls reroute.

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
- draining: a backend can stop taking new requests while in-flight ones finish

**Traffic control.** Per-client token-bucket rate limiting, adjustable at runtime.

**Observability.** Per-backend p50/p95/p99 and smoothed latency, request rates, a live event stream (server-sent events), Prometheus metrics at `/_switchboard/metrics`, and JSON status at `/_switchboard/status.json`.

**Runtime admin.** Add, remove, reweight and drain backends, change strategy and rate limit without restarting. Protect it with `-admin-token` (or `SWITCHBOARD_ADMIN_TOKEN`). Without a token, anyone who can reach the proxy can change settings.

## Control panel

| Part of the board | What it shows or does |
| --- | --- |
| Meters | calls per second, p95 answer time, share of failed calls |
| Jack field | one line per backend; the lamp glows with load, a shutter drops when the line stops answering or its breaker opens |
| Cords | each finished request: tan answered, amber slow, red failed, grey refused (rate limited or nowhere to go), dashed brass if it was retried |
| Routing dial | the balancing strategy |
| Traffic lever | the built-in traffic generator (demo mode) |
| Line card | click a jack: stats, weight, drain, fault injection, remove |
| Chart recorder | p50, p95 and request rate over the last ~100 seconds |
| Call log | a sample of recent requests, with every failure included |

## Run against your own backends

```sh
go run ./cmd/backend -port 9001 &
go run ./cmd/backend -port 9002 -latency 80 -jitter 40 &
go run ./cmd/backend -port 9003 -error-rate 0.1 &

go run ./cmd/switchboard -strategy p2c -backends http://localhost:9001,http://localhost:9002,http://localhost:9003
```

`cmd/backend` runs the same demo server as `-demo`, so its faults can be changed from the control panel. Fault changes are only accepted from the same machine and never through the proxy.

## Config

```sh
go run ./cmd/switchboard -config switchboard.example.json
```

Flags override the file. See `switchboard.example.json` for every setting. Backends can be plain URLs or `{"url": ..., "weight": n}`.

## Layout

```
cmd/switchboard     the proxy
cmd/backend         demo backend
internal/balancer   pool, strategies, breaker, rate limiter, events, metrics
internal/web        admin API and control panel (embedded, no build step)
internal/demo       demo backend with fault injection
internal/loadgen    traffic generator
```

## Test

```sh
go test -race ./...
```
