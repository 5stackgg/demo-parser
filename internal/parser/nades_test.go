package parser

import (
	"math"
	"testing"

	"github.com/5stackgg/demo-parser/internal/geometry"
	"github.com/golang/geo/r3"
)

// openSpaceMesh is a mesh whose only triangle is nowhere near the origin, so
// geometry is available (nothing falls back) but nothing obstructs.
func openSpaceMesh(t *testing.T) *geometry.Mesh {
	t.Helper()
	return meshFromBlob(t, triBlob([3]r3.Vector{
		{X: 9000, Y: 9000, Z: 9000},
		{X: 9100, Y: 9000, Z: 9000},
		{X: 9100, Y: 9100, Z: 9000},
	}))
}

func TestSmokeVolumeEndpointNeedsAMesh(t *testing.T) {
	if _, err := SmokeVolume(nil, SmokeVolumeRequest{Map: "de_nowhere"}); err != ErrNoMesh {
		t.Fatalf("no mesh should be ErrNoMesh, got %v", err)
	}
}

// A point inside geometry cannot bloom. Reporting an empty grid would tell the
// UI the smoke is fine and block nothing, so it is an error instead.
func TestSmokeVolumeRejectsASealedPoint(t *testing.T) {
	// A closed box of six quads, with the query point outside it but buried
	// deep in the solid half-space behind a wall the flood cannot escape.
	mesh := meshFromBlob(t, sealedBoxTriBlob(24))
	_, err := SmokeVolume(mesh, SmokeVolumeRequest{Map: "de_test"})
	if err != ErrSmokeSealed {
		t.Fatalf("a sealed point should be ErrSmokeSealed, got %v", err)
	}
}

func TestSmokeVolumeRejectsNonFiniteCoordinates(t *testing.T) {
	mesh := openSpaceMesh(t)
	req := SmokeVolumeRequest{Map: "de_test", X: math.NaN()}
	if _, err := SmokeVolume(mesh, req); err == nil {
		t.Fatal("NaN coordinates should be rejected")
	}
}

// The endpoint's job is to hand back exactly what the playback blob carries, so
// the browser can share one decoder between a mined lineup and a replay.
func TestSmokeVolumeMatchesTheParserVolume(t *testing.T) {
	mesh := meshFromBlob(t, bigWallTriBlob())
	at := r3.Vector{X: -40}

	res, err := SmokeVolume(mesh, SmokeVolumeRequest{Map: "de_test", X: at.X, Y: at.Y, Z: at.Z})
	if err != nil {
		t.Fatalf("smoke volume: %v", err)
	}
	want := mustVolume(mesh, at).export(0, 0, 0)
	if res.EventSmokeVolume != want {
		t.Fatalf("endpoint volume %+v differs from the parser's %+v", res.EventSmokeVolume, want)
	}
	if res.Cells < minCloudCells {
		t.Fatalf("cells = %d, expected a real cloud", res.Cells)
	}
	if res.Radius != smokeRadius {
		t.Fatalf("radius = %v, want %v", res.Radius, smokeRadius)
	}
}

