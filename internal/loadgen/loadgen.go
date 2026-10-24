// Package loadgen sends a steady stream of requests at a target, pretending
// to be many different clients.
package loadgen

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sync/atomic"
	"time"
)

// Callers are addresses from the documentation ranges (RFC 5737), so they
// can never be mistaken for real clients.
var Callers = func() []string {
	var out []string
	for i, prefix := range []string{"192.0.2.", "198.51.100.", "203.0.113."} {
		for j := range 12 {
			out = append(out, fmt.Sprintf("%s%d", prefix, 10+j*17+i*5))
		}
	}
	return out
}()

var paths = []string{
	"/", "/", "/", "/api/orders", "/api/orders/1042", "/api/users/7",
	"/api/cart", "/search?q=switchboard", "/static/app.js", "/static/logo.svg",
}

type Generator struct {
	target string
	rps    atomic.Int64
	client *http.Client
}

func New(target string) *Generator {
	return &Generator{
		target: target,
		client: &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{MaxIdleConnsPerHost: 256},
		},
	}
}

const MaxRPS = 500

func (g *Generator) SetRPS(n int64) { g.rps.Store(max(0, min(n, MaxRPS))) }
func (g *Generator) RPS() int64     { return g.rps.Load() }

// Run sends requests at the current rate until ctx ends.
func (g *Generator) Run(ctx context.Context) {
	const tick = 20 * time.Millisecond
	t := time.NewTicker(tick)
	defer t.Stop()
	sem := make(chan struct{}, 512)
	var debt float64
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		debt += float64(g.rps.Load()) * tick.Seconds()
		for ; debt >= 1; debt-- {
			select {
			case sem <- struct{}{}:
				go func() {
					defer func() { <-sem }()
					g.send(ctx)
				}()
			default: // too many outstanding; shed rather than queue
			}
		}
	}
}

func (g *Generator) send(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, "GET", g.target+paths[rand.IntN(len(paths))], nil)
	if err != nil {
		return
	}
	req.Header.Set("X-Forwarded-For", Callers[rand.IntN(len(Callers))])
	req.Header.Set("User-Agent", "switchboard-loadgen")
	resp, err := g.client.Do(req)
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}
