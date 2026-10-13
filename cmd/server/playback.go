package main

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/5stackgg/demo-parser/internal/parser"
	"github.com/klauspost/compress/gzip"
)

// playbackRequest asks /parse to build the demo's playback blob as well. The
// api sends the match map the blob belongs to and the blob version it reads;
// a version this parser does not build is ignored, and the response is the
// full parse the api builds the blob from itself.
type playbackRequest struct {
	MatchMapID    string `json:"match_map_id"`
	SchemaVersion int    `json:"schema_version"`
}

func (p *playbackRequest) wanted() bool {
	return p != nil && p.MatchMapID != "" && p.SchemaVersion == parser.PlaybackSchemaVersion
}

// playbackResponse is the gzipped blob, base64 in the JSON, plus the counts
// the api logs for it.
type playbackResponse struct {
	SchemaVersion int    `json:"schema_version"`
	MatchMapID    string `json:"match_map_id"`
	Blob          []byte `json:"blob"`
	Positions     int    `json:"positions"`
	ShotsFired    int    `json:"shots_fired"`
	GrenadeEvents int    `json:"grenade_events"`
	SmokeVolumes  int    `json:"smoke_volumes"`
	SmokeVoxels   int    `json:"smoke_voxels"`
	Infernos      int    `json:"infernos"`
	Damages       int    `json:"damages"`
	Flashes       int    `json:"flashes"`
}

// parseResponse is the parse result, with the blob when one was asked for.
type parseResponse struct {
	*parser.Result
	Playback *playbackResponse `json:"playback,omitempty"`
}

// buildPlayback builds and compresses the blob, then drops from the result
// what only the blob needed: those fields are ~95% of a parse, and the api
// would otherwise receive and decode them for nothing.
func buildPlayback(result *parser.Result, req *playbackRequest) (*playbackResponse, time.Duration, error) {
	start := time.Now()
	blob := parser.BuildPlayback(result, req.MatchMapID)

	var gz bytes.Buffer
	// Every replay viewer downloads this, so size matters more than the few
	// hundred ms: level 7 comes out a little smaller than the api's zlib
	// default did (5.36 MB against 5.41 MB on a 66 MB de_anubis blob), where
	// the default here is 10% bigger.
	zw, err := gzip.NewWriterLevel(&gz, 7)
	if err != nil {
		return nil, 0, err
	}
	enc := json.NewEncoder(zw)
	// The api's JSON.stringify leaves <, > and & alone; so does this, so a
	// player name reads the same in either blob.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(blob); err != nil {
		return nil, 0, err
	}
	if err := zw.Close(); err != nil {
		return nil, 0, err
	}

	out := &playbackResponse{
		SchemaVersion: blob.SchemaVersion,
		MatchMapID:    blob.MatchMapID,
		Blob:          gz.Bytes(),
		Positions:     len(blob.Positions),
		ShotsFired:    len(blob.ShotsFired),
		GrenadeEvents: len(blob.GrenadeThrows),
		SmokeVolumes:  len(blob.SmokeVolumes),
		SmokeVoxels:   len(blob.SmokeVoxels),
		Infernos:      len(blob.Infernos),
		Damages:       len(blob.Damages),
		Flashes:       len(blob.Flashes),
	}
	result.Positions = nil
	result.GrenadeTrajectories = nil
	result.SmokeVolumes = nil
	result.SmokeVoxels = nil
	result.Infernos = nil
	result.KitDrops = nil
	return out, time.Since(start), nil
}
