package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/5stackgg/demo-parser/internal/geometry"
	"github.com/5stackgg/demo-parser/internal/simulate"
)

// POST /drift — map-patch drift detection.
//
// Given a map, two mesh revisions and a batch of stored lineups, re-fly every
// lineup against both meshes and report what moved. The other map endpoints
// answer questions about one mesh; this one is the only place two are resident
// at once, which is what shapes the limits below.

// maxDriftBody caps the request body. A lineup is ~150 bytes of JSON, so this
// comfortably holds the MaxLineups cap with room for a full constants block.
const maxDriftBody = 16 << 20

// driftConcurrency bounds how many drift requests run at once, and it is
// deliberately 1 by default.
//
// A drift request holds TWO meshes for its whole life — 11-68 MiB each once the
// BVH is built — and it holds them past any cache eviction, since it keeps the
// pointers. Two concurrent requests on different maps is four meshes and a
// quarter of a gigabyte on top of whatever demo this pod is parsing. Drift runs
// when a map updates, which is rarely and in bulk, so serializing costs nothing
// that matters. Raise DRIFT_CONCURRENCY on a pod dedicated to it.
func driftConcurrency() int {
	if n, ok := envInt("DRIFT_CONCURRENCY"); ok && n > 0 {
		return n
	}
	return 1
}

// driftWorkers is how many flights one request runs in parallel. Flights are
// independent and each is deterministic, so this changes throughput and nothing
// about the answer. Capped because the BVH walk is memory-latency bound and
// stops scaling well before it saturates a big machine — and because this pod
// still has demos to parse.
func driftWorkers() int {
	if n, ok := envInt("DRIFT_WORKERS"); ok && n > 0 {
		return n
	}
	return min(runtime.NumCPU(), 8)
}

func envInt(key string) (int, bool) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, false
	}
	return n, true
}

var driftSlots = make(chan struct{}, max(1, driftConcurrency()))

// The mesh LRU has to hold both revisions or every request re-downloads the
// one it evicted. Warned once rather than per request, and not enforced: a
// bound of 1 still produces correct answers, just slowly.
var warnCacheOnce sync.Once

func warnIfCacheTooSmall() {
	warnCacheOnce.Do(func() {
		if n := geometry.MaxCachedMeshes(); n < 2 {
			log.Printf("[drift] MAP_MESH_CACHE is %d; drift holds two meshes at once, so every request will refetch one. Set it to 2 or more.", n)
		}
	})
}

func handleDrift(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req simulate.DriftRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDriftBody))
	if err := dec.Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("bad request: %v", err), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Map) == "" {
		http.Error(w, "map is required", http.StatusBadRequest)
		return
	}
	if len(req.Lineups) == 0 {
		http.Error(w, "lineups must not be empty", http.StatusBadRequest)
		return
	}
	if len(req.Lineups) > simulate.MaxLineups {
		http.Error(w, fmt.Sprintf("too many lineups: %d (max %d)", len(req.Lineups), simulate.MaxLineups),
			http.StatusRequestEntityTooLarge)
		return
	}
	if !req.Stream && len(req.Lineups) > simulate.MaxBufferedLineups {
		http.Error(w, fmt.Sprintf(
			"%d lineups is more than the %d that fit in one JSON body: set \"stream\": true for NDJSON, or send smaller batches",
			len(req.Lineups), simulate.MaxBufferedLineups), http.StatusRequestEntityTooLarge)
		return
	}

	if _, err := geometry.ResolveMeshRevision(req.From); err != nil {
		http.Error(w, fmt.Sprintf("from: %v", err), http.StatusBadRequest)
		return
	}
	if _, err := geometry.ResolveMeshRevision(req.To); err != nil {
		http.Error(w, fmt.Sprintf("to: %v", err), http.StatusBadRequest)
		return
	}
	warnIfCacheTooSmall()

	fromMesh, err := grenadeWorld(req.Map, req.From)
	if err != nil {
		driftMeshError(w, "from", req.Map, req.From, err)
		return
	}
	toMesh, err := grenadeWorld(req.Map, req.To)
	if err != nil {
		driftMeshError(w, "to", req.Map, req.To, err)
		return
	}

	if err := acquireDrift(r.Context()); err != nil {
		http.Error(w, "server busy", http.StatusServiceUnavailable)
		return
	}
	defer releaseDrift()

	// Echo the revisions the worlds were actually loaded at rather than what was
	// sent, so an empty "from" (the process default) is legible in the report
	// months later and names the build even if latest.json moved since.
	req.From, req.To = fromMesh.Revision(), toMesh.Revision()

	if req.Stream {
		streamDrift(w, fromMesh, toMesh, req)
		return
	}
	res, err := simulate.Drift(fromMesh, toMesh, req, simulate.DriftOptions{Workers: driftWorkers()})
	if err != nil {
		// Both meshes are already resolved and the batch is already sized, so
		// anything left is the request describing something that cannot be
		// run — a constant or a threshold out of range.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("[drift] %s %s -> %s: %d lineups, %d moved, %d broken, %d unsimulatable",
		req.Map, req.From, req.To, res.Summary.Lineups, res.Summary.Moved, res.Summary.Broken, res.Summary.Unsimulatable)
	writeJSON(w, res)
}

