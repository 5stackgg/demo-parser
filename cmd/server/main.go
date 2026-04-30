// demo-parser entry point. Two modes, picked by the first arg:
//
//   demo-parser parse       reads .dem bytes from stdin, writes Result
//                           JSON to stdout. Exit non-zero on parse error.
//                           This is the mode the api shells out to.
//
//   demo-parser server      legacy HTTP mode (POST /parse with a JSON
//                           body containing demo_url). Kept for ad-hoc
//                           testing — not the production path.
//
// Default (no arg) is `server` for backwards compatibility with old
// docker images that ENTRYPOINT'd the binary directly.
//
// Why bundled-binary instead of a separate microservice: events
// (kills, rounds, bomb) are small enough to ship with the api
// process. 2D playback would change that calculus — frame data is
// orders of magnitude larger and we'd revisit then.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/5stackgg/demo-parser/internal/parser"
)

type parseRequest struct {
	MatchMapDemoID string `json:"match_map_demo_id"`
	DemoURL        string `json:"demo_url"`
}

func main() {
	mode := "server"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "parse":
		runCLI()
	case "server":
		runServer()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s (want 'parse' or 'server')\n", mode)
		os.Exit(2)
	}
}

// CLI mode: read .dem from stdin, write Result JSON to stdout.
// Errors and progress logs go to stderr so they don't pollute the
// JSON stream the api consumes.
func runCLI() {
	log.SetOutput(os.Stderr)
	log.SetFlags(0)
	log.SetPrefix("[demo-parser] ")

	start := time.Now()
	result, err := parser.Parse(os.Stdin)
	if err != nil {
		log.Printf("parse error: %v", err)
		os.Exit(1)
	}
	log.Printf(
		"parsed in %s: ticks=%d tick_rate=%.1f rounds=%d kills=%d bombs=%d map=%s",
		time.Since(start),
		result.TotalTicks, result.TickRate,
		len(result.RoundTicks), len(result.Kills), len(result.Bombs),
		result.MapName,
	)
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		log.Printf("encode result: %v", err)
		os.Exit(1)
	}
}

func runServer() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/parse", handleParse)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      5 * time.Minute,
	}
	log.Printf("demo-parser listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func handleParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req parseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("bad request: %v", err), http.StatusBadRequest)
		return
	}
	if req.DemoURL == "" {
		http.Error(w, "demo_url required", http.StatusBadRequest)
		return
	}

	logPrefix := req.MatchMapDemoID
	if logPrefix == "" {
		logPrefix = "<no-id>"
	}
	log.Printf("[%s] fetching demo", logPrefix)

	demoResp, err := http.Get(req.DemoURL)
	if err != nil {
		http.Error(w, fmt.Sprintf("fetch demo: %v", err), http.StatusBadGateway)
		return
	}
	defer demoResp.Body.Close()
	if demoResp.StatusCode >= 400 {
		http.Error(w,
			fmt.Sprintf("fetch demo: upstream %d", demoResp.StatusCode),
			http.StatusBadGateway,
		)
		return
	}

	start := time.Now()
	result, err := parser.Parse(demoResp.Body)
	if err != nil {
		log.Printf("[%s] parse error: %v", logPrefix, err)
		http.Error(w, fmt.Sprintf("parse: %v", err), http.StatusUnprocessableEntity)
		return
	}
	log.Printf(
		"[%s] parsed in %s: ticks=%d tick_rate=%.1f rounds=%d map=%s",
		logPrefix, time.Since(start),
		result.TotalTicks, result.TickRate, len(result.RoundTicks), result.MapName,
	)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Printf("[%s] response encode: %v", logPrefix, err)
	}
}
