// Package demo provides a backend server with adjustable faults, for trying
// Switchboard out and for watching it react.
package demo

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Chaos is the fault configuration of a demo backend.
type Chaos struct {
	LatencyMs int     `json:"latency_ms"` // added to every request
	JitterMs  int     `json:"jitter_ms"`  // random extra latency, up to this
	ErrorRate float64 `json:"error_rate"` // fraction of requests answered with 500
	Down      bool    `json:"down"`       // fail health checks and drop connections
}

type Server struct {
	Name string
	URL  string

	latency, jitter atomic.Int64
	errorRate       atomic.Uint64
	down            atomic.Bool
	srv             *http.Server
}

// Start listens on addr (use "127.0.0.1:0" for a free port) and serves in the
// background.
func Start(name, addr string, chaos Chaos) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{Name: name, URL: "http://" + ln.Addr().String()}
	s.SetChaos(chaos)
	s.srv = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go s.srv.Serve(ln)
	return s, nil
}

func (s *Server) Close() error { return s.srv.Close() }

func (s *Server) Chaos() Chaos {
	return Chaos{
		LatencyMs: int(s.latency.Load()),
		JitterMs:  int(s.jitter.Load()),
		ErrorRate: math.Float64frombits(s.errorRate.Load()),
		Down:      s.down.Load(),
	}
}

func (s *Server) SetChaos(c Chaos) {
	s.latency.Store(int64(max(0, min(c.LatencyMs, 10000))))
	s.jitter.Store(int64(max(0, min(c.JitterMs, 10000))))
	s.errorRate.Store(math.Float64bits(max(0, min(c.ErrorRate, 1))))
	s.down.Store(c.Down)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if s.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "OK")
	})
	mux.HandleFunc("/_chaos", s.serveChaos)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if s.down.Load() {
			// Behave like a crashed process: drop the connection unanswered.
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
					return
				}
			}
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		delay := s.latency.Load()
		if j := s.jitter.Load(); j > 0 {
			delay += rand.Int64N(j + 1)
		}
		select {
		case <-time.After(time.Duration(delay) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		if rand.Float64() < math.Float64frombits(s.errorRate.Load()) {
			http.Error(w, "Line noise on "+s.Name, http.StatusInternalServerError)
			return
		}
		w.Header().Set("X-Backend", s.Name)
		fmt.Fprintf(w, "Hello from %s\n", s.Name)
	})
	return mux
}

// serveChaos reads or changes the fault settings. Changes are accepted only
// from the local machine and never through a proxy, so traffic sent through
// Switchboard cannot break its own backends.
func (s *Server) serveChaos(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() || r.Header.Get("X-Forwarded-For") != "" {
			http.Error(w, "chaos can only be changed locally", http.StatusForbidden)
			return
		}
		c := s.Chaos()
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&c); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.SetChaos(c)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.Chaos())
}