// grenadeWorld loads one revision of what a grenade collides with on a map —
// the hull plus its grenade clips, unlike the LOS endpoints — mapping "no .tri
// published" onto the same ErrNoMesh the other endpoints use.
func grenadeWorld(mapName, revision string) (*geometry.GrenadeWorld, error) {
	world, err := geometry.LoadGrenadeWorld(mapName, revision)
	if err != nil {
		return nil, errUpstream{fmt.Errorf("load mesh for %s: %w", mapName, err)}
	}
	if world == nil {
		return nil, simulate.ErrNoMesh
	}
	return world, nil
}

// driftMeshError names which side failed. "no mesh at revision X" and "no mesh
// at revision Y" are very different problems for the caller — the first is a
// bad old build, the second means the new build has not been published yet.
func driftMeshError(w http.ResponseWriter, side, mapName, revision string, err error) {
	var upstream errUpstream
	switch {
	case errors.Is(err, simulate.ErrNoMesh):
		if revision == "" {
			revision = "the default revision"
		}
		http.Error(w, fmt.Sprintf("%s: no collision mesh for map %q at %s", side, mapName, revision),
			http.StatusNotFound)
	case errors.As(err, &upstream):
		http.Error(w, fmt.Sprintf("%s: %v", side, err), http.StatusBadGateway)
	default:
		http.Error(w, fmt.Sprintf("%s: %v", side, err), http.StatusBadRequest)
	}
}

func acquireDrift(ctx context.Context) error {
	select {
	case driftSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func releaseDrift() {
	<-driftSlots
}

// streamDrift writes the report as NDJSON: one header line, one line per
// lineup, one summary line. A batch of thousands is answered without ever
// holding more than a chunk of results in memory, and the caller can start
// filing bug reports before the run finishes.
//
// The status line is already sent by the time the first result is written, so a
// failure part way through cannot be a status code — it is a final line with
// type "error", and a consumer that does not check for one will silently treat
// a truncated run as a clean one.
func streamDrift(w http.ResponseWriter, fromMesh, toMesh *geometry.GrenadeWorld, req simulate.DriftRequest) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)

	consts := req.Constants.Apply(simulate.DefaultConstants())
	if err := enc.Encode(driftStreamHeader{
		Type:       "header",
		Map:        req.Map,
		From:       req.From,
		To:         req.To,
		Lineups:    len(req.Lineups),
		Constants:  consts,
		Thresholds: req.Thresholds(),
		Caveats:    simulate.DriftCaveats(fromMesh, toMesh),
	}); err != nil {
		return
	}

	emit := func(batch []simulate.LineupDrift) error {
		for i := range batch {
			if err := enc.Encode(driftStreamResult{Type: "result", LineupDrift: batch[i]}); err != nil {
				return err
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	res, err := simulate.Drift(fromMesh, toMesh, req, simulate.DriftOptions{
		Workers: driftWorkers(),
		Emit:    emit,
	})
	if err != nil {
		_ = enc.Encode(driftStreamError{Type: "error", Error: err.Error()})
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	_ = enc.Encode(driftStreamSummary{Type: "summary", Summary: res.Summary})
	if flusher != nil {
		flusher.Flush()
	}
	log.Printf("[drift] %s %s -> %s (streamed): %d lineups, %d moved, %d broken, %d unsimulatable",
		req.Map, req.From, req.To, res.Summary.Lineups, res.Summary.Moved, res.Summary.Broken, res.Summary.Unsimulatable)
}

// The NDJSON line shapes. Every line carries "type", so one decoder handles the
// whole stream.
type driftStreamHeader struct {
	Type       string              `json:"type"`
	Map        string              `json:"map"`
	From       string              `json:"from"`
	To         string              `json:"to"`
	Lineups    int                 `json:"lineups"`
	Constants  simulate.Constants  `json:"constants"`
	Thresholds simulate.Thresholds `json:"thresholds"`
	Caveats    []string            `json:"caveats"`
}

type driftStreamResult struct {
	Type string `json:"type"`
	simulate.LineupDrift
}

type driftStreamSummary struct {
	Type    string                `json:"type"`
	Summary simulate.DriftSummary `json:"summary"`
}

type driftStreamError struct {
	Type  string `json:"type"`
	Error string `json:"error"`
}
