package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/5stackgg/demo-parser/internal/parser"
	"github.com/klauspost/compress/gzip"
)

func TestPlaybackIsBuiltOnlyForTheVersionThisParserMakes(t *testing.T) {
	cases := []struct {
		req  *playbackRequest
		want bool
	}{
		{nil, false},
		{&playbackRequest{MatchMapID: "mm", SchemaVersion: parser.PlaybackSchemaVersion}, true},
		{&playbackRequest{MatchMapID: "mm", SchemaVersion: parser.PlaybackSchemaVersion + 1}, false},
		{&playbackRequest{SchemaVersion: parser.PlaybackSchemaVersion}, false},
	}
	for _, c := range cases {
		if got := c.req.wanted(); got != c.want {
			t.Errorf("wanted(%+v) = %v, want %v", c.req, got, c.want)
		}
	}
}

// The blob carries what the parse had, and the response then leaves out what
// only the blob needed while keeping everything the api persists.
func TestBuildPlaybackCompressesTheBlobAndSlimsTheResponse(t *testing.T) {
	res := &parser.Result{
		TickRate:            64,
		MapName:             "de_anubis",
		Kills:               []parser.EventKill{{Tick: 5}},
		ShotsFired:          []parser.EventShotFired{{Tick: 6}},
		Players:             []parser.PlayerInfo{{SteamID: "7656", Name: "<3 & co"}},
		Positions:           []parser.EventPosition{{Tick: 7}},
		GrenadeTrajectories: []parser.GrenadeTrajectory{{GrenadeID: 1}},
		SmokeVolumes:        []parser.EventSmokeVolume{{}},
		SmokeVoxels:         []parser.EventSmokeVoxels{{}},
		Infernos:            []parser.EventInferno{{ID: 63976326766372499}},
		KitDrops:            []parser.EventKitDrop{{}},
	}
	pb, _, err := buildPlayback(res, &playbackRequest{MatchMapID: "mm-1", SchemaVersion: parser.PlaybackSchemaVersion})
	if err != nil {
		t.Fatal(err)
	}

	zr, err := gzip.NewReader(bytes.NewReader(pb.Blob))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	var blob map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blob); err != nil {
		t.Fatal(err)
	}
	if string(blob["match_map_id"]) != `"mm-1"` || len(blob["positions"]) < 10 {
		t.Fatalf("blob is missing what the parse had: %s", raw)
	}
	if !strings.Contains(string(raw), `"id":63976326766372499`) {
		t.Error("an inferno id should survive at full precision")
	}
	// The api's JSON.stringify left these characters alone; so must this.
	if !strings.Contains(string(raw), `"name":"<3 & co"`) {
		t.Errorf("a player name was escaped: %s", raw)
	}
	if pb.Positions != 1 || pb.ShotsFired != 1 {
		t.Errorf("counts %+v", pb)
	}

	out, err := json.Marshal(parseResponse{Result: res, Playback: pb})
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]json.RawMessage
	_ = json.Unmarshal(out, &resp)
	for _, gone := range []string{"positions", "grenade_trajectories", "smoke_volumes", "smoke_voxels", "infernos", "kit_drops"} {
		if _, ok := resp[gone]; ok {
			t.Errorf("response still carries %s", gone)
		}
	}
	for _, kept := range []string{"kills", "shots_fired", "map_name", "playback"} {
		if _, ok := resp[kept]; !ok {
			t.Errorf("response lost %s", kept)
		}
	}
}
