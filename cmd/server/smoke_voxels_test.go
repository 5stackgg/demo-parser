package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/5stackgg/demo-parser/internal/parser"
)

// recordedVoxelLog is a log captured off one of test.dem's smokes.
func recordedVoxelLog(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "parser", "testdata", "smoke_voxels_inferno_4019.bin"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func smokeVoxelsErrorOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error == "" {
		t.Fatalf("error is not JSON: %q", w.Body.String())
	}
	return body.Error
}

func TestSmokeVoxelsEndpoint(t *testing.T) {
	buf := recordedVoxelLog(t)
	w := post(t, handleSmokeVoxels, map[string]any{
		"log":        base64.StdEncoding.EncodeToString(buf),
		"samples":    [][2]int{{len(buf) / 3, 0}, {len(buf), 40}},
		"detonation": map[string]float64{"x": -100, "y": 1250, "z": 100},
		"tick_rate":  64,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	// Decoded as a bare map first: these are the keys of a playback blob's
	// smoke_voxels row, which the API stores as is.
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"gid", "round", "start_tick", "end_tick", "ox", "oy", "oz", "vs", "dx", "dy", "dz", "arr"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("response is missing %q: %v", key, raw)
		}
	}

	var res parser.EventSmokeVoxels
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.StartTick != 0 || res.EndTick != 40 || res.VoxelSize != 20 {
		t.Fatalf("ticks %d–%d vs=%v, want 0–40 vs=20", res.StartTick, res.EndTick, res.VoxelSize)
	}
	arr, err := base64.StdEncoding.DecodeString(res.Arrival)
	if err != nil {
		t.Fatalf("arr is not base64: %v", err)
	}
	if res.DimX <= 0 || len(arr) != res.DimX*res.DimY*res.DimZ {
		t.Fatalf("%d arrival bytes for a %dx%dx%d box", len(arr), res.DimX, res.DimY, res.DimZ)
	}
}

func TestSmokeVoxelsEndpointRejects(t *testing.T) {
	buf := recordedVoxelLog(t)
	log := base64.StdEncoding.EncodeToString(buf)
	det := map[string]float64{"x": -100, "y": 1250, "z": 100}
	cases := []struct {
		name string
		body any
		want int
	}{
		{"empty log", map[string]any{"log": "", "samples": [][2]int{}, "detonation": det}, http.StatusUnprocessableEntity},
		{"no detonation", map[string]any{"log": log, "samples": [][2]int{{len(buf), 0}}}, http.StatusUnprocessableEntity},
		{"bad base64", map[string]any{"log": "not base64!", "samples": [][2]int{{8, 0}}, "detonation": det}, http.StatusBadRequest},
		{"sample past the log", map[string]any{"log": log, "samples": [][2]int{{len(buf) + 1, 0}}, "detonation": det}, http.StatusBadRequest},
		{"tick goes back", map[string]any{"log": log, "samples": [][2]int{{10, 5}, {len(buf), 2}}, "detonation": det}, http.StatusBadRequest},
		{"oversized body", map[string]any{"log": log, "pad": strings.Repeat("x", maxSmokeVoxelsBody)}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := post(t, handleSmokeVoxels, tc.body)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			smokeVoxelsErrorOf(t, w)
		})
	}

	t.Run("bad json", func(t *testing.T) {
		w := httptest.NewRecorder()
		handleSmokeVoxels(w, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte("{"))))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", w.Code)
		}
		smokeVoxelsErrorOf(t, w)
	})
	t.Run("method", func(t *testing.T) {
		w := httptest.NewRecorder()
		handleSmokeVoxels(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status %d, want 405", w.Code)
		}
	})
}
