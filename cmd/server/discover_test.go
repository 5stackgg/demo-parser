package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/5stackgg/demo-parser/internal/discover"
	"github.com/golang/geo/r3"
)

func openGround(t *testing.T) string {
	t.Helper()
	return synthRevision(t, map[string][][9]float64{
		"de_clip.tri.gz": {
			{-3000, -3000, 0, 3000, -3000, 0, 3000, 3000, 0},
			{-3000, -3000, 0, 3000, 3000, 0, -3000, 3000, 0},
		},
	})
}

func discoverRequest(revision string) discover.Request {
	return discover.Request{
		Map:        "de_clip",
		Revision:   revision,
		Target:     r3.Vector{X: 900, Y: 300},
		Radius:     24,
		Area:       discover.Area{Min: r3.Vector{X: -64, Y: -64, Z: -32}, Max: r3.Vector{X: 64, Y: 64, Z: 32}},
		Techniques: []discover.Technique{{}, {JumpThrow: true}},
		Strengths:  []discover.Strength{discover.Full},
		BudgetMs:   20000,
	}
}

func TestDiscoverEndpointFindsThrows(t *testing.T) {
	t.Setenv("MAP_MESH_CACHE", "4")
	w := post(t, handleDiscover, discoverRequest(openGround(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var res discover.Response
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) == 0 || res.Truncated {
		t.Fatalf("%d candidates, truncated %v", len(res.Candidates), res.Truncated)
	}
	for _, c := range res.Candidates {
		if c.Distance > 24 || c.Name == "" {
			t.Fatalf("candidate %+v", c)
		}
	}
}

func TestDiscoverEndpointRejectsBadRequests(t *testing.T) {
	t.Setenv("MAP_MESH_CDN", "")
	cases := map[string]func(*discover.Request){
		"no map":            func(r *discover.Request) { r.Map = "" },
		"no technique":      func(r *discover.Request) { r.Techniques = nil },
		"no radius":         func(r *discover.Request) { r.Radius = 0 },
		"escaping revision": func(r *discover.Request) { r.Revision = "../../../etc" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := discoverRequest("")
			mutate(&req)
			if w := post(t, handleDiscover, req); w.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestDiscoverEndpoint404sAMapWithNoMesh(t *testing.T) {
	t.Setenv("MAP_MESH_CACHE", "4")
	req := discoverRequest(openGround(t))
	req.Map = "de_nonexistent"
	if w := post(t, handleDiscover, req); w.Code != http.StatusNotFound {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

func TestDiscoverEndpointRejectsNonPost(t *testing.T) {
	w := httptest.NewRecorder()
	handleDiscover(w, httptest.NewRequest(http.MethodGet, "/discover", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", w.Code)
	}
}

func TestDiscoverEndpointWritesLowercasePoints(t *testing.T) {
	t.Setenv("MAP_MESH_CACHE", "4")
	w := post(t, handleDiscover, discoverRequest(openGround(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		Candidates []map[string]any `json:"candidates"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) == 0 {
		t.Fatal("no candidates")
	}
	for _, field := range []string{"stand", "eye", "position", "velocity", "landing"} {
		v, ok := res.Candidates[0][field].(map[string]any)
		if !ok {
			t.Fatalf("%s is %T", field, res.Candidates[0][field])
		}
		for _, axis := range []string{"x", "y", "z"} {
			if _, ok := v[axis].(float64); !ok {
				t.Fatalf("%s has no %q: %v", field, axis, v)
			}
		}
	}
}

// A spot picked off a radar has no real height. Snapped, a guess 90 units off
// still finds the throws onto the floor there; unsnapped it finds none.
func TestDiscoverEndpointSnapsAGuessedHeightToTheFloor(t *testing.T) {
	t.Setenv("MAP_MESH_CACHE", "4")
	revision := openGround(t)

	req := discoverRequest(revision)
	req.Target.Z = 90
	req.Area.Min.Z, req.Area.Max.Z = -200, 200

	w := post(t, handleDiscover, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var unsnapped discover.Response
	if err := json.Unmarshal(w.Body.Bytes(), &unsnapped); err != nil {
		t.Fatal(err)
	}
	if len(unsnapped.Candidates) != 0 {
		t.Fatalf("unsnapped, %d candidates land 90 units under the target", len(unsnapped.Candidates))
	}

	req.Snap = true
	w = post(t, handleDiscover, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		Candidates []map[string]any `json:"candidates"`
		Target     struct{ X, Y, Z float64 }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) == 0 || res.Target.Z != 0 {
		t.Fatalf("snapped: %d candidates, target z %v", len(res.Candidates), res.Target.Z)
	}
}

func TestFloorEndpoint(t *testing.T) {
	t.Setenv("MAP_MESH_CACHE", "4")
	revision := openGround(t)

	w := post(t, handleFloor, floorRequest{Map: "de_clip", Revision: revision, X: 10, Y: 10, Z: 64})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var res floorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Found || res.Z != 0 {
		t.Fatalf("floor %+v, want found at 0", res)
	}

	w = post(t, handleFloor, floorRequest{Map: "de_clip", Revision: revision, X: 9000, Y: 9000, Z: 0})
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Found {
		t.Fatalf("off the map: %+v %v", res, err)
	}
}
