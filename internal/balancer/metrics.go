package balancer

import (
	"sync"
	"sync/atomic"
	"time"
)

// counters tracks pool-wide totals and a per-second request history.
type counters struct {
	requests atomic.Int64
	errors   atomic.Int64 // 5xx returned to clients, including our own 502/503
	limited  atomic.Int64
	retries  atomic.Int64

	mu      sync.Mutex
	secs    [120]int64
	secTime [120]int64
}

func (c *counters) tick(now time.Time) {
	s := now.Unix()
	i := s % int64(len(c.secs))
	c.mu.Lock()
	if c.secTime[i] != s {
		c.secTime[i], c.secs[i] = s, 0
	}
	c.secs[i]++
	c.mu.Unlock()
}

// history returns requests per second for the last n whole seconds, oldest first.
func (c *counters) history(now time.Time, n int) []int64 {
	out := make([]int64, n)
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range n {
		s := now.Unix() - int64(n-k)
		i := s % int64(len(c.secs))
		if c.secTime[i] == s {
			out[k] = c.secs[i]
		}
	}
	return out
}
