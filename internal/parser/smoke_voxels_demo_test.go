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
		if filled < defaultVoxelFlood.budgetMin || filled > defaultVoxelFlood.budgetMax {
			t.Errorf("smoke %d holds %d voxels, outside the fitted range", i, filled)
		}
		if first != 0 {
			t.Errorf("smoke %d first fills at %d, want 0 (its sources, on detonation)", i, first)
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
// parser does, and keeps them for inspection.
func collectVoxelTracks(t *testing.T, path string) []*smokeVoxelTrack {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p := dem.NewParser(f)
	defer p.Close()
	live := map[int]*smokeVoxelTrack{}
	var all []*smokeVoxelTrack
	p.RegisterEventHandler(func(e events.SmokeStart) {
		if ent := p.GameState().Entities()[e.GrenadeEntityID]; ent != nil {
			tr := newSmokeVoxelTrack(ent, 0, 0, p.GameState().IngameTick())
			live[e.GrenadeEntityID] = tr
			all = append(all, tr)
		}
	})
	p.RegisterEventHandler(func(events.FrameDone) {
		tick := p.GameState().IngameTick()
		for id, tr := range live {
			if p.GameState().Entities()[id] != tr.entity {
				delete(live, id)
				continue
			}
			tr.poll(tick)
		}
	})
	// test.dem ends mid-packet; everything before that is intact.
	_ = p.ParseToEnd()
	return all
}

// TestSmokeVoxelCalibration is where defaultVoxelFlood's numbers come from.
// It scores the reconstruction by the one ground truth the demo carries — the
// bricks the server sent, which are the bricks its cloud occupied or bordered —
// as intersection over union with the bricks ours occupies or borders.
//
// On test.dem (three de_inferno smokes, 52/68/76 bricks):
//
//	fixed budget  700: 0.909 0.824 0.737  mean 0.823
//	fixed budget  800: 0.862 0.882 0.756  mean 0.834  ← best single budget
//	fixed budget  900: 0.758 0.884 0.825  mean 0.822
//	fixed budget 1000: 0.704 0.900 0.821  mean 0.809
//	fitted per smoke:  0.923 0.900 0.838  mean 0.887  (budgets 646/994/910)
//
// The best budgets spread 610–1000, so no single size is right and each smoke
// is fitted inside 600–1100 (the spread seen here, padded), preferring the
// size nearest 800 where several match equally. The breadth-first shape beats
// ordering the flood by brick arrival (0.77–0.83 mean at its best), which is
// why arrival only sets timing.
//
// The step is set from growth: the server sent 47–58% of each cloud's bricks
// on the detonation frame, 50–71% by 16 ticks and 72–90% by 32. At six ticks
// a voxel the fitted clouds are 40–50% filled by 16 ticks and 96–100% by 32;
// at two or four ticks most of the cloud is in by 16, ahead of the server.
func TestSmokeVoxelCalibration(t *testing.T) {
	tracks := collectVoxelTracks(t, testDemo(t))
	if len(tracks) != len(testDemoSmokes) {
		t.Fatalf("%d smokes, want %d", len(tracks), len(testDemoSmokes))
	}
	p := defaultVoxelFlood
	var fixedSum, fittedSum float64
	for _, tr := range tracks {
		l := tr.log
		order := l.fillOrder(p.budgetMax)
		fixed := l.brickIoU(order[:p.budget])
		budget, fitted := l.fitBudget(order, p)
		fixedSum += fixed
		fittedSum += fitted

		sent := 0
		for _, bt := range l.brickTick {
			if bt >= 0 {
				sent++
			}
		}
		arrival := l.timeShape(order[:budget], int(p.stepSecs*64+0.5))
		var by16, by32, last int
		for _, a := range arrival {
			if a < 0 {
				continue
			}
			if a <= 16 {
				by16++
			}
			if a <= 32 {
				by32++
			}
			last = max(last, a)
		}
		t.Logf("smoke %d: %d frames, %d bricks; fixed %d → IoU %.3f; fitted %d → IoU %.3f; filled %.0f%% by 16 ticks, %.0f%% by 32, last at %d",
			tr.startTick, l.frames, sent, p.budget, fixed, budget, fitted,
			100*float64(by16)/float64(budget), 100*float64(by32)/float64(budget), last)

		if budget <= p.budgetMin || budget >= p.budgetMax {
			t.Errorf("smoke %d fitted to the edge of the range (%d); widen it", tr.startTick, budget)
		}
	}
	n := float64(len(tracks))
	t.Logf("mean IoU: fixed %.3f, fitted %.3f", fixedSum/n, fittedSum/n)
	if fittedSum/n < 0.85 {
		t.Errorf("fitted clouds match the server's bricks at %.3f, want ≥ 0.85", fittedSum/n)
	}
}
