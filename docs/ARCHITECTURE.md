# Switchboard architecture

Diagrams of how Switchboard is put together and how a request moves through it. GitHub renders the Mermaid blocks below.

## 1. System overview

```mermaid
flowchart LR
    subgraph clients["Clients"]
        browser["Browser"]
        curl["curl / scripts"]
        loadgen["Traffic generator<br/>(-loadgen / -demo)"]
    end

    subgraph sb["Switchboard process :8080"]
        mux["HTTP mux"]
        pool["Pool<br/>(load balancer)"]
        web["Status page + admin API<br/>/_switchboard/"]
        health["Health checker"]
        hub["Event hub"]
    end

    subgraph backends["Backends (separate processes)"]
        b1["api-1 :9001"]
        b2["api-2 :9002"]
        b3["api-3 :9003"]
        b4["api-4 :9004"]
    end

    browser -->|"GET /"| mux
    curl --> mux
    loadgen -->|"X-Forwarded-For: fake callers"| mux
    browser -->|"GET /_switchboard/"| mux
    mux -->|"everything else"| pool
    mux -->|"/_switchboard/*"| web
    pool --> b1 & b2 & b3 & b4
    health -->|"GET /health every 3 s"| b1 & b2 & b3 & b4
    pool -->|"finished requests"| hub
    hub -->|"server-sent events"| web
    web -->|"strategy, weights, drain, faults"| pool
```

## 2. Packages

```mermaid
flowchart TD
    cmdsb["cmd/switchboard<br/>flags, config, server"]
    cmdbe["cmd/backend<br/>stand-alone demo backend"]
    balancer["internal/balancer<br/>pool, strategies, breaker,<br/>rate limiter, events, metrics"]
    web["internal/web<br/>admin API, status page (embedded)"]
    demo["internal/demo<br/>demo server, shop, fault injection"]
    loadgen["internal/loadgen<br/>traffic generator"]

    cmdsb --> balancer
    cmdsb --> web
    cmdsb --> demo
    cmdsb --> loadgen
    web --> balancer
    web --> demo
    web --> loadgen
    cmdbe --> demo
```

`internal/balancer` depends only on the standard library. Nothing imports `cmd/*`.

## 3. Core types

```mermaid
classDiagram
    class Pool {
        -backends atomic.Pointer~[]*Backend~
        -strategy atomic.Pointer~Strategy~
        -limiter *RateLimiter
        -hub *Hub
        +ServeHTTP(w, r)
        +Add(BackendConfig) *Backend
        +Remove(id) *Backend
        +SetStrategy(name)
        +SetWeight(id, w)
        +SetDraining(id, bool)
        +Run(ctx, interval)
        +Snapshot() Snapshot
    }
    class Backend {
        +ID string
        +URL *url.URL
        -proxy *httputil.ReverseProxy
        -weight atomic.Int64
        -healthy atomic.Bool
        -draining atomic.Bool
        -breaker breaker
        -active, served, failed atomic.Int64
        -ewma latency
        -lat latencyWindow
        +Available(now) bool
        +Name() string
    }
    class breaker {
        -state closed | open | half-open
        -fails int
        -openedAt time
        +ready(now) bool
        +acquire(now) bool
        +record(ok, now)
        +release()
    }
    class Strategy {
        <<interface>>
        +Name() string
        +Pick(backends, client, ok) *Backend
    }
    class RateLimiter {
        -buckets map~client~bucket~
        +Allow(client, now) bool
        +Set(rate, burst)
    }
    class Hub {
        -pending []RequestEvent
        -subs set~chan~
        +Publish(RequestEvent)
        +Subscribe() chan
    }
    Pool "1" o-- "*" Backend
    Pool --> Strategy
    Pool --> RateLimiter
    Pool --> Hub
    Backend *-- breaker
    Strategy <|.. roundRobin
    Strategy <|.. weighted
    Strategy <|.. leastConn
    Strategy <|.. p2c
    Strategy <|.. consistentHash
```

## 4. Life of a request

```mermaid
sequenceDiagram
    autonumber
    participant C as Client
    participant P as Pool.ServeHTTP
    participant RL as Rate limiter
    participant S as Strategy
    participant BR as Breaker (backend X)
    participant X as Backend X
    participant Y as Backend Y
    participant H as Event hub

    C->>P: GET /cart
    P->>RL: Allow(client)?
    alt over the limit
        RL-->>P: no
        P-->>C: 429 Too Many Requests
        P->>H: event (429)
    else allowed
        RL-->>P: yes
        P->>S: Pick(available, not yet tried)
        S-->>P: backend X
        P->>BR: acquire()
        BR-->>P: allowed
        P->>X: proxied request (active++)
        alt X answers
            X-->>P: 200 (or 5xx)
            P->>BR: record(status < 500)
            P-->>C: response
            P->>H: event (backend X, status, latency)
        else connection fails
            X--xP: connection refused / reset
            P->>P: mark X down, breaker failure
            Note over P: GET/HEAD/OPTIONS without a body only
            P->>S: Pick again, excluding X
            S-->>P: backend Y
            P->>Y: proxied request
            Y-->>P: 200
            P-->>C: response (client never sees the failure)
            P->>H: event (backend Y, attempts = 2)
        end
    end
```