func TestSightlinesBlockedClearAndGrazing(t *testing.T) {
	mesh := openSpaceMesh(t)
	at := Point{}
	req := SightlineRequest{
		Map: "de_test",
		At:  &at,
		Pairs: []SightlinePair{
			// Straight through the core.
			{From: Point{X: -500}, To: Point{X: 500}},
			// Clipping the rim, where the cloud is thin enough to see through.
			{From: Point{X: -500, Y: smokeRadius * 0.9}, To: Point{X: 500, Y: smokeRadius * 0.9}},
			// Nowhere near it.
			{From: Point{X: -500, Y: 600}, To: Point{X: 500, Y: 600}},
		},
	}
	res, err := Sightlines(mesh, req)
	if err != nil {
		t.Fatalf("sightlines: %v", err)
	}
	if res.Threshold != DefaultBlockThreshold {
		t.Fatalf("threshold = %v, want the default %v", res.Threshold, DefaultBlockThreshold)
	}
	if len(res.Smokes) != 1 || res.Smokes[0].Model != "voxel" || res.Smokes[0].Sealed {
		t.Fatalf("expected one flooded cloud, got %+v", res.Smokes)
	}

	core, grazing, clear := res.Results[0], res.Results[1], res.Results[2]
	if !core.Blocked || core.BlockedBy != "smoke" {
		t.Fatalf("a line through the core should be blocked by smoke: %+v", core)
	}
	if core.Depth < DefaultBlockThreshold {
		t.Fatalf("core depth %.2f is under the threshold", core.Depth)
	}
	if grazing.Blocked {
		t.Fatalf("a line clipping the rim should not be blocked: %+v", grazing)
	}
	if grazing.Depth <= 0 || grazing.Depth >= DefaultBlockThreshold {
		t.Fatalf("a grazing line should carry some smoke but not enough: depth %.2f", grazing.Depth)
	}
	if clear.Blocked || clear.Depth != 0 || clear.Transmittance != 1 {
		t.Fatalf("a line away from the cloud should be untouched: %+v", clear)
	}
	// Transmittance is the reading the threshold is a judgement about, so it
	// has to agree with the depth it came from.
	if math.Abs(grazing.Transmittance-math.Exp(-grazing.Depth)) > 1e-9 {
		t.Fatalf("transmittance %v does not match depth %v", grazing.Transmittance, grazing.Depth)
	}
}

// The threshold is a judgement, so a caller can move it — and moving it has to
// actually change the verdict rather than just the reported number.
func TestSightlineThresholdIsTunable(t *testing.T) {
	mesh := openSpaceMesh(t)
	at := Point{}
	pair := SightlinePair{
		From: Point{X: -500, Y: smokeRadius * 0.9},
		To:   Point{X: 500, Y: smokeRadius * 0.9},
	}
	loose := 0.1
	res, err := Sightlines(mesh, SightlineRequest{
		Map: "de_test", At: &at, Pairs: []SightlinePair{pair}, Threshold: &loose,
	})
	if err != nil {
		t.Fatalf("sightlines: %v", err)
	}
	if !res.Results[0].Blocked {
		t.Fatalf("the grazing line should block once the threshold drops to %v: %+v", loose, res.Results[0])
	}
	if res.Threshold != loose {
		t.Fatalf("threshold = %v, want %v", res.Threshold, loose)
	}
}

// A wall in the way is the map's doing, and a lineup does not get to claim it.
func TestSightlineAttributesAWallToTheWorld(t *testing.T) {
	mesh := meshFromBlob(t, bigWallTriBlob())
	at := Point{X: -40}
	res, err := Sightlines(mesh, SightlineRequest{
		Map: "de_test", At: &at,
		Pairs: []SightlinePair{{From: Point{X: -300}, To: Point{X: 300}}},
	})
	if err != nil {
		t.Fatalf("sightlines: %v", err)
	}
	got := res.Results[0]
	if !got.Blocked || got.BlockedBy != "world" || !got.WorldBlocked {
		t.Fatalf("the wall should own this one: %+v", got)
	}
}

