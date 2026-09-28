package simulate

import (
	"math"
	"math/rand"
	"testing"
)

func seedAt(id string, px, py, pz, vx, vy, vz float64, nade string) LineupSeed {
	return LineupSeed{
		ID:              id,
		NadeType:        nade,
		InitialPosition: &Point{X: px, Y: py, Z: pz},
		InitialVelocity: &Point{X: vx, Y: vy, Z: vz},
	}
}

func runDrift(t *testing.T, from, to *meshPair, lineups []LineupSeed, mut func(*DriftRequest)) DriftResponse {
	t.Helper()
	req := DriftRequest{Map: "de_test", From: from.ref, To: to.ref, Lineups: lineups}
	if mut != nil {
		mut(&req)
	}
	res, err := Drift(from.mesh, to.mesh, req, DriftOptions{Workers: 4})
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	if len(res.Results) != len(lineups) {
		t.Fatalf("got %d results for %d lineups", len(res.Results), len(lineups))
	}
	return res
}

// THE PROPERTY THE WHOLE DESIGN RESTS ON.
//
// If the same geometry is on both sides of the comparison, nothing may be
// reported as having moved or broken — whatever the physics constants are, and
// whatever the throw is. The model's accuracy is irrelevant to this: an error
// in the constants lands identically on both sides and subtracts out. If this
// ever fails, every drift report this service has ever produced is noise.
func TestSameGeometryOnBothSidesNeverReportsDrift(t *testing.T) {
	tris := terrain()
	left := newMeshPair(t, tris)
	right := newMeshPair(t, tris)

	rng := rand.New(rand.NewSource(20260818))
	for trial := 0; trial < 12; trial++ {
		c := randomConstants(rng)
		if err := c.Validate(); err != nil {
			t.Fatalf("trial %d generated invalid constants %+v: %v", trial, c, err)
		}
		lineups := randomLineups(rng, 24)
		res := runDrift(t, left, right, lineups, func(req *DriftRequest) {
			req.Constants = overridesFrom(c)
		})
		for _, d := range res.Results {
			switch d.Verdict {
			case VerdictMoved, VerdictBroken:
				t.Fatalf("trial %d lineup %d: identical meshes reported %q (%s)\n from %+v\n to %+v",
					trial, d.Index, d.Verdict, d.Reason, d.From, d.To)
			}
			if d.Distance != nil && *d.Distance != 0 {
				t.Fatalf("trial %d lineup %d: identical meshes moved the landing by %v",
					trial, d.Index, *d.Distance)
			}
		}
		if res.Summary.Moved != 0 || res.Summary.Broken != 0 || res.Summary.MaxDistance != 0 {
			t.Fatalf("trial %d summary reports drift against identical meshes: %+v", trial, res.Summary)
		}
	}
}

// The same, against a real shipped mesh. A synthetic box has a handful of
// triangles and no seams worth grazing; de_mirage has hundreds of thousands,
// and a bounce off one crosses exactly the kind of boundary where a sloppy
// raycaster would answer differently between two builds.
func TestSameRealMeshNeverReportsDrift(t *testing.T) {
	base := realMeshRevision(t)
	other := realMeshRevision(t)
	from := loadMesh(t, base, "de_mirage")
	to := loadMesh(t, other, "de_mirage")
	if from.Hull() == to.Hull() {
		t.Fatal("test setup: wanted two separately built meshes")
	}

	// Around Mirage's A site, thrown in every direction so the flights land all
	// over the map rather than all in one open area.
	rng := rand.New(rand.NewSource(7))
	lineups := make([]LineupSeed, 0, 64)
	for i := 0; i < 64; i++ {
		a := rng.Float64() * 2 * math.Pi
		lineups = append(lineups, seedAt(
			"mirage", -2300, 0, -64,
			700*math.Cos(a), 700*math.Sin(a), rng.Float64()*400-100,
			[]string{"Smoke", "HE", "Flash", "Molotov"}[i%4],
		))
	}
	res, err := Drift(from, to, DriftRequest{
		Map: "de_mirage", From: base, To: other, Lineups: lineups,
	}, DriftOptions{Workers: 4})
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	if res.Summary.Moved != 0 || res.Summary.Broken != 0 {
		for _, d := range res.Results {
			if d.Verdict == VerdictMoved || d.Verdict == VerdictBroken {
				t.Errorf("lineup %d on identical real meshes: %s (%s) from %+v to %+v",
					d.Index, d.Verdict, d.Reason, d.From, d.To)
			}
		}
		t.Fatalf("identical de_mirage meshes reported drift: %+v", res.Summary)
	}
	if res.Summary.Unchanged == 0 {
		t.Fatal("no lineup resolved on de_mirage; the test is not exercising anything")
	}
	t.Logf("de_mirage self-comparison: %+v", res.Summary)
}

