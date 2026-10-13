package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/5stackgg/demo-parser/internal/parser"
	"github.com/cosnicolaou/pbzip2"
)

type parseRequest struct {
	MatchMapDemoID string           `json:"match_map_demo_id"`
	DemoURL        string           `json:"demo_url"`
	Playback       *playbackRequest `json:"playback,omitempty"`
}

func runServer() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/parse", handleParse)
	mux.HandleFunc("/smoke-volume", handleSmokeVolume)
	mux.HandleFunc("/smoke-voxels", handleSmokeVoxels)
	mux.HandleFunc("/sightlines", handleSightlines)
	mux.HandleFunc("/oneway", handleOneway)
	mux.HandleFunc("/drift", handleDrift)

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

	start := time.Now()
	result, t, err := fetchAndParse(req.DemoURL, logPrefix)
	if err != nil {
		status := http.StatusBadGateway
		var pe parseError
		if errors.As(err, &pe) {
			status = http.StatusUnprocessableEntity
		}
		log.Printf("[%s] failed after %s: %v", logPrefix, time.Since(start), err)
		http.Error(w, err.Error(), status)
		return
	}

	resp := parseResponse{Result: result}
	var blobNote string
	if req.Playback.wanted() {
		pb, took, err := buildPlayback(result, req.Playback)
		if err != nil {
			// The full result still answers: the api builds the blob from it.
			log.Printf("[%s] playback blob: %v", logPrefix, err)
		} else {
			resp.Playback = pb
			blobNote = fmt.Sprintf(", blob %s (%d bytes)", took.Round(time.Millisecond), len(pb.Blob))
		}
	} else if req.Playback != nil {
		log.Printf("[%s] playback blob v%d not built (this parser builds v%d); answering with the full parse",
			logPrefix, req.Playback.SchemaVersion, parser.PlaybackSchemaVersion)
	}

	respondStart := time.Now()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	cw := &countingWriter{w: w}
	if err := json.NewEncoder(cw).Encode(resp); err != nil {
		log.Printf("[%s] response encode: %v", logPrefix, err)
	}
	// One line with the whole request, so a slow import can be read off the
	// parser's log without adding up the lines above it. Download and parse
	// overlap: the parse starts on the first bytes and follows the download.
	log.Printf(
		"[%s] done in %s: download %s (%d bytes), parse %s%s, respond %s (%d bytes)%s",
		logPrefix, time.Since(start).Round(time.Millisecond),
		t.download.Round(time.Millisecond), t.size, t.parse.Round(time.Millisecond),
		blobNote, time.Since(respondStart).Round(time.Millisecond), cw.n, t.attemptsNote(),
	)
}

// parseError is a demo that downloaded but would not parse: a CDN error page,
// a corrupt file. Retrying the download would only fetch it again.
type parseError struct{ err error }

func (e parseError) Error() string { return e.err.Error() }
func (e parseError) Unwrap() error { return e.err }

// parseTimings is what one request spent where, for the summary log line.
type parseTimings struct {
	download time.Duration
	parse    time.Duration
	size     int64
	attempts int
}

