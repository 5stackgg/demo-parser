package parser

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"sync"
	"testing"

	dem "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"
)

// testDemo is the de_inferno demo kept beside the repo root (gitignored).
// TEST_DEMO points elsewhere, e.g. from a worktree.
func testDemo(t *testing.T) string {
	t.Helper()
	path := os.Getenv("TEST_DEMO")
	if path == "" {
		path = filepath.Join("..", "..", "test.dem")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no demo at %s", path)
	}
	return path
}

// test.dem's smokes, by detonation tick. All three are thrown after a round
// is decided and removed by the next round start, so each log is cut off
// 4–6 s in.
var testDemoSmokes = []struct{ start, end int }{
	{4019, 4381},
	{12642, 12919},
	{14690, 14948},
}

var (
	testDemoOnce   sync.Once
	testDemoResult *Result
	testDemoErr    error
)

// parseTestDemo parses test.dem once for every test that reads the result.
// No mesh, so the parse stays offline; nothing read from it needs one.
func parseTestDemo(t *testing.T) *Result {
	t.Helper()
	path := testDemo(t)
	testDemoOnce.Do(func() {
		t.Setenv("MAP_MESH_CDN", "")
		f, err := os.Open(path)
		if err != nil {
			testDemoErr = err
			return
		}
		defer f.Close()
		testDemoResult, testDemoErr = Parse(f)
	})
	if testDemoErr != nil {
		t.Fatal(testDemoErr)
	}
	return testDemoResult
}

func TestParseExportsSmokeVoxels(t *testing.T) {
	res := parseTestDemo(t)
	if res.SchemaVersion != 3 {
		t.Errorf("schema_version %d, want 3", res.SchemaVersion)
	}
	if len(res.SmokeVoxels) != len(testDemoSmokes) {
		t.Fatalf("%d smoke_voxels, want %d", len(res.SmokeVoxels), len(testDemoSmokes))
	}
	for i, sv := range res.SmokeVoxels {
		want := testDemoSmokes[i]
		if sv.StartTick != want.start || sv.EndTick != want.end {
			t.Errorf("smoke %d spans %d–%d, want %d–%d", i, sv.StartTick, sv.EndTick, want.start, want.end)
		}
		if sv.GrenadeID == 0 || sv.Round == 0 {
			t.Errorf("smoke %d has gid %d round %d", i, sv.GrenadeID, sv.Round)
		}
		if sv.VoxelSize != 20 || sv.DimX > voxelGridDim || sv.DimY > voxelGridDim || sv.DimZ > voxelGridDim {
			t.Fatalf("smoke %d box %dx%dx%d vs=%v", i, sv.DimX, sv.DimY, sv.DimZ, sv.VoxelSize)
		}
		raw, err := base64.StdEncoding.DecodeString(sv.Arrival)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) != sv.DimX*sv.DimY*sv.DimZ {
			t.Fatalf("smoke %d: %d bytes for a %dx%dx%d box", i, len(raw), sv.DimX, sv.DimY, sv.DimZ)
		}
		filled, first := 0, 255
		for _, b := range raw {
			if b != arrNever {
				filled++
				first = min(first, int(b))
			}
		}
		if filled < 400 || filled > 1600 {
			t.Errorf("smoke %d holds %d voxels; a cloud is several hundred to a little over a thousand", i, filled)
		}
		if first != 0 {
			t.Errorf("smoke %d first fills at %d, want 0 (the burst, on detonation)", i, first)
		}
		t.Logf("smoke %d: gid=%d round=%d box %dx%dx%d at (%.0f,%.0f,%.0f), %d voxels, %d bytes, %d base64",
			i, sv.GrenadeID, sv.Round, sv.DimX, sv.DimY, sv.DimZ, sv.OriginX, sv.OriginY, sv.OriginZ,
			filled, len(raw), len(sv.Arrival))
	}

	// The playback blob passes damage detail through; test.dem throws no
	// flashbangs, so flashes go unchecked here.
	armor := 0
	for _, d := range res.Damages {
		if d.Weapon == "" || d.Hitgroup == 0 {
			t.Fatalf("damage at %d lacks weapon or hitgroup: %+v", d.Tick, d)
		}
		if d.DamageArmor > 0 {
			armor++
		}
	}
	if armor == 0 {
		t.Error("no damage carries armor damage")
	}
}

