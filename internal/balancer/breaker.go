package balancer

import (
	"sync"
	"time"
)

type BreakerState string

const (
	BreakerClosed   BreakerState = "closed"
	BreakerOpen     BreakerState = "open"
	BreakerHalfOpen BreakerState = "half-open"
)

// breaker opens after threshold consecutive failures, waits cooldown, then
// lets a single trial request through. The trial decides whether it closes
// again or goes back to open.
type breaker struct {
	mu        sync.Mutex
	state     BreakerState
	fails     int
	openedAt  time.Time
	trialOut  bool
	threshold int
	cooldown  time.Duration
}

func (b *breaker) init(threshold int, cooldown time.Duration) {
	b.state, b.threshold, b.cooldown = BreakerClosed, threshold, cooldown
}

// ready reports, without side effects, whether acquire could succeed.
func (b *breaker) ready(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case BreakerOpen:
		return now.Sub(b.openedAt) >= b.cooldown
	case BreakerHalfOpen:
		return !b.trialOut
	}
	return true
}

// acquire claims permission to send one request.
func (b *breaker) acquire(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case BreakerOpen:
		if now.Sub(b.openedAt) < b.cooldown {
			return false
		}
		b.state = BreakerHalfOpen
		fallthrough
	case BreakerHalfOpen:
		if b.trialOut {
			return false
		}
		b.trialOut = true
	}
	return true
}

// record reports the outcome of a request that acquire allowed.
func (b *breaker) record(ok bool, now time.Time) (changed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	before := b.state
	switch {
	case ok:
		b.fails = 0
		b.state = BreakerClosed
	case b.state == BreakerHalfOpen:
		b.state, b.openedAt = BreakerOpen, now
	default:
		b.fails++
		if b.fails >= b.threshold {
			b.state, b.openedAt = BreakerOpen, now
		}
	}
	b.trialOut = false
	return before != b.state
}

// release gives back a trial whose outcome is unknown, e.g. the client left.
func (b *breaker) release() {
	b.mu.Lock()
	b.trialOut = false
	b.mu.Unlock()
}

func (b *breaker) State(now time.Time) BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == BreakerOpen && now.Sub(b.openedAt) >= b.cooldown {
		return BreakerHalfOpen
	}
	return b.state
}
