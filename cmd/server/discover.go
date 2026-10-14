package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strings"

	"github.com/5stackgg/demo-parser/internal/discover"
	"github.com/5stackgg/demo-parser/internal/geometry"
	"github.com/5stackgg/demo-parser/internal/simulate"
)

// POST /discover — throws that land a grenade on a chosen spot.
//
// Given a map, a target and an area to throw from, search the techniques and
// mouse buttons asked for and return the throws the fitted flight model lands
// on the target, easiest first. They are predictions for a game server to
// throw for real; see package discover.

const maxDiscoverBody = 64 << 10

// discoverConcurrency bounds how many searches run at once. A search keeps
// every core it is given busy for its whole budget, so two at once only makes
// both slower; the API queues them anyway.
func discoverConcurrency() int {
	if n, ok := envInt("DISCOVER_CONCURRENCY"); ok && n > 0 {
		return n
	}
	return 1
}

func discoverWorkers() int {
	if n, ok := envInt("DISCOVER_WORKERS"); ok && n > 0 {
		return n
	}
	return min(runtime.NumCPU(), 8)
}

var discoverSlots = make(chan struct{}, max(1, discoverConcurrency()))

func handleDiscover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req discover.Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDiscoverBody))
	if err := dec.Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("bad request: %v", err), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Map) == "" {
		http.Error(w, "map is required", http.StatusBadRequest)
		return
	}
	if err := req.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := geometry.ResolveMeshRevision(req.Revision); err != nil {
		http.Error(w, fmt.Sprintf("revision: %v", err), http.StatusBadRequest)
		return
	}

	select {
	case discoverSlots <- struct{}{}:
		defer func() { <-discoverSlots }()
	case <-r.Context().Done():
		return
	}

	world, err := grenadeWorld(req.Map, req.Revision)
	if err != nil {
		var upstream errUpstream
		switch {
		case errors.Is(err, simulate.ErrNoMesh):
			http.Error(w, fmt.Sprintf("no collision mesh for %s", req.Map), http.StatusNotFound)
		case errors.As(err, &upstream):
			http.Error(w, err.Error(), http.StatusBadGateway)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	res, err := discover.Search(r.Context(), world, req, discoverWorkers())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("[discover] %s: %d stands, %d flights, %d candidates%s",
		req.Map, res.Stands, res.Flights, len(res.Candidates), map[bool]string{true: " (budget ran out)", false: ""}[res.Truncated])
	writeJSON(w, res)
}

// POST /floor — the floor under a point nearest the height given for it, for a
// caller that picked the point off a radar and has only a guess at the height.

type floorRequest struct {
	Map      string  `json:"map"`
	Revision string  `json:"revision,omitempty"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	Z        float64 `json:"z"`
}

type floorResponse struct {
	Found bool    `json:"found"`
	Z     float64 `json:"z"`
}

func handleFloor(w http.ResponseWriter, r *http.Request) {
	var req floorRequest
	if !decodeGeometryRequest(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Map) == "" {
		http.Error(w, "map is required", http.StatusBadRequest)
		return
	}
	if _, err := geometry.ResolveMeshRevision(req.Revision); err != nil {
		http.Error(w, fmt.Sprintf("revision: %v", err), http.StatusBadRequest)
		return
	}
	world, err := grenadeWorld(req.Map, req.Revision)
	if err != nil {
		var upstream errUpstream
		switch {
		case errors.Is(err, simulate.ErrNoMesh):
			http.Error(w, fmt.Sprintf("no collision mesh for %s", req.Map), http.StatusNotFound)
		case errors.As(err, &upstream):
			http.Error(w, err.Error(), http.StatusBadGateway)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	z, found := discover.FloorNear(world, req.X, req.Y, req.Z)
	writeJSON(w, floorResponse{Found: found, Z: z})
}
