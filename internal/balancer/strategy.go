package balancer

import (
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Strategy chooses a backend for a request. ok filters out backends that are
// unavailable or were already tried for this request.
type Strategy interface {
	Name() string
	Pick(bs []*Backend, client string, ok func(*Backend) bool) *Backend
}

var strategyNames = []string{"roundrobin", "weighted", "leastconn", "p2c", "hash"}

func StrategyNames() []string { return slices.Clone(strategyNames) }

func NewStrategy(name string) (Strategy, error) {
	switch name {
	case "roundrobin":
		return &roundRobin{}, nil
	case "weighted":
		return &weighted{}, nil
	case "leastconn":
		return leastConn{}, nil
	case "p2c":
		return p2c{}, nil
	case "hash":
		return &consistentHash{}, nil
	}
	return nil, fmt.Errorf("unknown strategy %q (want one of %s)", name, strings.Join(strategyNames, ", "))
}

type roundRobin struct{ next atomic.Uint64 }

func (*roundRobin) Name() string { return "roundrobin" }

func (s *roundRobin) Pick(bs []*Backend, _ string, ok func(*Backend) bool) *Backend {
	n := len(bs)
	if n == 0 {
		return nil
	}
	start := int((s.next.Add(1) - 1) % uint64(n))
	for i := range n {
		if b := bs[(start+i)%n]; ok(b) {
			return b
		}
	}
	return nil
}

// weighted is nginx's smooth weighted round robin: heavier backends get
// proportionally more requests, interleaved rather than in bursts.
type weighted struct{ mu sync.Mutex }

func (*weighted) Name() string { return "weighted" }

func (s *weighted) Pick(bs []*Backend, _ string, ok func(*Backend) bool) *Backend {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *Backend
	var total int64
	for _, b := range bs {
		if !ok(b) {
			continue
		}
		w := b.Weight()
		b.cw += w
		total += w
		if best == nil || b.cw > best.cw {
			best = b
		}
	}
	if best != nil {
		best.cw -= total
	}
	return best
}

type leastConn struct{}

func (leastConn) Name() string { return "leastconn" }

func (leastConn) Pick(bs []*Backend, _ string, ok func(*Backend) bool) *Backend {
	var best *Backend
	var bestScore float64
	for _, b := range bs {
		if !ok(b) {
			continue
		}
		score := float64(b.active.Load()) / float64(b.Weight())
		if best == nil || score < bestScore {
			best, bestScore = b, score
		}
	}
	return best
}

// p2c samples two backends at random and keeps the one with the lower
// expected wait (smoothed latency times queue depth). It reacts to slow
// backends without the herding that pure least-latency causes.
type p2c struct{}

func (p2c) Name() string { return "p2c" }

func (p2c) Pick(bs []*Backend, _ string, ok func(*Backend) bool) *Backend {
	var cands []*Backend
	for _, b := range bs {
		if ok(b) {
			cands = append(cands, b)
		}
	}
	switch len(cands) {
	case 0:
		return nil
	case 1:
		return cands[0]
	}
	i := rand.IntN(len(cands))
	j := rand.IntN(len(cands) - 1)
	if j >= i {
		j++
	}
	a, b := cands[i], cands[j]
	if p2cScore(b) < p2cScore(a) {
		return b
	}
	return a
}

func p2cScore(b *Backend) float64 {
	lat := b.ewma()
	if lat == 0 {
		lat = 1
	}
	return lat * float64(b.active.Load()+1) / float64(b.Weight())
}

// consistentHash pins each client to a backend on a hash ring, so adding or
// removing a backend only moves the clients that hashed near it.
type consistentHash struct {
	mu   sync.Mutex
	key  string
	ring []ringPoint
}

type ringPoint struct {
	hash uint64
	b    *Backend
}

const vnodesPerWeight = 40

func (*consistentHash) Name() string { return "hash" }

func hash64(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	// fnv is weak on short similar keys; finish with a mixer for spread.
	x := h.Sum64()
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	return x
}

func (s *consistentHash) ringFor(bs []*Backend) []ringPoint {
	var sb strings.Builder
	for _, b := range bs {
		sb.WriteString(b.ID + ":" + strconv.FormatInt(b.Weight(), 10) + ",")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if key := sb.String(); key != s.key {
		s.key, s.ring = key, nil
		for _, b := range bs {
			for i := range int(min(b.Weight(), 10)) * vnodesPerWeight {
				s.ring = append(s.ring, ringPoint{hash64(b.ID + "#" + strconv.Itoa(i)), b})
			}
		}
		slices.SortFunc(s.ring, func(x, y ringPoint) int {
			if x.hash < y.hash {
				return -1
			}
			if x.hash > y.hash {
				return 1
			}
			return 0
		})
	}
	return s.ring
}

func (s *consistentHash) Pick(bs []*Backend, client string, ok func(*Backend) bool) *Backend {
	ring := s.ringFor(bs)
	if len(ring) == 0 {
		return nil
	}
	h := hash64(client)
	start, _ := slices.BinarySearchFunc(ring, h, func(p ringPoint, h uint64) int {
		if p.hash < h {
			return -1
		}
		if p.hash > h {
			return 1
		}
		return 0
	})
	for i := range ring {
		if b := ring[(start+i)%len(ring)].b; ok(b) {
			return b
		}
	}
	return nil
}