// A map update that raises the ground under a lineup moves where it lands.
func TestNewGeometryMovesTheLanding(t *testing.T) {
	from := newMeshPair(t, terrain())
	to := newMeshPair(t, terrainWithPlatform())
	lineups := []LineupSeed{seedAt("onto-the-platform", 0, 0, 64, 500, 0, 200, "Smoke")}

	res := runDrift(t, from, to, lineups, nil)
	d := res.Results[0]
	if d.Verdict != VerdictMoved {
		t.Fatalf("a platform under the landing should move it, got %q (%s)\n from %+v\n to %+v",
			d.Verdict, d.Reason, d.From, d.To)
	}
	if d.Distance == nil || *d.Distance < res.Thresholds.Unchanged {
		t.Fatalf("distance %v does not clear the unchanged threshold", d.Distance)
	}
	if d.From.ComparisonPoint.Z > 8 {
		t.Fatalf("the old landing should be on the ground, got z=%v", d.From.ComparisonPoint.Z)
	}
	if d.To.ComparisonPoint.Z < 56 {
		t.Fatalf("the new landing should be on top of the 64-unit platform, got z=%v", d.To.ComparisonPoint.Z)
	}
	if d.Severity == "" {
		t.Fatal("a moved lineup should carry a severity")
	}
	if res.Summary.Moved != 1 || res.Summary.Unchanged != 0 {
		t.Fatalf("summary %+v", res.Summary)
	}
}

// The floor under a lineup is removed: the grenade now falls out of the map and
// the lineup no longer resolves at all.
func TestRemovedFloorBreaksTheLineup(t *testing.T) {
	// A floor with a rectangular hole, and the same floor with the hole
	// patched. The patch is the only difference between the two meshes.
	holed := floorWithHole()
	whole := append(append([]tri(nil), holed...), floorQuad(1200, 0, 0, 400)...)

	from := newMeshPair(t, whole)
	to := newMeshPair(t, holed)
	lineups := []LineupSeed{seedAt("into-the-hole", 0, 0, 64, 500, 0, 200, "Smoke")}

	res := runDrift(t, from, to, lineups, nil)
	d := res.Results[0]
	if d.Verdict != VerdictBroken {
		t.Fatalf("a lineup landing in a new hole should be broken, got %q (%s)\n from %+v\n to %+v",
			d.Verdict, d.Reason, d.From, d.To)
	}
	if d.To.Stop != StopOutOfWorld {
		t.Fatalf("expected the flight to leave the map, got %q", d.To.Stop)
	}
	if d.Distance != nil {
		t.Fatalf("a broken lineup has no meaningful distance, got %v", *d.Distance)
	}
	if res.Summary.Broken != 1 {
		t.Fatalf("summary %+v", res.Summary)
	}
}

// A map update that builds something where the thrower stood.
func TestSealedThrowPositionBreaksTheLineup(t *testing.T) {
	before := terrain()
	after := append(append([]tri(nil), before...),
		box(pt(-6, -6, 58), pt(6, 6, 70))...)

	from := newMeshPair(t, before)
	to := newMeshPair(t, after)
	res := runDrift(t, from, to, []LineupSeed{seedAt("walled-in", 0, 0, 64, 500, 0, 200, "Smoke")}, nil)

	d := res.Results[0]
	if d.Verdict != VerdictBroken || d.To.Stop != StopStartSealed {
		t.Fatalf("a throw spot filled in by the update should be broken, got %q (%s) stop %q",
			d.Verdict, d.Reason, d.To.Stop)
	}
}