## 5. Request handling decisions

```mermaid
flowchart TD
    start(["Request arrives"]) --> limit{"Client within<br/>rate limit?"}
    limit -- no --> r429["429 Too Many Requests"]
    limit -- yes --> pick["Strategy picks a backend that is<br/>healthy, not draining, weight > 0,<br/>breaker allows, not tried yet"]
    pick --> found{"Backend<br/>found?"}
    found -- "no, none tried" --> r503["503 No healthy backends"]
    found -- "no, all tried failed" --> r502["502 All backends failed"]
    found -- yes --> acq{"Breaker grants<br/>the request?"}
    acq -- "no (half-open trial taken)" --> pick
    acq -- yes --> proxy["Proxy to backend"]
    proxy --> ok{"Backend<br/>answered?"}
    ok -- yes --> rec["Record latency, outcome,<br/>breaker result"] --> resp["Return backend's response"]
    ok -- "no, client hung up" --> r499["Stop (499), release breaker trial"]
    ok -- "no, connection error" --> down["Mark backend down,<br/>breaker failure"]
    down --> idem{"GET / HEAD / OPTIONS<br/>without a body?"}
    idem -- yes --> pick
    idem -- no --> r502b["502 Bad gateway"]
```

## 6. Circuit breaker

```mermaid
stateDiagram-v2
    [*] --> Closed
    Closed --> Closed: success (failure count reset)
    Closed --> Closed: failure, below threshold
    Closed --> Open: 3rd consecutive failure
    Open --> Open: requests refused during cooldown (5 s)
    Open --> HalfOpen: cooldown elapsed
    HalfOpen --> Closed: trial request succeeds
    HalfOpen --> Open: trial request fails
    note right of HalfOpen
        Only one trial request is let through.
        Others go to other backends.
    end note
    note right of Closed
        A failure is a connection error
        or a 5xx response.
    end note
```

## 7. Backend availability

A backend takes new requests only if every condition holds.

```mermaid
flowchart LR
    h{"healthy?<br/>(last /health was 200,<br/>no connection error since)"}
    d{"not draining?"}
    w{"weight > 0?"}
    b{"breaker closed,<br/>or half-open with<br/>no trial out?"}
    yes(["available"])
    no(["skipped"])
    h -- yes --> d -- yes --> w -- yes --> b -- yes --> yes
    h -- no --> no
    d -- no --> no
    w -- no --> no
    b -- no --> no
```

## 8. Health checks

```mermaid
sequenceDiagram
    participant T as Ticker (every 3 s)
    participant HC as Health checker
    participant B1 as api-1
    participant B2 as api-2
    participant Pool

    T->>HC: tick
    par probes run in parallel, 2 s timeout each
        HC->>B1: GET /health
        B1-->>HC: 200 OK, X-Backend-Name: api-1
    and
        HC->>B2: GET /health
        B2-->>HC: 503 (or timeout)
    end
    HC->>Pool: api-1 healthy, name api-1
    HC->>Pool: api-2 down
    Note over Pool: state changes are logged:<br/>"backend ... is now DOWN"
```

## 9. Strategies

```mermaid
flowchart TD
    req["Request from client c"] --> which{"Active strategy"}
    which -->|roundrobin| rr["Next backend in turn<br/>(atomic counter)"]
    which -->|weighted| wrr["Smooth weighted round robin:<br/>each pick adds weight to every<br/>backend's score, highest wins,<br/>winner loses the total"]
    which -->|leastconn| lc["Lowest in-flight ÷ weight"]
    which -->|p2c| p2["Two random backends,<br/>keep lower EWMA latency ×<br/>(in-flight + 1) ÷ weight"]
    which -->|hash| ch["Hash c onto a ring of virtual<br/>nodes (40 per unit of weight),<br/>walk clockwise to the first<br/>available backend"]
    rr & wrr & lc & p2 & ch --> chosen(["Chosen backend"])
```

Smooth weighted round robin with weights 5, 1, 1 for a, b, c gives `a a b a c a a`: the heavy backend gets its share without receiving five requests in a row.

## 10. Consistent hashing

