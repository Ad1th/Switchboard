// Command switchboard is a reverse proxy and load balancer.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Ad1th/Switchboard/internal/balancer"
)

type config struct {
	Addr           string   `json:"addr"`
	Strategy       string   `json:"strategy"`
	HealthInterval string   `json:"health_interval"`
	Backends       []string `json:"backends"`
}

func loadConfig() (config, error) {
	cfg := config{
		Addr:           ":8080",
		Strategy:       string(balancer.RoundRobin),
		HealthInterval: "3s",
		Backends:       []string{"http://localhost:9001", "http://localhost:9002", "http://localhost:9003"},
	}

	path := flag.String("config", "", "path to a JSON config file")
	addr := flag.String("addr", "", "listen address (default :8080)")
	strategy := flag.String("strategy", "", "roundrobin or leastconn")
	backends := flag.String("backends", "", "comma-separated backend URLs")
	flag.Parse()

	if *path != "" {
		data, err := os.ReadFile(*path)
		if err != nil {
			return cfg, err
		}
		cfg.Backends = nil // the file replaces the defaults, not extends them
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("%s: %w", *path, err)
		}
	}
	if *addr != "" {
		cfg.Addr = *addr
	}
	if *strategy != "" {
		cfg.Strategy = *strategy
	}
	if *backends != "" {
		cfg.Backends = strings.Split(*backends, ",")
	}
	return cfg, nil
}

var dashboard = template.Must(template.New("d").Parse(`<!doctype html>
<meta charset="utf-8"><meta http-equiv="refresh" content="2">
<title>Switchboard</title>
<style>
body{font:15px system-ui;margin:2rem auto;max-width:42rem;padding:0 1rem}
table{border-collapse:collapse;width:100%}td,th{padding:.5rem;border-bottom:1px solid #8884;text-align:left}
.up{color:#1a7f37}.down{color:#cf222e}
</style>
<h1>⚡ Switchboard</h1>
<p>strategy: <b>{{.Strategy}}</b></p>
<table><tr><th>backend<th>state<th>active<th>served<th>failed</tr>
{{range .Backends}}<tr><td>{{.URL}}<td class="{{if .Alive}}up">UP{{else}}down">DOWN{{end}}<td>{{.Active}}<td>{{.Served}}<td>{{.Failed}}</tr>
{{end}}</table>
<p><a href="/">send a request</a> · <a href="/_switchboard/status.json">json</a></p>
`))

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	interval, err := time.ParseDuration(cfg.HealthInterval)
	if err != nil || interval <= 0 {
		log.Fatalf("invalid health_interval %q", cfg.HealthInterval)
	}
	pool, err := balancer.New(cfg.Backends, balancer.Strategy(cfg.Strategy))
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go pool.RunHealthChecks(ctx, interval)

	mux := http.NewServeMux()
	mux.Handle("/", pool)
	mux.HandleFunc("/_switchboard/", func(w http.ResponseWriter, r *http.Request) {
		dashboard.Execute(w, map[string]any{"Strategy": pool.Strategy(), "Backends": pool.Status()})
	})
	mux.HandleFunc("/_switchboard/status.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pool.Status())
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("⚡ Switchboard on %s (%s, %d backends)", cfg.Addr, cfg.Strategy, len(cfg.Backends))
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