// A landing that ends up buried in the new geometry. The enclosure probe is
// raised for this one: at its default a pocket tight enough to trap a grenade
// is also too tight for one to fly into, so the mechanism is exercised at a
// scale a test can actually build.
func TestLandingInsideNewGeometryBreaksTheLineup(t *testing.T) {
	from := newMeshPair(t, chamber(false))
	to := newMeshPair(t, chamber(true))
	probe := 320.0
	res := runDrift(t, from, to, []LineupSeed{seedAt("roofed-in", -100, 0, 125, 500, 0, 0, "Smoke")},
		func(req *DriftRequest) {
			req.Constants = &ConstantOverrides{EnclosureProbe: &probe}
		})

	d := res.Results[0]
	if d.Verdict != VerdictBroken || d.To.Stop != StopInsideGeometry {
		t.Fatalf("a landing with no space left around it should be broken as inside geometry, got %q (%s) stop %q",
			d.Verdict, d.Reason, d.To.Stop)
	}
	if !d.From.Resolved {
		t.Fatalf("the lineup should have worked before the roof went on: %+v", d.From)
	}
}

// Most lineups in the library were mined out of demos and have no recorded
// throw. Reporting those as unchanged would be a quiet lie — they were never
// checked — and inventing a throw for them would be a loud one.
func TestLineupWithNoSeedIsUnsimulatable(t *testing.T) {
	from := newMeshPair(t, terrain())
	to := newMeshPair(t, terrain())
	lineups := []LineupSeed{
		{ID: "no-seed-at-all", NadeType: "Smoke"},
		{ID: "position-only", NadeType: "Smoke", InitialPosition: &Point{Z: 64}},
		{ID: "velocity-only", NadeType: "Smoke", InitialVelocity: &Point{X: 500}},
		seedAt("fine", 0, 0, 64, 500, 0, 200, "Smoke"),
	}
	res := runDrift(t, from, to, lineups, nil)

	for _, d := range res.Results[:3] {
		if d.Verdict != VerdictUnsimulatable {
			t.Fatalf("lineup %q with no seed should be unsimulatable, got %q", d.ID, d.Verdict)
		}
		if d.From != nil || d.To != nil {
			t.Fatalf("lineup %q never flew, so it should carry no outcomes", d.ID)
		}
		if d.Reason == "" {
			t.Fatalf("lineup %q should say why it could not be simulated", d.ID)
		}
	}
	if res.Results[3].Verdict != VerdictUnchanged {
		t.Fatalf("the seeded lineup should still have been answered, got %q", res.Results[3].Verdict)
	}
	if res.Summary.Unsimulatable != 3 || res.Summary.Unchanged != 1 {
		t.Fatalf("summary %+v", res.Summary)
	}
}

func TestUnknownNadeTypeIsUnsimulatable(t *testing.T) {
	from := newMeshPair(t, terrain())
	to := newMeshPair(t, terrain())
	res := runDrift(t, from, to, []LineupSeed{seedAt("what", 0, 0, 64, 500, 0, 200, "banana")}, nil)
	if res.Results[0].Verdict != VerdictUnsimulatable {
		t.Fatalf("an unknown grenade should be unsimulatable, got %q", res.Results[0].Verdict)
	}
}

// A flight that fails on BOTH meshes says nothing about the map, so it is not
// "broken" — it is a seed or a model this service cannot answer for.
func TestFailingOnBothMeshesIsUnsimulatableNotBroken(t *testing.T) {
	from := newMeshPair(t, floorQuad(0, 0, 0, 200))
	to := newMeshPair(t, floorQuad(0, 0, 0, 200))
	res := runDrift(t, from, to, []LineupSeed{seedAt("off-the-edge", 0, 0, 64, 900, 0, 100, "Smoke")}, nil)
	d := res.Results[0]
	if d.Verdict != VerdictUnsimulatable {
		t.Fatalf("a flight that fails on both sides should be unsimulatable, got %q (%s)", d.Verdict, d.Reason)
	}
	if d.From == nil || d.To == nil {
		t.Fatal("both flights ran, so both outcomes should be reported")
	}
}