```mermaid
flowchart LR
    subgraph ring["Hash ring (sorted points)"]
        direction LR
        p1(("api-1#0")) --> p2(("api-3#7")) --> p3(("api-2#3")) --> p4(("api-1#12")) --> p5(("api-4#1")) --> p6(("api-2#9"))
    end
    c1["client 192.0.2.10"] -.->|"hash lands here"| p2
    c2["client 198.51.100.5"] -.-> p5
    note["Removing api-3 only moves clients<br/>whose hash landed on api-3's points.<br/>Everyone else keeps their backend."]
    p6 -.->|wraps around| p1
```

## 11. Rate limiting (token bucket per client)

```mermaid
flowchart TD
    a(["Request from client c"]) --> off{"Limit set?"}
    off -- "no (0)" --> pass(["Allowed"])
    off -- yes --> refill["tokens = min(burst,<br/>tokens + elapsed × rate)"]
    refill --> enough{"tokens ≥ 1?"}
    enough -- yes --> spend["tokens -= 1"] --> pass
    enough -- no --> deny(["429 + Retry-After: 1"])
```

Buckets idle for more than a minute are dropped, so memory stays bounded.

## 12. Changing backends at runtime

The backend list is copy-on-write. Requests read it without a lock; changes build a new list and swap a pointer.

```mermaid
sequenceDiagram
    participant UI as Status page
    participant API as Admin API
    participant Pool
    participant New as New backend
    participant Req as In-flight requests

    UI->>API: POST /_switchboard/api/backends {url}
    API->>Pool: Add(url)
    Pool->>Pool: lock membership mutex
    Pool->>New: GET /health
    New-->>Pool: 200
    Pool->>Pool: copy list, append, atomic swap
    Pool->>Pool: unlock
    Note over Req: requests already running keep the<br/>old list, new requests see the new one
    API-->>UI: new state
    UI->>API: PATCH /backends/2 {draining: true}
    API->>Pool: SetDraining(2)
    Note over Pool: backend 2 finishes what it has,<br/>takes nothing new
```

## 13. Live status page

```mermaid
sequenceDiagram
    participant Pool
    participant Hub as Event hub
    participant SSE as /api/events handler
    participant Page as Status page (browser)

    Page->>SSE: GET /_switchboard/api/events
    SSE->>Hub: Subscribe()
    loop every finished request
        Pool->>Hub: Publish(event)
    end
    loop every 100 ms
        Hub->>SSE: batch of events (dropped if the page is slow)
        SSE-->>Page: event: calls
    end
    loop every 500 ms
        SSE->>Pool: Snapshot()
        SSE-->>Page: event: state
    end
    Page->>Page: update tables, share bars,<br/>charts and request log
```

## 14. Fault injection

```mermaid
sequenceDiagram
    participant Page as Status page
    participant API as Admin API (Switchboard)
    participant B as Backend api-3
    participant X as Anyone through the proxy

    Page->>API: PUT /api/backends/3/chaos {error_rate: 1}
    API->>B: POST /_chaos (from localhost, no X-Forwarded-For)
    B-->>API: new fault settings
    API-->>Page: ok
    X->>B: POST /_chaos via the proxy
    B-->>X: 403 (has X-Forwarded-For)
    Note over B: Faults can only be changed locally,<br/>so traffic through Switchboard<br/>cannot break its own backends
```

## 15. Shop cart across backends

The demo shop keeps the cart in a cookie, so any backend can serve any page.

```mermaid
sequenceDiagram
    participant U as Browser
    participant SB as Switchboard (round robin)
    participant A as api-1
    participant B as api-2
    participant C as api-3

    U->>SB: GET /cart/add?id=3
    SB->>A: forward
    A-->>U: 303 to /cart, Set-Cookie: cart=3
    U->>SB: GET /cart (cookie cart=3)
    SB->>B: forward
    B-->>U: cart page with 1 bottle, "Served by api-2"
    U->>SB: GET /cart/add?id=5 (cookie cart=3)
    SB->>C: forward
    C-->>U: 303, Set-Cookie: cart=3.5
```

## 16. Local deployments

Two independent deployments run side by side on one machine.

```mermaid
flowchart TB
    subgraph d1["Deployment 1 (status page version)"]
        s1["switchboard :8080"] --> a1["api-1 :9001"] & a2["api-2 :9002"] & a3["api-3 :9003"] & a4["api-4 :9004"]
    end
    subgraph d2["Deployment 2 (shop version)"]
        s2["switchboard :8081"] --> c1["api-1 :9101"] & c2["api-2 :9102"] & c3["api-3 :9103"] & c4["api-4 :9104"]
    end
    you["Browser"] -->|"localhost:8080"| s1
    you -->|"localhost:8081"| s2
```
