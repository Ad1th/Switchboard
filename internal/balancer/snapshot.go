package balancer

import (
	"fmt"
	"io"
	"math"
	"time"
)

type BackendStatus struct {
	ID       string       `json:"id"`
	URL      string       `json:"url"`
	Weight   int64        `json:"weight"`
	Healthy  bool         `json:"healthy"`
	Draining bool         `json:"draining"`
	Breaker  BreakerState `json:"breaker"`
	Active   int64        `json:"active"`
	Served   int64        `json:"served"`
	Failed   int64        `json:"failed"`
	EWMA     float64      `json:"ewma_ms"`
	P50      float64      `json:"p50_ms"`
	P95      float64      `json:"p95_ms"`
	P99      float64      `json:"p99_ms"`
}

type Snapshot struct {
	Strategy   string          `json:"strategy"`
	Strategies []string        `json:"strategies"`
	Uptime     float64         `json:"uptime_s"`
	Requests   int64           `json:"requests"`
	Errors     int64           `json:"errors"`
	Limited    int64           `json:"limited"`
	Retries    int64           `json:"retries"`
	RPS        []int64         `json:"rps"` // last 60 seconds, oldest first
	RateLimit  float64         `json:"rate_limit"`
	RateBurst  float64         `json:"rate_burst"`
	Backends   []BackendStatus `json:"backends"`
}

func round(x float64) float64 { return math.Round(x*10) / 10 }

func (b *Backend) Status(now time.Time) BackendStatus {
	ps := b.lat.percentiles(0.5, 0.95, 0.99)
	return BackendStatus{
		ID:       b.ID,
		URL:      b.URL.String(),
		Weight:   b.Weight(),
		Healthy:  b.healthy.Load(),
		Draining: b.draining.Load(),
		Breaker:  b.breaker.State(now),
		Active:   b.active.Load(),
		Served:   b.served.Load(),
		Failed:   b.failed.Load(),
		EWMA:     round(b.ewma()),
		P50:      round(ps[0]),
		P95:      round(ps[1]),
		P99:      round(ps[2]),
	}
}

func (p *Pool) Snapshot() Snapshot {
	now := time.Now()
	rate, burst := p.limiter.Limits()
	s := Snapshot{
		Strategy:   p.StrategyName(),
		Strategies: StrategyNames(),
		Uptime:     math.Round(now.Sub(p.started).Seconds()),
		Requests:   p.stats.requests.Load(),
		Errors:     p.stats.errors.Load(),
		Limited:    p.stats.limited.Load(),
		Retries:    p.stats.retries.Load(),
		RPS:        p.stats.history(now, 60),
		RateLimit:  rate,
		RateBurst:  burst,
	}
	for _, b := range p.Backends() {
		s.Backends = append(s.Backends, b.Status(now))
	}
	return s
}

// WriteMetrics writes the pool's metrics in the Prometheus text format.
func (p *Pool) WriteMetrics(w io.Writer) {
	s := p.Snapshot()
	fmt.Fprintf(w, "# TYPE switchboard_requests_total counter\nswitchboard_requests_total %d\n", s.Requests)
	fmt.Fprintf(w, "# TYPE switchboard_errors_total counter\nswitchboard_errors_total %d\n", s.Errors)
	fmt.Fprintf(w, "# TYPE switchboard_rate_limited_total counter\nswitchboard_rate_limited_total %d\n", s.Limited)
	fmt.Fprintf(w, "# TYPE switchboard_retries_total counter\nswitchboard_retries_total %d\n", s.Retries)
	gauge := func(name, typ string, val func(BackendStatus) float64) {
		fmt.Fprintf(w, "# TYPE switchboard_backend_%s %s\n", name, typ)
		for _, b := range s.Backends {
			fmt.Fprintf(w, "switchboard_backend_%s{backend=%q} %g\n", name, b.URL, val(b))
		}
	}
	b2f := func(v bool) float64 {
		if v {
			return 1
		}
		return 0
	}
	gauge("up", "gauge", func(b BackendStatus) float64 { return b2f(b.Healthy) })
	gauge("breaker_open", "gauge", func(b BackendStatus) float64 { return b2f(b.Breaker != BreakerClosed) })
	gauge("active", "gauge", func(b BackendStatus) float64 { return float64(b.Active) })
	gauge("served_total", "counter", func(b BackendStatus) float64 { return float64(b.Served) })
	gauge("failed_total", "counter", func(b BackendStatus) float64 { return float64(b.Failed) })
	gauge("latency_p95_ms", "gauge", func(b BackendStatus) float64 { return b.P95 })
}