// Streaming must produce exactly the buffered answer, in the same order — the
// two paths differ only in where the results are written.
func TestStreamingMatchesTheBufferedAnswer(t *testing.T) {
	from := newMeshPair(t, terrain())
	to := newMeshPair(t, terrainWithPlatform())

	rng := rand.New(rand.NewSource(99))
	lineups := randomLineups(rng, chunkSize*2+7)
	req := DriftRequest{Map: "de_test", From: from.ref, To: to.ref, Lineups: lineups}

	buffered, err := Drift(from.mesh, to.mesh, req, DriftOptions{Workers: 4})
	if err != nil {
		t.Fatalf("buffered: %v", err)
	}

	var streamed []LineupDrift
	stream, err := Drift(from.mesh, to.mesh, req, DriftOptions{
		Workers: 4,
		Emit: func(batch []LineupDrift) error {
			streamed = append(streamed, batch...)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("streamed: %v", err)
	}
	if len(stream.Results) != 0 {
		t.Fatal("a streamed run should not also buffer its results")
	}
	if stream.Summary != buffered.Summary {
		t.Fatalf("summaries differ:\n buffered %+v\n streamed %+v", buffered.Summary, stream.Summary)
	}
	if len(streamed) != len(buffered.Results) {
		t.Fatalf("streamed %d results, buffered %d", len(streamed), len(buffered.Results))
	}
	for i := range streamed {
		if streamed[i].Index != i {
			t.Fatalf("result %d is out of order (index %d)", i, streamed[i].Index)
		}
		if !sameDrift(streamed[i], buffered.Results[i]) {
			t.Fatalf("result %d differs:\n streamed %+v\n buffered %+v", i, streamed[i], buffered.Results[i])
		}
	}
	if buffered.Summary.Moved == 0 {
		t.Fatal("the two meshes differ, so something should have moved")
	}
}

// The worker pool changes throughput and nothing else.
func TestWorkerCountDoesNotChangeTheAnswer(t *testing.T) {
	from := newMeshPair(t, terrain())
	to := newMeshPair(t, terrainWithPlatform())
	rng := rand.New(rand.NewSource(4))
	req := DriftRequest{Map: "de_test", From: from.ref, To: to.ref, Lineups: randomLineups(rng, 40)}

	one, err := Drift(from.mesh, to.mesh, req, DriftOptions{Workers: 1})
	if err != nil {
		t.Fatalf("workers=1: %v", err)
	}
	many, err := Drift(from.mesh, to.mesh, req, DriftOptions{Workers: 16})
	if err != nil {
		t.Fatalf("workers=16: %v", err)
	}
	if one.Summary != many.Summary {
		t.Fatalf("summaries differ by worker count:\n 1 %+v\n 16 %+v", one.Summary, many.Summary)
	}
	for i := range one.Results {
		if !sameDrift(one.Results[i], many.Results[i]) {
			t.Fatalf("result %d differs by worker count:\n 1 %+v\n 16 %+v", i, one.Results[i], many.Results[i])
		}
	}
}

func TestDriftRejectsWhatItCannotAnswer(t *testing.T) {
	pair := newMeshPair(t, terrain())
	good := []LineupSeed{seedAt("a", 0, 0, 64, 500, 0, 200, "Smoke")}

	if _, err := Drift(nil, pair.mesh, DriftRequest{Lineups: good}, DriftOptions{}); err == nil {
		t.Error("a missing from-mesh should be an error")
	}
	if _, err := Drift(pair.mesh, nil, DriftRequest{Lineups: good}, DriftOptions{}); err == nil {
		t.Error("a missing to-mesh should be an error")
	}
	if _, err := Drift(pair.mesh, pair.mesh, DriftRequest{}, DriftOptions{}); err == nil {
		t.Error("an empty batch should be an error")
	}
	bad := -1.0
	if _, err := Drift(pair.mesh, pair.mesh, DriftRequest{
		Lineups: good, UnchangedRadius: &bad,
	}, DriftOptions{}); err == nil {
		t.Error("a negative threshold should be an error")
	}
	zero := 0.0
	if _, err := Drift(pair.mesh, pair.mesh, DriftRequest{
		Lineups: good, Constants: &ConstantOverrides{TimeStep: &zero},
	}, DriftOptions{}); err == nil {
		t.Error("a zero timestep should be an error")
	}
}

func TestSeverityTracksTheThresholds(t *testing.T) {
	from := newMeshPair(t, terrain())
	to := newMeshPair(t, terrainWithPlatform())
	lineups := []LineupSeed{seedAt("crate", 0, 0, 64, 500, 0, 200, "Smoke")}

	tiny := 0.001
	res := runDrift(t, from, to, lineups, func(req *DriftRequest) { req.MajorRadius = &tiny; req.UnchangedRadius = &tiny })
	if res.Results[0].Severity != "major" {
		t.Fatalf("with a hair-thin major threshold everything is major, got %q", res.Results[0].Severity)
	}
	huge := 100000.0
	res = runDrift(t, from, to, lineups, func(req *DriftRequest) { req.MajorRadius = &huge })
	if res.Results[0].Severity != "minor" {
		t.Fatalf("with an unreachable major threshold nothing is major, got %q", res.Results[0].Severity)
	}
	res = runDrift(t, from, to, lineups, func(req *DriftRequest) { req.UnchangedRadius = &huge; req.MajorRadius = &huge })
	if res.Results[0].Verdict != VerdictUnchanged {
		t.Fatalf("a threshold wider than the move should call it unchanged, got %q", res.Results[0].Verdict)
	}
}

// The caveats ride along with the payload, because the one conclusion a reader
// must not draw from a screen full of coordinates is that they are real.
func TestResponseCarriesItsCaveats(t *testing.T) {
	pair := newMeshPair(t, terrain())
	res := runDrift(t, pair, pair, []LineupSeed{seedAt("a", 0, 0, 64, 500, 0, 200, "Smoke")}, nil)
	if len(res.Caveats) == 0 {
		t.Fatal("a drift response must carry its caveats")
	}
	if res.Constants != DefaultConstants() {
		t.Fatal("the response must echo the constants the answer was computed with")
	}
}

// One lineup is two flights, so this is the cost of a single row of a drift
// report. It is the number the batch limits and the README are sized from;
// re-run it when the integrator or the BVH changes.
func BenchmarkDriftFlightPairOnARealMesh(b *testing.B) {
	mesh := loadMesh(b, realMeshRevision(b), "de_mirage")
	seed := Seed{Type: Smoke, Position: pt(-2300, 0, -64), Velocity: pt(600, 300, 250)}
	c := DefaultConstants()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := SimulateForComparison(mesh, seed, c); err != nil {
			b.Fatal(err)
		}
		if _, err := SimulateForComparison(mesh, seed, c); err != nil {
			b.Fatal(err)
		}
	}
}

// A whole batch across the worker pool, which is what a caller actually waits
// on. Reported per lineup so it can be read straight off against a batch size.
func BenchmarkDriftBatchOnARealMesh(b *testing.B) {
	b.Setenv("MAP_MESH_CACHE", "4")
	from := loadMesh(b, realMeshRevision(b), "de_mirage")
	to := loadMesh(b, realMeshRevision(b), "de_mirage")
	b.Logf("de_mirage: %d triangles, %.1f MiB per mesh, %.1f MiB resident for the pair",
		from.Triangles(), float64(from.Hull().Bytes())/(1<<20), float64(from.Hull().Bytes()+to.Hull().Bytes())/(1<<20))

	rng := rand.New(rand.NewSource(3))
	const batch = 256
	lineups := make([]LineupSeed, 0, batch)
	types := []string{"Smoke", "HE", "Flash", "Molotov", "Decoy"}
	for i := 0; i < batch; i++ {
		a := rng.Float64() * 2 * math.Pi
		lineups = append(lineups, seedAt("bench", -2300, 0, -64,
			700*math.Cos(a), 700*math.Sin(a), rng.Float64()*400-100, types[i%len(types)]))
	}
	req := DriftRequest{Map: "de_mirage", Lineups: lineups}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Drift(from, to, req, DriftOptions{Workers: 8}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*batch)/1e6, "ms/lineup")
}
