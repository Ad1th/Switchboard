package balancer

import (
	"math"
	"net/http/httputil"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Backend is one upstream server ("line") in the pool.
type Backend struct {
	ID  string
	URL *url.URL

	name    atomic.Pointer[string] // from config, or reported by the backend
	nameSet bool                   // name came from config and is not overwritten

	proxy    *httputil.ReverseProxy
	weight   atomic.Int64
	healthy  atomic.Bool
	draining atomic.Bool
	breaker  breaker

	active atomic.Int64
	served atomic.Int64
	failed atomic.Int64

	ewmaBits atomic.Uint64 // float64 milliseconds
	lat      latencyWindow

	cw int64 // smooth weighted round robin state, guarded by weighted.mu
}

func (b *Backend) Weight() int64 { return b.weight.Load() }

// Name is the backend's display name: the configured one, else the one the
// backend reports in X-Backend-Name on /health, else its host:port.
func (b *Backend) Name() string {
	if n := b.name.Load(); n != nil {
		return *n
	}
	return b.URL.Host
}

// Available reports whether the backend may receive new requests.
func (b *Backend) Available(now time.Time) bool {
	return b.healthy.Load() && !b.draining.Load() && b.weight.Load() > 0 && b.breaker.ready(now)
}

func (b *Backend) ewma() float64 { return math.Float64frombits(b.ewmaBits.Load()) }

func (b *Backend) observe(d time.Duration) {
	ms := float64(d.Microseconds()) / 1000
	b.lat.add(ms)
	for {
		old := b.ewmaBits.Load()
		prev := math.Float64frombits(old)
		next := ms
		if prev > 0 {
			next = 0.3*ms + 0.7*prev
		}
		if b.ewmaBits.CompareAndSwap(old, math.Float64bits(next)) {
			return
		}
	}
}

// latencyWindow keeps the most recent samples for percentile estimates.
type latencyWindow struct {
	mu      sync.Mutex
	samples [512]float64
	n, next int
}

func (w *latencyWindow) add(ms float64) {
	w.mu.Lock()
	w.samples[w.next] = ms
	w.next = (w.next + 1) % len(w.samples)
	if w.n < len(w.samples) {
		w.n++
	}
	w.mu.Unlock()
}

func (w *latencyWindow) percentiles(ps ...float64) []float64 {
	w.mu.Lock()
	s := slices.Clone(w.samples[:w.n])
	w.mu.Unlock()
	out := make([]float64, len(ps))
	if len(s) == 0 {
		return out
	}
	slices.Sort(s)
	for i, p := range ps {
		out[i] = s[min(len(s)-1, int(p*float64(len(s))))]
	}
	return out
}
