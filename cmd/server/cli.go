package main

import (
	"encoding/json"
	"log"
	"os"
	"time"

	"github.com/5stackgg/demo-parser/internal/parser"
)

// runCLI reads a .dem from stdin and writes a Result JSON to stdout.
// Logs go to stderr so they don't pollute the JSON stream.
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
		"parsed in %s: ticks=%d tick_rate=%.1f rounds=%d kills=%d bombs=%d "+
			"shots=%d damages=%d spotted=%d nades_thrown=%d nades_detonated=%d map=%s",
		time.Since(start),
		result.TotalTicks, result.TickRate,
		len(result.RoundTicks), len(result.Kills), len(result.Bombs),
		len(result.ShotsFired), len(result.Damages), len(result.Spotted),
		len(result.GrenadeThrows), len(result.GrenadeDetonations),
		result.MapName,
	)
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		log.Printf("encode result: %v", err)
		os.Exit(1)
	}
}
