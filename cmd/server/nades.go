package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strings"

	"github.com/5stackgg/demo-parser/internal/geometry"
	"github.com/5stackgg/demo-parser/internal/parser"
)

// The lineup endpoints: given a map and a point, what shape does the smoke
// take, and what does it block. None of them touch a demo — they run the same
// geometry the parser runs, against a mesh loaded by map name, so a lineup
// nobody has ever thrown can be answered the same way a played one is.

// maxGeometryBody caps a request body. The largest legitimate one is a few
// hundred sightline pairs plus a supplied volume, which is tens of KB.
const maxGeometryBody = 1 << 20

// geometrySlots bounds how many requests build or walk a volume at once. A
// build is thousands of raycasts against a mesh of a hundred thousand
// triangles, and this process also parses demos; without a bound a burst of
// previews from one browser would starve them.
var geometrySlots = make(chan struct{}, max(2, runtime.NumCPU()))

func acquireGeometry(ctx context.Context) error {
	select {
	case geometrySlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func releaseGeometry() {
	<-geometrySlots
}

// meshFor resolves a map name to its collision mesh — never the grenade clips,
// which block no sightline. geometry.Load is process-wide, concurrency-safe and
// LRU-bounded (MAP_MESH_CACHE), so many maps can pass through this service
// without it holding all of them: a mesh is a few MB gzipped on the wire, up to
// ~40 MB of .tri once inflated, and more again once the BVH is built.
func meshFor(mapName string) (*geometry.Mesh, error) {
	if strings.TrimSpace(mapName) == "" {
		return nil, errors.New("map is required")
	}
	mesh, err := geometry.Load(mapName)
	if err != nil {
		return nil, errUpstream{fmt.Errorf("load mesh for %s: %w", mapName, err)}
	}
	if mesh == nil {
		return nil, parser.ErrNoMesh
	}
	return mesh, nil
}

// errUpstream marks a failure that is not the caller's doing — the mesh CDN
// being unreachable — so it is not reported back as a bad request.
type errUpstream struct{ err error }

func (e errUpstream) Error() string { return e.err.Error() }
func (e errUpstream) Unwrap() error { return e.err }

// geometryError maps a failure onto a status. Anything unrecognised is the
// caller's request being wrong rather than the server failing, since these
// handlers do no I/O of their own beyond the mesh fetch.
func geometryError(w http.ResponseWriter, mapName string, err error) {
	var upstream errUpstream
	switch {
	case errors.Is(err, parser.ErrNoMesh):
		http.Error(w, fmt.Sprintf("no collision mesh published for map %q", mapName), http.StatusNotFound)
	case errors.Is(err, parser.ErrSmokeSealed):
		http.Error(w, "smoke point is sealed inside geometry: no free space to bloom into", http.StatusUnprocessableEntity)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		http.Error(w, "server busy", http.StatusServiceUnavailable)
	case errors.As(err, &upstream):
		http.Error(w, err.Error(), http.StatusBadGateway)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

func decodeGeometryRequest(w http.ResponseWriter, r *http.Request, into any) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxGeometryBody))
	if err := dec.Decode(into); err != nil {
		http.Error(w, fmt.Sprintf("bad request: %v", err), http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[geometry] response encode: %v", err)
	}
}

// handleSmokeVolume returns the real bloom at a point: the free space a smoke
// would fill there, as the same voxel grid the playback blob carries.
func handleSmokeVolume(w http.ResponseWriter, r *http.Request) {
	var req parser.SmokeVolumeRequest
	if !decodeGeometryRequest(w, r, &req) {
		return
	}
	mesh, err := meshFor(req.Map)
	if err != nil {
		geometryError(w, req.Map, err)
		return
	}
	if err := acquireGeometry(r.Context()); err != nil {
		geometryError(w, req.Map, err)
		return
	}
	defer releaseGeometry()

	res, err := parser.SmokeVolume(mesh, req)
	if err != nil {
		geometryError(w, req.Map, err)
		return
	}
	writeJSON(w, res)
}

// handleSightlines answers, for a batch of eye-to-eye lines, whether the smoke
// (or the map) blocks them and by how much.
func handleSightlines(w http.ResponseWriter, r *http.Request) {
	var req parser.SightlineRequest
	if !decodeGeometryRequest(w, r, &req) {
		return
	}
	mesh, err := meshFor(req.Map)
	if err != nil {
		geometryError(w, req.Map, err)
		return
	}
	if err := acquireGeometry(r.Context()); err != nil {
		geometryError(w, req.Map, err)
		return
	}
	defer releaseGeometry()

	res, err := parser.Sightlines(mesh, req)
	if err != nil {
		geometryError(w, req.Map, err)
		return
	}
	writeJSON(w, res)
}

// handleOneway reports asymmetric visibility — one side sees through, the other
// does not — across standing and crouched eye heights.
func handleOneway(w http.ResponseWriter, r *http.Request) {
	var req parser.OneWayRequest
	if !decodeGeometryRequest(w, r, &req) {
		return
	}
	mesh, err := meshFor(req.Map)
	if err != nil {
		geometryError(w, req.Map, err)
		return
	}
	if err := acquireGeometry(r.Context()); err != nil {
		geometryError(w, req.Map, err)
		return
	}
	defer releaseGeometry()

	res, err := parser.OneWay(mesh, req)
	if err != nil {
		geometryError(w, req.Map, err)
		return
	}
	writeJSON(w, res)
}
