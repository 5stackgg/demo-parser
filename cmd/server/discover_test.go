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
