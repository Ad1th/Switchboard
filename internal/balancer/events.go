package balancer

import (
	"encoding/json"
	"sync"
	"time"
)

// RequestEvent describes one finished request, for the live dashboard.
type RequestEvent struct {
	T        int64   `json:"t"` // unix milliseconds when it finished
	Client   string  `json:"client"`
	Method   string  `json:"method"`
	Path     string  `json:"path"`
	Backend  string  `json:"backend"` // backend ID, empty if none was reached
	Status   int     `json:"status"`
	Ms       float64 `json:"ms"`
	Attempts int     `json:"attempts"`
}

// Hub batches events and fans them out to subscribers. Slow subscribers
// miss batches instead of slowing the proxy down.
type Hub struct {
	mu      sync.Mutex
	pending []RequestEvent
	subs    map[chan []byte]struct{}
}

func NewHub() *Hub { return &Hub{subs: map[chan []byte]struct{}{}} }

func (h *Hub) Publish(ev RequestEvent) {
	h.mu.Lock()
	if len(h.subs) > 0 && len(h.pending) < 2000 {
		h.pending = append(h.pending, ev)
	}
	h.mu.Unlock()
}

func (h *Hub) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 16)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

func (h *Hub) run(stop <-chan struct{}) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		h.mu.Lock()
		batch := h.pending
		h.pending = nil
		subs := make([]chan []byte, 0, len(h.subs))
		for ch := range h.subs {
			subs = append(subs, ch)
		}
		h.mu.Unlock()
		if len(batch) == 0 {
			continue
		}
		data, _ := json.Marshal(batch)
		for _, ch := range subs {
			select {
			case ch <- data:
			default:
			}
		}
	}
}
