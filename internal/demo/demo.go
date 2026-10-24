// Package demo provides a backend server with adjustable faults, for trying
// Switchboard out and for watching it react.
package demo

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"html/template"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
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
	served          atomic.Int64
	started         time.Time
	srv             *http.Server
}

// Palette gives each backend a colour so its page and its row in the
// Switchboard panel can be matched at a glance.
var Palette = []string{"#1f77b4", "#d62728", "#2ca02c", "#9467bd", "#ff7f0e", "#17becf", "#8c564b", "#e377c2"}

// Color picks a backend's colour: by its trailing number if it has one
// (api-1, api-2, ... get distinct colours), otherwise by a hash of the name.
// internal/web/static/app.js implements the same rule.
func Color(name string) string {
	i := len(name)
	for i > 0 && name[i-1] >= '0' && name[i-1] <= '9' {
		i--
	}
	if n, err := strconv.Atoi(name[i:]); err == nil && n > 0 {
		return Palette[(n-1)%len(Palette)]
	}
	h := fnv.New32a()
	h.Write([]byte(name))
	return Palette[h.Sum32()%uint32(len(Palette))]
}

// Start listens on addr (use "127.0.0.1:0" for a free port) and serves in the
// background.
func Start(name, addr string, chaos Chaos) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{Name: name, URL: "http://" + ln.Addr().String(), started: time.Now()}
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
		w.Header().Set("X-Backend-Name", s.Name)
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
		n := s.served.Add(1)
		w.Header().Set("X-Backend-Name", s.Name)
		if strings.Contains(r.Header.Get("Accept"), "text/html") {
			s.shop(w, r, n)
			return
		}
		fmt.Fprintf(w, "Hello from %s (port %s, pid %d)\n", s.Name, s.port(), os.Getpid())
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

func (s *Server) port() string {
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(s.URL, "http://"))
	return port
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Name}}</title>
<link rel="icon" href="data:,">
<style>
body { margin: 0; font: 15px/1.5 -apple-system, system-ui, "Segoe UI", Roboto, sans-serif; color: #222; background: #fff; }
.bar { background: {{.Color}}; color: #fff; padding: 28px 32px; }
h1 { margin: 0; font-size: 44px; line-height: 1.1; }
.bar p { margin: 6px 0 0; opacity: .9; }
table { border-collapse: collapse; margin: 24px 32px; }
th, td { text-align: left; padding: 5px 16px 5px 0; border-bottom: 1px solid #e5e5e5; vertical-align: top; }
th { font-weight: 600; color: #555; }
td { font-family: ui-monospace, Menlo, Consolas, monospace; font-size: 14px; }
footer { margin: 0 32px 32px; color: #666; font-size: 14px; }
@media (prefers-color-scheme: dark) {
  body { background: #161616; color: #ddd; }
  th, td { border-color: #333; } th { color: #aaa; } footer { color: #999; }
}
</style></head>
<body>
<div class="bar"><h1>{{.Name}}</h1><p>This response was served by {{.Name}} on port {{.Port}}.</p></div>
<table>
<tr><th>Backend</th><td>{{.Name}}</td></tr>
<tr><th>Listening on</th><td>{{.Addr}}</td></tr>
<tr><th>Process ID</th><td>{{.PID}}</td></tr>
<tr><th>Host</th><td>{{.Host}}</td></tr>
<tr><th>Up for</th><td>{{.Uptime}}</td></tr>
<tr><th>Requests served</th><td>{{.Served}}</td></tr>
<tr><th>Request</th><td>{{.Method}} {{.Path}}</td></tr>
<tr><th>Client (X-Forwarded-For)</th><td>{{.Client}}</td></tr>
<tr><th>Received via (X-Forwarded-Host)</th><td>{{.Via}}</td></tr>
<tr><th>Time</th><td>{{.Time}}</td></tr>
</table>
<footer><a href="/">Back to the shop</a>. Reload this page to send another request through the load balancer.</footer>
</body></html>`))

func (s *Server) page(w http.ResponseWriter, r *http.Request, served int64) {
	host, _ := os.Hostname()
	client := r.Header.Get("X-Forwarded-For")
	if client == "" {
		client = r.RemoteAddr + " (direct, not through the proxy)"
	}
	via := r.Header.Get("X-Forwarded-Host")
	if via == "" {
		via = "direct"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	pageTmpl.Execute(w, map[string]any{
		"Name":   s.Name,
		"Color":  template.CSS(Color(s.Name)),
		"Port":   s.port(),
		"Addr":   strings.TrimPrefix(s.URL, "http://"),
		"PID":    os.Getpid(),
		"Host":   host,
		"Uptime": time.Since(s.started).Round(time.Second).String(),
		"Served": served,
		"Method": r.Method,
		"Path":   r.URL.RequestURI(),
		"Client": client,
		"Via":    via,
		"Time":   time.Now().Format("15:04:05.000"),
	})
}