// collectVoxelTracks reads every smoke's voxel log off a demo, the way the
// parser does, and keeps them for inspection. samples holds, per track, the
// log length and detonation-relative tick of every poll that grew it: what a
// server plugin polling the projectile would send.
func collectVoxelTracks(t *testing.T, path string) (tracks []*smokeVoxelTrack, samples map[*smokeVoxelTrack][][2]int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p := dem.NewParser(f)
	defer p.Close()
	live := map[int]*smokeVoxelTrack{}
	samples = map[*smokeVoxelTrack][][2]int{}
	p.RegisterEventHandler(func(e events.SmokeStart) {
		if ent := p.GameState().Entities()[e.GrenadeEntityID]; ent != nil {
			tr := newSmokeVoxelTrack(ent, 0, 0, p.GameState().IngameTick())
			live[e.GrenadeEntityID] = tr
			tracks = append(tracks, tr)
		}
	})
	p.RegisterEventHandler(func(events.FrameDone) {
		tick := p.GameState().IngameTick()
		for id, tr := range live {
			if p.GameState().Entities()[id] != tr.entity {
				delete(live, id)
				continue
			}
			had := len(tr.buf)
			tr.poll(tick)
			if len(tr.buf) > had {
				samples[tr] = append(samples[tr], [2]int{len(tr.buf), tick - tr.startTick})
			}
		}
	})
	// test.dem ends mid-packet; everything before that is intact.
	_ = p.ParseToEnd()
	return tracks, samples
}

// The live endpoint gets the log a plugin read off the projectile and the
// ticks it grew on. Given exactly what the demo path read, it must rebuild the
// cloud the playback blob carries.
func TestReconstructSmokeVoxelsMatchesTheDemo(t *testing.T) {
	res := parseTestDemo(t)
	tracks, samples := collectVoxelTracks(t, testDemo(t))
	if len(tracks) != len(res.SmokeVoxels) {
		t.Fatalf("%d tracks, %d exported smokes", len(tracks), len(res.SmokeVoxels))
	}
	timed := 0
	for i, tr := range tracks {
		want := res.SmokeVoxels[i]
		if tr.startTick != want.StartTick {
			t.Fatalf("track %d starts at %d, exported smoke at %d", i, tr.startTick, want.StartTick)
		}
		s := samples[tr]
		if len(s) < 2 || s[0][1] != 0 || s[len(s)-1][0] != len(tr.buf) {
			t.Fatalf("smoke %d: samples %v for a %d-byte log", want.StartTick, s, len(tr.buf))
		}
		got, err := ReconstructSmokeVoxels(tr.buf, s, tr.det, res.TickRate)
		if err != nil {
			t.Fatalf("smoke %d: %v", want.StartTick, err)
		}
		if got.OriginX != want.OriginX || got.OriginY != want.OriginY || got.OriginZ != want.OriginZ ||
			got.VoxelSize != want.VoxelSize ||
			got.DimX != want.DimX || got.DimY != want.DimY || got.DimZ != want.DimZ {
			t.Fatalf("smoke %d: box %+v, want %+v", want.StartTick, got, want)
		}
		if got.Arrival != want.Arrival {
			t.Fatalf("smoke %d: arrival differs from the demo's", want.StartTick)
		}
		if got.GrenadeID != 0 || got.Round != 0 || got.StartTick != 0 || got.EndTick != s[len(s)-1][1] {
			t.Errorf("smoke %d: gid %d round %d ticks %d–%d, want 0 0 0–%d",
				want.StartTick, got.GrenadeID, got.Round, got.StartTick, got.EndTick, s[len(s)-1][1])
		}
		t.Logf("smoke %d: %d bytes over %d samples, last at tick %d", want.StartTick, len(tr.buf), len(s), s[len(s)-1][1])

		// The whole log on the detonation tick has the same shape; only the
		// timing, which comes from when each frame arrived, can differ.
		at0, err := ReconstructSmokeVoxels(tr.buf, [][2]int{{len(tr.buf), 0}}, tr.det, res.TickRate)
		if err != nil {
			t.Fatal(err)
		}
		if at0.DimX != want.DimX || at0.DimY != want.DimY || at0.DimZ != want.DimZ {
			t.Errorf("smoke %d: one sample at tick 0 should keep the box", want.StartTick)
		}
		if at0.Arrival != want.Arrival {
			timed++
		}
	}
	if timed == 0 {
		t.Error("no smoke's timing depends on its samples; the test would pass without them")
	}
}
