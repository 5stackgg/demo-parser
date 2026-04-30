package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"time"

	"github.com/5stackgg/demo-parser/internal/parser"
)

type parseRequest struct {
	MatchMapDemoID string `json:"match_map_demo_id"`
	DemoURL        string `json:"demo_url"`
}

func runServer() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/parse", handleParse)
	mux.HandleFunc("/parse-file", handleParseFile)

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

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleParse fetches a demo from a (typically pre-signed) URL and
// returns its parsed Result as JSON.
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

	parseAndRespond(w, demoResp.Body, logPrefix)
}

// handleParseFile is the pre-upload entry point used by a game-server
// node that has the .dem on local disk. The .dem streams in via
// multipart/form-data on the "demo" field — never buffered to memory
// or disk.
func handleParseFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, fmt.Sprintf("bad multipart: %v", err), http.StatusBadRequest)
		return
	}

	var (
		filePart *multipart.Part
		fileName string
	)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("read part: %v", err), http.StatusBadRequest)
			return
		}
		if part.FormName() == "demo" {
			filePart = part
			fileName = part.FileName()
			break
		}
		_ = part.Close()
	}

	if filePart == nil {
		http.Error(w, "missing 'demo' form field", http.StatusBadRequest)
		return
	}
	defer filePart.Close()

	logPrefix := fileName
	if logPrefix == "" {
		logPrefix = "<no-name>"
	}
	log.Printf("[%s] parsing uploaded demo", logPrefix)

	parseAndRespond(w, filePart, logPrefix)
}

func parseAndRespond(w http.ResponseWriter, body io.Reader, logPrefix string) {
	start := time.Now()
	result, err := parser.Parse(body)
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
