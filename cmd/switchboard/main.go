// Command switchboard is a reverse proxy and load balancer with a live
// control panel.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Ad1th/Switchboard/internal/balancer"
	"github.com/Ad1th/Switchboard/internal/demo"
	"github.com/Ad1th/Switchboard/internal/loadgen"
	"github.com/Ad1th/Switchboard/internal/web"
)

type config struct {
	Addr             string    `json:"addr"`
	Strategy         string    `json:"strategy"`
	HealthInterval   string    `json:"health_interval"`
	BreakerThreshold int       `json:"breaker_threshold"`
	BreakerCooldown  string    `json:"breaker_cooldown"`
	RateLimit        float64   `json:"rate_limit"`
	RateBurst        float64   `json:"rate_burst"`
	TrustForwarded   bool      `json:"trust_forwarded"`
	AdminToken       string    `json:"admin_token"`
	Backends         []backend `json:"backends"`
}

// backend accepts either "http://host:port" or {"url": ..., "weight": n}.
type backend balancer.BackendConfig

func (b *backend) UnmarshalJSON(data []byte) error {
	var s string
	if json.Unmarshal(data, &s) == nil {
		*b = backend{URL: s}
		return nil
	}
	var obj struct {
		URL    string `json:"url"`
		Weight int64  `json:"weight"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*b = backend{URL: obj.URL, Weight: obj.Weight}
	return nil
}

func loadConfig() (config, int, error) {
	cfg := config{
		Addr:            ":8080",
		Strategy:        "roundrobin",
		HealthInterval:  "3s",
		BreakerCooldown: "5s",
		Backends:        []backend{{URL: "http://localhost:9001"}, {URL: "http://localhost:9002"}, {URL: "http://localhost:9003"}},
	}

	path := flag.String("config", "", "path to a JSON config file")
	addr := flag.String("addr", "", "listen address (default :8080)")
	strategy := flag.String("strategy", "", strings.Join(balancer.StrategyNames(), ", "))
	backends := flag.String("backends", "", "comma-separated backend URLs")
	rate := flag.Float64("rate-limit", -1, "requests per second per client, 0 for none")
	token := flag.String("admin-token", "", "require this token to change settings from the control panel")
	demoLines := flag.Int("demo", 0, "start this many built-in demo backends and the traffic generator")
	flag.Parse()

	if *path != "" {
		data, err := os.ReadFile(*path)
		if err != nil {
			return cfg, 0, err
		}
		cfg.Backends = nil // the file replaces the defaults, not extends them
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, 0, fmt.Errorf("%s: %w", *path, err)
		}
	}
	if *addr != "" {
		cfg.Addr = *addr
	}
	if *strategy != "" {
		cfg.Strategy = *strategy
	}
	if *backends != "" {
		cfg.Backends = nil
		for _, u := range strings.Split(*backends, ",") {
			cfg.Backends = append(cfg.Backends, backend{URL: u})
		}
	}
	if *rate >= 0 {
		cfg.RateLimit = *rate
	}
	if *token != "" {
		cfg.AdminToken = *token
	}
	if env := os.Getenv("SWITCHBOARD_ADMIN_TOKEN"); env != "" && cfg.AdminToken == "" {
		cfg.AdminToken = env
	}
	return cfg, *demoLines, nil
}

func main() {
	cfg, demoLines, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	interval, err := time.ParseDuration(cfg.HealthInterval)
	if err != nil || interval <= 0 {
		log.Fatalf("invalid health_interval %q", cfg.HealthInterval)
	}
	cooldown, err := time.ParseDuration(cfg.BreakerCooldown)
	if err != nil || cooldown <= 0 {
		log.Fatalf("invalid breaker_cooldown %q", cfg.BreakerCooldown)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ui := &web.Server{AdminToken: cfg.AdminToken}
	if demoLines > 0 {
		// Demo mode: backends run in this process and the traffic generator
		// pretends to be many callers, so trust its X-Forwarded-For.
		ui.Fleet = web.NewFleet()
		defer ui.Fleet.Close()
		cfg.Backends = nil
		cfg.TrustForwarded = true
		for i := range demoLines {
			d, err := ui.Fleet.Spawn(demo.Chaos{LatencyMs: 15 + 10*i, JitterMs: 40})
			if err != nil {
				log.Fatal(err)
			}
			cfg.Backends = append(cfg.Backends, backend{URL: d.URL})
		}
	}

	bcs := make([]balancer.BackendConfig, len(cfg.Backends))
	for i, b := range cfg.Backends {
		bcs[i] = balancer.BackendConfig(b)
	}
	pool, err := balancer.New(bcs, balancer.Options{
		Strategy:         cfg.Strategy,
		BreakerThreshold: cfg.BreakerThreshold,
		BreakerCooldown:  cooldown,
		RateLimit:        cfg.RateLimit,
		RateBurst:        cfg.RateBurst,
		TrustForwarded:   cfg.TrustForwarded,
	})
	if err != nil {
		log.Fatal(err)
	}
	ui.Pool = pool
	go pool.Run(ctx, interval)

	mux := http.NewServeMux()
	mux.Handle("/", pool)
	ui.Register(mux)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		log.Fatal(err)
	}
	if demoLines > 0 {
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		ui.Loadgen = loadgen.New("http://127.0.0.1:" + port)
		go ui.Loadgen.Run(ctx)
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("⚡ Switchboard on %s (%s, %d backends), control panel at /_switchboard/", cfg.Addr, cfg.Strategy, len(cfg.Backends))
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