// Handing back a volume and asking about it again is the round trip the web UI
// makes: preview once, then ask what it blocks without paying for the flood
// twice. The two answers have to agree.
func TestSightlineAcceptsASuppliedVolume(t *testing.T) {
	mesh := openSpaceMesh(t)
	at := Point{}
	vol, err := SmokeVolume(mesh, SmokeVolumeRequest{Map: "de_test"})
	if err != nil {
		t.Fatalf("smoke volume: %v", err)
	}

	pairs := []SightlinePair{
		{From: Point{X: -500}, To: Point{X: 500}},
		{From: Point{X: -500, Y: smokeRadius * 0.9}, To: Point{X: 500, Y: smokeRadius * 0.9}},
		{From: Point{X: -500, Y: 600}, To: Point{X: 500, Y: 600}},
	}
	fromPoint, err := Sightlines(mesh, SightlineRequest{Map: "de_test", At: &at, Pairs: pairs})
	if err != nil {
		t.Fatalf("sightlines from point: %v", err)
	}
	supplied := vol.EventSmokeVolume
	fromVolume, err := Sightlines(mesh, SightlineRequest{
		Map: "de_test", Smoke: &CloudSpec{Volume: &supplied}, Pairs: pairs,
	})
	if err != nil {
		t.Fatalf("sightlines from volume: %v", err)
	}
	for i := range pairs {
		a, b := fromPoint.Results[i], fromVolume.Results[i]
		if a.Blocked != b.Blocked {
			t.Fatalf("pair %d: flooded says blocked=%v, round-tripped says %v", i, a.Blocked, b.Blocked)
		}
		// The wire form quantises density to 16 levels, so the depths differ
		// slightly. One level over a long chord is a few tenths.
		if math.Abs(a.Depth-b.Depth) > 0.05+0.05*a.Depth {
			t.Fatalf("pair %d: depth %.3f flooded vs %.3f round-tripped", i, a.Depth, b.Depth)
		}
	}
}

func TestSuppliedVolumeIsValidated(t *testing.T) {
	mesh := openSpaceMesh(t)
	pairs := []SightlinePair{{From: Point{X: -500}, To: Point{X: 500}}}
	cases := []struct {
		name string
		vol  EventSmokeVolume
	}{
		{"no dims", EventSmokeVolume{VoxelSize: 16}},
		{"no voxel size", EventSmokeVolume{DimX: 2, DimY: 2, DimZ: 2}},
		{"density not base64", EventSmokeVolume{DimX: 2, DimY: 2, DimZ: 2, VoxelSize: 16, Density: "!!!"}},
		{"density too short", EventSmokeVolume{DimX: 2, DimY: 2, DimZ: 2, VoxelSize: 16, Density: "AA=="}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vol := tc.vol
			_, err := Sightlines(mesh, SightlineRequest{
				Map: "de_test", Smoke: &CloudSpec{Volume: &vol}, Pairs: pairs,
			})
			if err == nil {
				t.Fatal("expected the malformed volume to be rejected")
			}
		})
	}
}

// Several clouds stack: a line through two thin ones is blocked even when
// neither would do it alone.
func TestSightlineSumsDepthAcrossClouds(t *testing.T) {
	mesh := openSpaceMesh(t)
	// Two clouds either side of the line's midpoint, each grazed near its rim.
	a := Point{Y: -smokeRadius * 0.9}
	b := Point{X: 300, Y: -smokeRadius * 0.9}
	pairs := []SightlinePair{{From: Point{X: -300}, To: Point{X: 600}}}

	one, err := Sightlines(mesh, SightlineRequest{Map: "de_test", At: &a, Pairs: pairs})
	if err != nil {
		t.Fatalf("sightlines: %v", err)
	}
	both, err := Sightlines(mesh, SightlineRequest{
		Map:   "de_test",
		Smoke: &CloudSpec{At: &a}, Smokes: []CloudSpec{{At: &b}},
		Pairs: pairs,
	})
	if err != nil {
		t.Fatalf("sightlines: %v", err)
	}
	if len(both.Results[0].PerSmoke) != 2 {
		t.Fatalf("expected a per-cloud split, got %+v", both.Results[0].PerSmoke)
	}
	sum := both.Results[0].PerSmoke[0] + both.Results[0].PerSmoke[1]
	if math.Abs(sum-both.Results[0].Depth) > 1e-9 {
		t.Fatalf("per-smoke depths %v do not sum to %v", both.Results[0].PerSmoke, both.Results[0].Depth)
	}
	if both.Results[0].Depth <= one.Results[0].Depth {
		t.Fatalf("two clouds should put more smoke on the line than one: %.2f vs %.2f",
			both.Results[0].Depth, one.Results[0].Depth)
	}
}

