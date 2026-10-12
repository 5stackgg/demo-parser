package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/5stackgg/demo-parser/internal/parser"
	"github.com/golang/geo/r3"
)

// POST /smoke-voxels — a smoke rebuilt from the voxel log a server plugin read
// live off its projectile, for lineups thrown on a practice server. It runs
// the same reconstruction as a demo's smoke_voxels, so the answer is a row of
// that shape.

// maxSmokeVoxelsBody caps the request body. The log is at most 3072 bytes, 4 KB
// of base64, and grows at most once a tick, so even a sample for every byte
// stays well inside this.
const maxSmokeVoxelsBody = 64 << 10

// smokeVoxelsRequest is the log as the plugin last read it. Samples are
// [log length, ticks after detonation] for each tick the log grew.
type smokeVoxelsRequest struct {
	Log        []byte       `json:"log"`
	Samples    [][2]int     `json:"samples"`
	Detonation parser.Point `json:"detonation"`
	TickRate   float64      `json:"tick_rate"`
}

func handleSmokeVoxels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		smokeVoxelsError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req smokeVoxelsRequest
	// []byte decodes from base64, so a bad log fails here too.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSmokeVoxelsBody)).Decode(&req); err != nil {
		smokeVoxelsError(w, http.StatusBadRequest, fmt.Sprintf("bad request: %v", err))
		return
	}
	det := r3.Vector{X: req.Detonation.X, Y: req.Detonation.Y, Z: req.Detonation.Z}
	out, err := parser.ReconstructSmokeVoxels(req.Log, req.Samples, det, req.TickRate)
	switch {
	case errors.Is(err, parser.ErrNoSmokeVoxels):
		smokeVoxelsError(w, http.StatusUnprocessableEntity, err.Error())
		return
	case err != nil:
		smokeVoxelsError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, out)
}

// smokeVoxelsError answers in JSON, so the API can pass the reason through.
func smokeVoxelsError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{msg}); err != nil {
		log.Printf("[smoke-voxels] response encode: %v", err)
	}
}
