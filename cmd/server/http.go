package main

import (
	"bufio"
	"bytes"
	"compress/bzip2"
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

	f, err := downloadDemo(req.DemoURL, logPrefix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() {
		name := f.Name()
		_ = f.Close()
		_ = os.Remove(name)
	}()

	body, err := sniffDemoStream(f, logPrefix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	log.Printf("[%s] parsing demo", logPrefix)
	parseAndRespond(w, body, logPrefix)
}

// demoClient drains the demo CDN at full network speed. Valve's replay
// servers drop a connection that stays idle, so the download must not be
// paced by the parser — handleParse buffers the whole body to a temp file
// before parsing rather than streaming the socket straight into the parser.
var demoClient = &http.Client{Timeout: 5 * time.Minute}

// downloadDemo fetches the demo to a temp file and verifies the byte count
// against Content-Length. A truncated download (CDN dropped the connection)
// becomes a hard error here instead of a silent partial parse downstream.
// The returned file is rewound to the start; the caller owns its cleanup.
func downloadDemo(url, logPrefix string) (*os.File, error) {
	log.Printf("[%s] fetching demo %s", logPrefix, url)
	fetchStart := time.Now()

	resp, err := demoClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch demo: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("fetch demo: upstream %d", resp.StatusCode)
	}

	f, err := os.CreateTemp("", "demo-*.dem")
	if err != nil {
		return nil, fmt.Errorf("create temp: %v", err)
	}

	n, err := io.Copy(f, resp.Body)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("download demo (after %d bytes): %w", n, err)
	}
	if want := resp.ContentLength; want > 0 && n != want {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("download truncated: got %d of %d bytes", n, want)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("rewind temp: %v", err)
	}

	log.Printf("[%s] downloaded %d bytes in %s", logPrefix, n, time.Since(fetchStart))
	return f, nil
}

// sniffDemoStream inspects the first 8 bytes to decide whether the
// body is bzip2-compressed, a raw CS:GO/CS2 demo, or something else
// entirely (an HTML error page from Valve's CDN when a demo has been
// tombstoned). For raw demos we accept either HL2DEMO (legacy) or
// PBDEMS2 (CS2 source 2 protobuf). Anything else returns an error
// with the magic bytes so the caller can see what was actually served.
func sniffDemoStream(body io.Reader, logPrefix string) (io.Reader, error) {
	br := bufio.NewReader(body)
	magic, err := br.Peek(8)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("read demo head: %w", err)
	}
	if len(magic) < 3 {
		return nil, fmt.Errorf("demo too short (%d bytes)", len(magic))
	}
	if bytes.HasPrefix(magic, []byte("BZh")) {
		return bzip2.NewReader(br), nil
	}
	if bytes.HasPrefix(magic, []byte("HL2DEMO")) ||
		bytes.HasPrefix(magic, []byte("PBDEMS2")) {
		return br, nil
	}
	log.Printf("[%s] unrecognized demo magic: %q", logPrefix, magic)
	return nil, fmt.Errorf("unrecognized demo content: %q", magic)
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