// A sealed point still has to answer something, and the answer has to say it is
// a guess.
func TestSightlineFallsBackToASphereWhenSealed(t *testing.T) {
	mesh := meshFromBlob(t, sealedBoxTriBlob(24))
	at := Point{}
	res, err := Sightlines(mesh, SightlineRequest{
		Map: "de_test", At: &at,
		Pairs: []SightlinePair{{From: Point{X: -500, Z: 200}, To: Point{X: 500, Z: 200}}},
	})
	if err != nil {
		t.Fatalf("sightlines: %v", err)
	}
	if len(res.Smokes) != 1 || !res.Smokes[0].Sealed || res.Smokes[0].Model != "sphere" {
		t.Fatalf("a sealed cloud should be reported as a sphere fallback: %+v", res.Smokes)
	}
}

func TestSightlineRequestValidation(t *testing.T) {
	mesh := openSpaceMesh(t)
	at := Point{}
	if _, err := Sightlines(nil, SightlineRequest{Map: "x", At: &at}); err != ErrNoMesh {
		t.Fatal("a missing mesh should be ErrNoMesh")
	}
	if _, err := Sightlines(mesh, SightlineRequest{Map: "x", At: &at}); err == nil {
		t.Fatal("an empty pair list should be rejected")
	}
	tooMany := make([]SightlinePair, maxSightlinePairs+1)
	if _, err := Sightlines(mesh, SightlineRequest{Map: "x", At: &at, Pairs: tooMany}); err == nil {
		t.Fatal("a pair list over the cap should be rejected")
	}
	if _, err := Sightlines(mesh, SightlineRequest{
		Map: "x", Smoke: &CloudSpec{}, Pairs: []SightlinePair{{}},
	}); err == nil {
		t.Fatal("a cloud with neither a point nor a volume should be rejected")
	}
	if _, err := Sightlines(mesh, SightlineRequest{
		Map: "x", At: &at,
		Pairs: []SightlinePair{{From: Point{X: math.Inf(1)}}},
	}); err == nil {
		t.Fatal("non-finite pair coordinates should be rejected")
	}
}

// Asking about no smoke at all is legitimate: it is the pure map question, and
// it must not be answered as "everything is blocked".
func TestSightlineWithNoCloudsIsAWorldQuery(t *testing.T) {
	mesh := meshFromBlob(t, bigWallTriBlob())
	res, err := Sightlines(mesh, SightlineRequest{
		Map: "de_test",
		Pairs: []SightlinePair{
			{From: Point{X: -300}, To: Point{X: 300}},
			{From: Point{X: -300}, To: Point{X: -100}},
		},
	})
	if err != nil {
		t.Fatalf("sightlines: %v", err)
	}
	if !res.Results[0].Blocked || res.Results[0].BlockedBy != "world" {
		t.Fatalf("the wall should block the crossing line: %+v", res.Results[0])
	}
	if res.Results[1].Blocked {
		t.Fatalf("a line on one side of the wall is open: %+v", res.Results[1])
	}
}

// sealedBoxTriBlob returns a closed axis-aligned box centred on the origin,
// small enough that a smoke at the origin has nowhere to go.
func sealedBoxTriBlob(half float64) []byte {
	quad := func(a, b, c, d r3.Vector) []byte {
		return triBlob([3]r3.Vector{a, b, c}, [3]r3.Vector{a, c, d})
	}
	h := half
	// Corners, low then high.
	l := [8]r3.Vector{
		{X: -h, Y: -h, Z: -h}, {X: h, Y: -h, Z: -h}, {X: h, Y: h, Z: -h}, {X: -h, Y: h, Z: -h},
		{X: -h, Y: -h, Z: h}, {X: h, Y: -h, Z: h}, {X: h, Y: h, Z: h}, {X: -h, Y: h, Z: h},
	}
	var blob []byte
	for _, face := range [][]byte{
		quad(l[0], l[1], l[2], l[3]),
		quad(l[4], l[5], l[6], l[7]),
		quad(l[0], l[1], l[5], l[4]),
		quad(l[3], l[2], l[6], l[7]),
		quad(l[0], l[3], l[7], l[4]),
		quad(l[1], l[2], l[6], l[5]),
	} {
		blob = append(blob, face...)
	}
	return blob
}
