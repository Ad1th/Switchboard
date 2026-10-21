// Command backend is a demo server for trying out Switchboard. Its faults
// can be changed at runtime from the Switchboard control panel.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"

	"github.com/Ad1th/Switchboard/internal/demo"
)

func main() {
	port := flag.String("port", "9001", "port to listen on")
	latency := flag.Int("latency", 0, "added latency in milliseconds")
	jitter := flag.Int("jitter", 0, "random extra latency in milliseconds")
	errorRate := flag.Float64("error-rate", 0, "fraction of requests answered with 500")
	flag.Parse()

	s, err := demo.Start("backend "+*port, ":"+*port, demo.Chaos{LatencyMs: *latency, JitterMs: *jitter, ErrorRate: *errorRate})
	if err != nil {
		log.Fatal(err)
	}
	log.Println("Backend running on port", *port)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	s.Close()
}
