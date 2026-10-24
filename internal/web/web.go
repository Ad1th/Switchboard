// Package web serves Switchboard's control panel and admin API under
// /_switchboard/.
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sync"
	"time"

	"github.com/Ad1th/Switchboard/internal/balancer"
	"github.com/Ad1th/Switchboard/internal/demo"
	"github.com/Ad1th/Switchboard/internal/loadgen"
)

//go:embed static
var static embed.FS

type Server struct {
	Pool    *balancer.Pool
	Loadgen *loadgen.Generator // nil disables the traffic control
	Fleet   *Fleet             // nil disables installing demo lines
}

// Fleet owns the demo backends that run inside the Switchboard process.
type Fleet struct {
	mu      sync.Mutex
	servers map[string]*demo.Server // keyed by URL
	spawned int
}

func NewFleet() *Fleet { return &Fleet{servers: map[string]*demo.Server{}} }

func (f *Fleet) Spawn(chaos demo.Chaos) (*demo.Server, error) {
	f.mu.Lock()
	f.spawned++
	name := fmt.Sprintf("api-%d", f.spawned)
	f.mu.Unlock()
	s, err := demo.Start(name, "127.0.0.1:0", chaos)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.servers[s.URL] = s
	f.mu.Unlock()
	return s, nil
}

func (f *Fleet) Has(url string) bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.servers[url] != nil
}

func (f *Fleet) Stop(url string) {
	f.mu.Lock()
	s := f.servers[url]
	delete(f.servers, url)
	f.mu.Unlock()
	if s != nil {
		s.Close()
	}
}

func (f *Fleet) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.servers {
		s.Close()
	}
}

type state struct {
	balancer.Snapshot
	Backends   []backendState `json:"backends"`
	LoadgenRPS int64          `json:"loadgen_rps"`
	LoadgenMax int64          `json:"loadgen_max"`
	HasLoadgen bool           `json:"has_loadgen"`
	CanInstall bool           `json:"can_install"`
}

type backendState struct {
	balancer.BackendStatus
	Demo bool `json:"demo"`
}

func (s *Server) state() state {
	st := state{
		Snapshot:   s.Pool.Snapshot(),
		HasLoadgen: s.Loadgen != nil,
		LoadgenMax: loadgen.MaxRPS,
		CanInstall: s.Fleet != nil,
	}
	if s.Loadgen != nil {
		st.LoadgenRPS = s.Loadgen.RPS()
	}
	for _, b := range st.Snapshot.Backends {
		st.Backends = append(st.Backends, backendState{b, s.Fleet.Has(b.URL)})
	}
	return st
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(v)
}

