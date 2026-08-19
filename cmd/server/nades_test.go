package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/5stackgg/demo-parser/internal/geometry"
	"github.com/5stackgg/demo-parser/internal/parser"
	"github.com/golang/geo/r3"
)

// A spot on Mirage's A site: open ground with a wall about a hundred units to
// the -X side and the floor just below. Real geometry rather than a synthetic
// box, so the numbers below are what the shipped mesh actually produces.
var mirageSpot = parser.Point{X: -2300, Y: 0, Z: -128}

// meshFixtureDir locates the replay-map-meshes clone checked out above this
// repo, so the endpoints run against a real .tri rather than a fixture shaped
// like one. Empty when the clone is absent — the meshes are tens of MB and are
// not vendored here, so those tests skip.
func meshFixtureDir(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("MAP_MESH_FIXTURES"); dir != "" {
		return dir
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for d := wd; ; {
		candidate := filepath.Join(d, "replay-map-meshes")
		if _, err := os.Stat(filepath.Join(candidate, "de_mirage.tri")); err == nil {
			return candidate
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// serveLocalMeshes points the mesh loader at that clone.
func serveLocalMeshes(t *testing.T) {
	t.Helper()
	dir := meshFixtureDir(t)
	if dir == "" {
		t.Skip("no replay-map-meshes clone found above the working directory; set MAP_MESH_FIXTURES")
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	t.Setenv("MAP_MESH_CDN", srv.URL)
}

func post(t *testing.T, h http.HandlerFunc, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)))
	return w
}

// floorUnder is the world height of the surface below a point, by raycast
// against the same mesh the endpoint used.
func floorUnder(t *testing.T, at parser.Point) float64 {
	t.Helper()
	mesh, err := geometry.Load("de_mirage")
	if err != nil || mesh == nil {
		t.Fatalf("load mesh: %v", err)
	}
	d, ok := mesh.RayHitDist(r3.Vector{X: at.X, Y: at.Y, Z: at.Z}, r3.Vector{Z: -1})
	if !ok {
		t.Fatal("no floor under the test point")
	}
	return at.Z - d
}

func TestSmokeVolumeEndpointOnARealMap(t *testing.T) {
	serveLocalMeshes(t)
	w := post(t, handleSmokeVolume, parser.SmokeVolumeRequest{
		Map: "de_mirage", X: mirageSpot.X, Y: mirageSpot.Y, Z: mirageSpot.Z,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	// Decoded as a bare map first: these key names are the contract the web
	// renderer and the API are coding against.
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"map", "ox", "oy", "oz", "vs", "dx", "dy", "dz", "den", "cells", "radius"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("response is missing %q: %v", key, raw)
		}
	}

	var res parser.SmokeVolumeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Cells < 500 {
		t.Fatalf("only %d cells on open ground; expected a full cloud", res.Cells)
	}
	if res.VoxelSize <= 0 || res.DimX <= 0 || res.DimY <= 0 || res.DimZ <= 0 {
		t.Fatalf("degenerate grid: %+v", res.EventSmokeVolume)
	}
	packed, err := base64.StdEncoding.DecodeString(res.Density)
	if err != nil {
		t.Fatalf("density is not base64: %v", err)
	}
	total := res.DimX * res.DimY * res.DimZ
	if len(packed) != (total+1)/2 {
		t.Fatalf("density is %d bytes, want %d for %d cells", len(packed), (total+1)/2, total)
	}

	// The map is what shapes the cloud, and the two things it must do here are
	// stop it at the floor and stop it at the wall. An unshaped sphere would be
	// the full 19 cells on every axis.
	full := 2*int(math.Ceil(144/float64(res.VoxelSize))) + 1
	if res.DimZ >= full {
		t.Fatalf("the floor should have clipped the cloud vertically: %d of %d cells", res.DimZ, full)
	}
	floor := floorUnder(t, mirageSpot)
	if lowest := float64(res.OriginZ) + float64(res.VoxelSize)/2; lowest < floor {
		t.Fatalf("cloud sank through the floor: lowest cell centre %.1f, floor %.1f", lowest, floor)
	}
}

func TestSmokeVolumeEndpoint404sAnUnknownMap(t *testing.T) {
	serveLocalMeshes(t)
	w := post(t, handleSmokeVolume, parser.SmokeVolumeRequest{Map: "de_not_a_map"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", w.Code, w.Body.String())
	}
}

// The status a caller sees decides whether they retry, fix their request, or
// tell the user the map is unsupported, so the mapping is pinned.
func TestGeometryErrorsMapToStatuses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"unsupported map", parser.ErrNoMesh, http.StatusNotFound},
		{"point inside geometry", parser.ErrSmokeSealed, http.StatusUnprocessableEntity},
		{"mesh cdn down", errUpstream{errors.New("load mesh for de_mirage: timeout")}, http.StatusBadGateway},
		{"client went away", context.Canceled, http.StatusServiceUnavailable},
		{"anything else", errors.New("pairs must not be empty"), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			geometryError(w, "de_mirage", tc.err)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d (%s)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestGeometryEndpointsRejectBadRequests(t *testing.T) {
	serveLocalMeshes(t)
	handlers := map[string]http.HandlerFunc{
		"smoke-volume": handleSmokeVolume,
		"sightlines":   handleSightlines,
		"oneway":       handleOneway,
	}
	for name, h := range handlers {
		t.Run(name+" method", func(t *testing.T) {
			w := httptest.NewRecorder()
			h(w, httptest.NewRequest(http.MethodGet, "/", nil))
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status %d, want 405", w.Code)
			}
		})
		t.Run(name+" body", func(t *testing.T) {
			w := httptest.NewRecorder()
			h(w, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte("{"))))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", w.Code)
			}
		})
		t.Run(name+" no map", func(t *testing.T) {
			w := post(t, h, map[string]any{"pairs": []any{}})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestSightlinesEndpointOnARealMap(t *testing.T) {
	serveLocalMeshes(t)
	at := mirageSpot
	// Along Y so the lines stay in the open part of the site: through the
	// cloud, over the top of it where it thins out, and well clear of it.
	line := func(dz, dy float64) parser.SightlinePair {
		return parser.SightlinePair{
			From: parser.Point{X: at.X, Y: at.Y - 220 + dy, Z: at.Z + dz},
			To:   parser.Point{X: at.X, Y: at.Y + 220 + dy, Z: at.Z + dz},
		}
	}
	w := post(t, handleSightlines, parser.SightlineRequest{
		Map: "de_mirage",
		At:  &at,
		Pairs: []parser.SightlinePair{line(0, 0), line(90, 0), {
			// Well clear of the cloud and still in the open part of the site.
			From: parser.Point{X: at.X, Y: at.Y - 500, Z: at.Z},
			To:   parser.Point{X: at.X, Y: at.Y - 300, Z: at.Z},
		}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var res parser.SightlineResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Threshold != parser.DefaultBlockThreshold {
		t.Fatalf("threshold = %v, want the default", res.Threshold)
	}
	if len(res.Smokes) != 1 || res.Smokes[0].Sealed {
		t.Fatalf("expected one flooded cloud: %+v", res.Smokes)
	}

	core, over, away := res.Results[0], res.Results[1], res.Results[2]
	if !core.Blocked || core.BlockedBy != "smoke" {
		t.Fatalf("a line through the cloud should be blocked by smoke: %+v", core)
	}
	if core.Depth < 2*res.Threshold {
		t.Fatalf("core depth %.2f is not comfortably over the threshold", core.Depth)
	}
	if over.Blocked || over.WorldBlocked {
		t.Fatalf("a line over the top of the cloud is open: %+v", over)
	}
	if over.Depth <= 0 || over.Depth >= res.Threshold {
		t.Fatalf("a line grazing the top should carry some smoke but not enough: %.2f", over.Depth)
	}
	if away.Blocked || away.Depth != 0 || away.WorldBlocked {
		t.Fatalf("a line 500 units away should be untouched: %+v", away)
	}
}

func TestOnewayEndpointOnARealMap(t *testing.T) {
	serveLocalMeshes(t)
	at := mirageSpot
	floor := floorUnder(t, at)
	w := post(t, handleOneway, parser.OneWayRequest{
		Map: "de_mirage",
		At:  &at,
		Pairs: []parser.SightlinePair{{
			From: parser.Point{X: at.X, Y: at.Y - 200, Z: floor},
			To:   parser.Point{X: at.X, Y: at.Y + 200, Z: floor},
		}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var res parser.OneWayResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Caveats) == 0 {
		t.Fatal("the caveats travel with every response")
	}
	got := res.Results[0]
	if len(got.Stances) != 4 {
		t.Fatalf("expected four stance pairings, got %d", len(got.Stances))
	}
	// Two players on the same floor either side of a ground-level smoke: the
	// cloud swallows both of them whichever way they stand.
	if got.OneWay {
		t.Fatalf("a symmetric pair should not be reported as a one-way: %+v", got)
	}
	for _, st := range got.Stances {
		if st.AToB.Visible || st.BToA.Visible {
			t.Fatalf("neither side sees through a full cloud: %+v", st)
		}
	}
}

// This runs as a shared service behind a UI that will fire a preview per click,
// while the same process is parsing demos. Concurrent callers must get the same
// answer, and the mesh must be loaded once rather than raced into.
func TestSmokeVolumeEndpointIsSafeUnderConcurrency(t *testing.T) {
	serveLocalMeshes(t)
	body, err := json.Marshal(parser.SmokeVolumeRequest{
		Map: "de_mirage", X: mirageSpot.X, Y: mirageSpot.Y, Z: mirageSpot.Z,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	const callers = 8
	var wg sync.WaitGroup
	codes := make([]int, callers)
	bodies := make([]string, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := httptest.NewRecorder()
			handleSmokeVolume(w, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)))
			codes[i], bodies[i] = w.Code, w.Body.String()
		}(i)
	}
	wg.Wait()

	for i := range codes {
		if codes[i] != http.StatusOK {
			t.Fatalf("caller %d: status %d: %s", i, codes[i], bodies[i])
		}
		if bodies[i] != bodies[0] {
			t.Fatalf("caller %d got a different volume for the same point", i)
		}
	}
}
