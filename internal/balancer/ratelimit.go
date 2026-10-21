package balancer

import (
	"sync"
	"time"
)

// RateLimiter is a token bucket per client. A rate of zero disables it.
type RateLimiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*bucket
	calls   int
}

type bucket struct {
	tokens float64
	last   time.Time
}

func NewRateLimiter(rate, burst float64) *RateLimiter {
	l := &RateLimiter{buckets: map[string]*bucket{}}
	l.Set(rate, burst)
	return l
}

func (l *RateLimiter) Set(rate, burst float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if burst < 1 {
		burst = max(1, rate)
	}
	l.rate, l.burst = rate, burst
	clear(l.buckets)
}

func (l *RateLimiter) Limits() (rate, burst float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rate, l.burst
}

func (l *RateLimiter) Allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rate <= 0 {
		return true
	}
	if l.calls++; l.calls%4096 == 0 {
		for k, b := range l.buckets {
			if now.Sub(b.last) > time.Minute {
				delete(l.buckets, k)
			}
		}
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