func (s *Server) Register(mux *http.ServeMux) {
	assets, _ := fs.Sub(static, "static")
	mux.HandleFunc("GET /_switchboard/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, assets, "index.html")
	})
	mux.Handle("GET /_switchboard/assets/", http.StripPrefix("/_switchboard/assets/", http.FileServerFS(assets)))
	mux.Handle("GET /_switchboard", http.RedirectHandler("/_switchboard/", http.StatusMovedPermanently))

	mux.HandleFunc("GET /_switchboard/api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.state())
	})
	mux.HandleFunc("GET /_switchboard/status.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.Pool.Snapshot().Backends)
	})
	mux.HandleFunc("GET /_switchboard/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		s.Pool.WriteMetrics(w)
	})
	mux.HandleFunc("GET /_switchboard/api/events", s.events)

	mux.HandleFunc("PUT /_switchboard/api/strategy", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Name string }
		if err := decode(r, &body); err != nil {
			fail(w, 400, err)
			return
		}
		if err := s.Pool.SetStrategy(body.Name); err != nil {
			fail(w, 400, err)
			return
		}
		writeJSON(w, 200, s.state())
	})

	mux.HandleFunc("POST /_switchboard/api/backends", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			URL    string
			Name   string
			Weight int64
			Demo   bool
		}
		if err := decode(r, &body); err != nil {
			fail(w, 400, err)
			return
		}
		if body.Demo {
			if s.Fleet == nil {
				fail(w, 400, errors.New("demo lines are only available with -demo"))
				return
			}
			d, err := s.Fleet.Spawn(demo.Chaos{LatencyMs: 20, JitterMs: 40})
			if err != nil {
				fail(w, 500, err)
				return
			}
			body.URL, body.Name = d.URL, d.Name
		}
		if _, err := s.Pool.Add(balancer.BackendConfig{URL: body.URL, Name: body.Name, Weight: body.Weight}); err != nil {
			if body.Demo {
				s.Fleet.Stop(body.URL)
			}
			fail(w, 400, err)
			return
		}
		writeJSON(w, 201, s.state())
	})

	mux.HandleFunc("PATCH /_switchboard/api/backends/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Weight   *int64
			Draining *bool
		}
		if err := decode(r, &body); err != nil {
			fail(w, 400, err)
			return
		}
		id := r.PathValue("id")
		var err error
		if body.Weight != nil {
			err = s.Pool.SetWeight(id, *body.Weight)
		}
		if body.Draining != nil && err == nil {
			err = s.Pool.SetDraining(id, *body.Draining)
		}
		if err != nil {
			fail(w, 400, err)
			return
		}
		writeJSON(w, 200, s.state())
	})

	mux.HandleFunc("DELETE /_switchboard/api/backends/{id}", func(w http.ResponseWriter, r *http.Request) {
		b, err := s.Pool.Remove(r.PathValue("id"))
		if err != nil {
			fail(w, 400, err)
			return
		}
		if s.Fleet.Has(b.URL.String()) {
			// Let in-flight requests finish before the server goes away.
			go func() {
				time.Sleep(15 * time.Second)
				s.Fleet.Stop(b.URL.String())
			}()
		}
		writeJSON(w, 200, s.state())
	})

	mux.HandleFunc("GET /_switchboard/api/backends/{id}/chaos", func(w http.ResponseWriter, r *http.Request) {
		s.chaos(w, r, nil)
	})
	mux.HandleFunc("PUT /_switchboard/api/backends/{id}/chaos", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
		if err != nil {
			fail(w, 400, err)
			return
		}
		s.chaos(w, r, body)
	})

	mux.HandleFunc("PUT /_switchboard/api/loadgen", func(w http.ResponseWriter, r *http.Request) {
		if s.Loadgen == nil {
			fail(w, 400, errors.New("traffic generator is only available with -demo"))
			return
		}
		var body struct{ RPS int64 }
		if err := decode(r, &body); err != nil {
			fail(w, 400, err)
			return
		}
		s.Loadgen.SetRPS(body.RPS)
		writeJSON(w, 200, s.state())
	})

	mux.HandleFunc("PUT /_switchboard/api/ratelimit", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Rate, Burst float64 }
		if err := decode(r, &body); err != nil {
			fail(w, 400, err)
			return
		}
		if body.Rate < 0 || body.Rate > 10000 {
			fail(w, 400, errors.New("rate must be between 0 and 10000"))
			return
		}
		s.Pool.SetRateLimit(body.Rate, body.Burst)
		writeJSON(w, 200, s.state())
	})
}

var chaosClient = &http.Client{Timeout: 3 * time.Second}

// chaos relays a read or change of fault settings to the backend's /_chaos
// endpoint. It works for any backend that runs the demo server.
func (s *Server) chaos(w http.ResponseWriter, r *http.Request, body []byte) {
	b := s.Pool.Get(r.PathValue("id"))
	if b == nil {
		fail(w, 404, fmt.Errorf("no backend %q", r.PathValue("id")))
		return
	}
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
	}
	req, _ := http.NewRequestWithContext(r.Context(), method, b.URL.JoinPath("_chaos").String(), bytes.NewReader(body))
	resp, err := chaosClient.Do(req)
	if err != nil {
		fail(w, 502, fmt.Errorf("backend unreachable: %w", err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		fail(w, resp.StatusCode, fmt.Errorf("backend does not accept fault controls: %s", bytes.TrimSpace(msg)))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.Copy(w, resp.Body)
}

// events streams state snapshots twice a second and batches of finished
// requests as they happen, as server-sent events.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	calls, unsubscribe := s.Pool.Hub().Subscribe()
	defer unsubscribe()
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()

	send := func(event string, data []byte) bool {
		rc.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	snapshot := func() bool {
		data, _ := json.Marshal(s.state())
		return send("state", data)
	}
	if !snapshot() {
		return
	}
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !snapshot() {
				return
			}
		case batch := <-calls:
			if !send("calls", batch) {
				return
			}
		}
	}
}