func (t parseTimings) attemptsNote() string {
	if t.attempts <= 1 {
		return ""
	}
	return fmt.Sprintf(", %d attempts", t.attempts)
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

const demoDownloadAttempts = 3

// fetchAndParse downloads and parses a demo. Valve's replay CDN intermittently
// drops connections mid-stream and returns transient 5xx, so a failed download
// is retried with backoff — the parse with it, since the parse runs alongside
// the download and has read what arrived. 4xx (e.g. an expired demo) is
// permanent and returned immediately.
func fetchAndParse(demoURL, logPrefix string) (*parser.Result, parseTimings, error) {
	log.Printf("[%s] fetching demo %s", logPrefix, demoURL)
	var lastErr error
	for attempt := 1; attempt <= demoDownloadAttempts; attempt++ {
		result, t, retryable, err := fetchAndParseOnce(demoURL, logPrefix)
		t.attempts = attempt
		if err == nil {
			return result, t, nil
		}
		lastErr = err
		if !retryable || attempt == demoDownloadAttempts {
			break
		}
		backoff := time.Duration(attempt*2) * time.Second
		log.Printf("[%s] download attempt %d/%d failed (%v); retrying in %s",
			logPrefix, attempt, demoDownloadAttempts, err, backoff)
		time.Sleep(backoff)
	}
	return nil, parseTimings{}, lastErr
}

// fetchAndParseOnce parses the demo while it downloads. The parser treats a
// read error as the end of the demo and returns what it had, so the download's
// own outcome decides whether the result stands: a parse of a download that
// broke off is a parse of a truncated demo, and is thrown away.
func fetchAndParseOnce(demoURL, logPrefix string) (*parser.Result, parseTimings, bool, error) {
	var t parseTimings
	dl, retryable, err := startDownload(demoURL, logPrefix)
	if err != nil {
		return nil, t, retryable, err
	}
	defer dl.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body, sniffErr := sniffDemoStream(ctx, dl.reader(), logPrefix)
	if sniffErr != nil {
		// Too short to sniff can be a download that broke off early.
		if res := dl.wait(); res.err != nil {
			return nil, t, res.retryable, res.err
		}
		return nil, t, false, parseError{sniffErr}
	}

	log.Printf("[%s] parsing demo", logPrefix)
	parseStart := time.Now()
	result, err := parser.Parse(body)
	t.parse = time.Since(parseStart)

	res := dl.wait()
	t.download, t.size = res.elapsed, res.size
	if res.err != nil {
		return nil, t, res.retryable, res.err
	}
	if err != nil {
		log.Printf("[%s] parse error: %v", logPrefix, err)
		return nil, t, false, parseError{fmt.Errorf("parse: %v", err)}
	}
	log.Printf(
		"[%s] parsed in %s: ticks=%d tick_rate=%.1f rounds=%d map=%s",
		logPrefix, t.parse,
		result.TotalTicks, result.TickRate, len(result.RoundTicks), result.MapName,
	)
	return result, t, false, nil
}

// bzip2Workers is how many blocks of a bzip2 demo decompress at once. Valve
// serves its demos bzip2'd, and the standard library's single-threaded decoder
// manages about 10 MB/s — slower than the parse it feeds, so it set the pace of
// the whole parse. Four keep well ahead of the parser without taking the cores
// it needs.
var bzip2Workers = min(runtime.GOMAXPROCS(0), 4)

// sniffDemoStream inspects the first 8 bytes to decide whether the
// body is bzip2-compressed (Valve), gzip-compressed (FACEIT), a raw
// CS:GO/CS2 demo, or something else entirely (an HTML error page from
// a CDN when a demo has been tombstoned). For raw demos we accept
// either HL2DEMO (legacy) or PBDEMS2 (CS2 source 2 protobuf). Anything
// else returns an error with the magic bytes so the caller can see
// what was actually served. Cancelling ctx stops a bzip2 decoder's workers.
func sniffDemoStream(ctx context.Context, body io.Reader, logPrefix string) (io.Reader, error) {
	br := bufio.NewReader(body)
	magic, err := br.Peek(8)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("read demo head: %w", err)
	}
	if len(magic) < 3 {
		return nil, fmt.Errorf("demo too short (%d bytes)", len(magic))
	}
	if bytes.HasPrefix(magic, []byte("BZh")) {
		return pbzip2.NewReader(ctx, br, pbzip2.DecompressionOptions(
			pbzip2.BZConcurrency(bzip2Workers),
		)), nil
	}
	if bytes.HasPrefix(magic, []byte{0x1f, 0x8b}) {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		return gz, nil
	}
	if bytes.HasPrefix(magic, []byte("HL2DEMO")) ||
		bytes.HasPrefix(magic, []byte("PBDEMS2")) {
		return br, nil
	}
	log.Printf("[%s] unrecognized demo magic: %q", logPrefix, magic)
	return nil, fmt.Errorf("unrecognized demo content: %q", magic)
}
