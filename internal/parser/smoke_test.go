package parser

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/5stackgg/demo-parser/internal/geometry"
	"github.com/golang/geo/r3"
)

const testRate = 64.0

func mustVolume(mesh *geometry.Mesh, center r3.Vector) *smokeVolume {
	v, _ := buildSmokeVolume(mesh, center)
	return v
}

// meshFromBlob serves a .tri blob over a throwaway HTTP server and loads it
// through the real fetch path. geometry.Load memoizes per map name, so each
// caller gets a name derived from its test.
func meshFromBlob(t *testing.T, blob []byte) *geometry.Mesh {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(blob)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MAP_MESH_CDN", srv.URL)
	mesh, err := geometry.Load(t.Name())
	if err != nil {
		t.Fatalf("load mesh: %v", err)
	}
	if mesh == nil {
		t.Fatal("expected a mesh to load")
	}
	return mesh
}

// triBlob serializes triangles into the .tri wire format (9 LE float32 each).
func triBlob(tris ...[3]r3.Vector) []byte {
	buf := make([]byte, 0, len(tris)*9*4)
	put := func(f float64) {
		var p [4]byte
		binary.LittleEndian.PutUint32(p[:], math.Float32bits(float32(f)))
		buf = append(buf, p[:]...)
	}
	for _, tr := range tris {
		for _, v := range tr {
			put(v.X)
			put(v.Y)
			put(v.Z)
		}
	}
	return buf
}

// bigWallTriBlob is a quad in the X=0 plane large enough to divide a whole
// smoke volume in two.
func bigWallTriBlob() []byte {
	const s = 400.0
	a := r3.Vector{X: 0, Y: -s, Z: -s}
	b := r3.Vector{X: 0, Y: s, Z: -s}
	c := r3.Vector{X: 0, Y: s, Z: s}
	d := r3.Vector{X: 0, Y: -s, Z: s}
	return triBlob([3]r3.Vector{a, b, c}, [3]r3.Vector{a, c, d})
}

// The property the old sphere-plus-wall-bleed-guard was working around: a cloud
// popped against a wall must have no presence on the far side of it.
func TestSmokeVolumeDoesNotCrossAWall(t *testing.T) {
	mesh := meshFromBlob(t, bigWallTriBlob())
	// Detonate 40 units to the -X side of the wall.
	center := r3.Vector{X: -40}
	v, _ := buildSmokeVolume(mesh, center)
	if v == nil {
		t.Fatal("expected a volume")
	}

	near, far := 0, 0
	for k := 0; k < v.dim[2]; k++ {
		for j := 0; j < v.dim[1]; j++ {
			for i := 0; i < v.dim[0]; i++ {
				if !v.get(i, j, k) {
					continue
				}
				// The wall is the X=0 plane and cell centres straddle it at
				// ±8, so any centre past 0 is a cell lying wholly beyond the
				// wall. Testing against half a cell instead let exactly one
				// layer of leaked cells through unnoticed.
				if c := v.cellCenter(i, j, k); c.X > 0 {
					far++
				} else {
					near++
				}
			}
		}
	}
	if near == 0 {
		t.Fatal("expected the cloud to fill the side it was thrown on")
	}
	if far > 0 {
		t.Fatalf("%d cells leaked through the wall (near side had %d)", far, near)
	}
}

func TestSmokeVolumeWithoutMeshIsNil(t *testing.T) {
	if v, _ := buildSmokeVolume(nil, r3.Vector{}); v != nil {
		t.Fatal("no mesh should mean no volume, so callers fall back to the sphere")
	}
}

// bruteForceDepth integrates density along the segment by fine sampling — the
// reference the cell-walking traversal must agree with.
func bruteForceDepth(v *smokeVolume, from, to r3.Vector) float64 {
	const steps = 40000
	segLen := math.Sqrt(
		(to.X-from.X)*(to.X-from.X) +
			(to.Y-from.Y)*(to.Y-from.Y) +
			(to.Z-from.Z)*(to.Z-from.Z))
	stepLen := segLen / steps / v.size
	depth := 0.0
	for n := 0; n <= steps; n++ {
		t := float64(n) / steps
		p := r3.Vector{
			X: from.X + (to.X-from.X)*t,
			Y: from.Y + (to.Y-from.Y)*t,
			Z: from.Z + (to.Z-from.Z)*t,
		}
		i := int(math.Floor((p.X - v.origin.X) / v.size))
		j := int(math.Floor((p.Y - v.origin.Y) / v.size))
		k := int(math.Floor((p.Z - v.origin.Z) / v.size))
		depth += float64(v.at(i, j, k)) / densityMax * stepLen
	}
	return depth
}

// The traversal is an optimisation over integrating the field directly, so it
// has to agree with the naive scan.
func TestOpticalDepthMatchesBruteForce(t *testing.T) {
	mesh := meshFromBlob(t, bigWallTriBlob())
	v, _ := buildSmokeVolume(mesh, r3.Vector{X: -40})

	// A deterministic spread of segments: through the cloud, past it, along its
	// edges, and axis-aligned degenerate directions.
	seeds := []struct{ from, to r3.Vector }{
		{r3.Vector{X: -400, Y: 0, Z: 0}, r3.Vector{X: -1, Y: 0, Z: 0}},
		{r3.Vector{X: -40, Y: -400, Z: 0}, r3.Vector{X: -40, Y: 400, Z: 0}},
		{r3.Vector{X: -40, Y: 0, Z: -400}, r3.Vector{X: -40, Y: 0, Z: 400}},
		{r3.Vector{X: -300, Y: -300, Z: -300}, r3.Vector{X: -1, Y: 300, Z: 300}},
		{r3.Vector{X: -400, Y: 500, Z: 0}, r3.Vector{X: -1, Y: 500, Z: 0}},
		{r3.Vector{X: -500, Y: -500, Z: 200}, r3.Vector{X: -450, Y: -450, Z: 200}},
		{r3.Vector{X: -60, Y: -20, Z: -10}, r3.Vector{X: -20, Y: 20, Z: 10}},
		{r3.Vector{X: -1000, Y: 30, Z: 20}, r3.Vector{X: 1000, Y: 30, Z: 20}},
	}
	rnd := uint64(12345)
	next := func() float64 {
		rnd = rnd*6364136223846793005 + 1442695040888963407
		return float64(int64(rnd>>11))/float64(1<<52)*800 - 400
	}
	for n := 0; n < 200; n++ {
		seeds = append(seeds, struct{ from, to r3.Vector }{
			r3.Vector{X: next(), Y: next(), Z: next()},
			r3.Vector{X: next(), Y: next(), Z: next()},
		})
	}

	for n, sg := range seeds {
		got := v.opticalDepth(sg.from, sg.to, r3.Vector{X: -40}, smokeRadius*4, nil)
		want := bruteForceDepth(v, sg.from, sg.to)
		// Sampling has its own quantisation, so allow a small absolute slack on
		// top of a relative tolerance.
		if math.Abs(got-want) > 0.02+0.02*want {
			t.Fatalf("segment %d %v→%v: traversal depth %.4f, brute force %.4f",
				n, sg.from, sg.to, got, want)
		}
	}
}

// A smoke does not block a sightline it has not yet grown across.
func TestSmokeBloomRamp(t *testing.T) {
	mesh := meshFromBlob(t, triBlob([3]r3.Vector{
		{X: 9000, Y: 9000, Z: 9000},
		{X: 9100, Y: 9000, Z: 9000},
		{X: 9100, Y: 9100, Z: 9000},
	}))
	center := r3.Vector{}
	v, _ := buildSmokeVolume(mesh, center)

	// A sightline 100 units off the centre line is only covered once the cloud
	// has grown past 100 of its 144 units.
	from := r3.Vector{X: -400, Y: 100}
	to := r3.Vector{X: 400, Y: 100}
	if v.occludedSegment(from, to, center, 40, nil) {
		t.Fatal("a smoke that has just popped should not yet cover the sightline")
	}
	if !v.occludedSegment(from, to, center, smokeRadius, nil) {
		t.Fatal("a fully grown smoke should cover the sightline")
	}
}

// End-to-end through state: the cloud is live between its start and expiry.
func TestSmokeOccludedRespectsLifetime(t *testing.T) {
	mesh := meshFromBlob(t, triBlob([3]r3.Vector{
		{X: 9000, Y: 9000, Z: 9000},
		{X: 9100, Y: 9000, Z: 9000},
		{X: 9100, Y: 9100, Z: 9000},
	}))
	center := r3.Vector{}
	s := &state{res: &Result{}, meshTried: true, mesh: mesh, tickRate: testRate, smokeByEnt: map[int]int{}}
	s.smokes = []smokeCloud{{
		center:    center,
		startTick: 0,
		endTick:   1000,
		vol:       mustVolume(mesh, center),
		exportIdx: -1,
	}}
	from := r3.Vector{X: -400}
	to := r3.Vector{X: 400}

	grown := int(testRate * smokeBloomSecs)
	if !s.smokeOccluded(grown, from, to) {
		t.Fatal("a live, grown smoke should block the sightline")
	}
	if s.smokeOccluded(1001, from, to) {
		t.Fatal("an expired smoke should not block anything")
	}
}

// The exported grid is what the radar and 3D replay draw, so it has to describe
// the same cells the sightline tests used.
func TestExportRoundTripsDensity(t *testing.T) {
	mesh := meshFromBlob(t, bigWallTriBlob())
	center := r3.Vector{X: -40}
	v, _ := buildSmokeVolume(mesh, center)

	ex := v.export(7, 3, 128)
	if ex.GrenadeID != 7 || ex.Round != 3 || ex.StartTick != 128 {
		t.Fatalf("export lost its identity: %+v", ex)
	}
	if ex.VoxelSize != float32(smokeVoxelSize) {
		t.Fatalf("voxel size = %v, want %v", ex.VoxelSize, smokeVoxelSize)
	}
	raw, err := base64.StdEncoding.DecodeString(ex.Density)
	if err != nil {
		t.Fatalf("density is not valid base64: %v", err)
	}
	total := ex.DimX * ex.DimY * ex.DimZ
	if len(raw) != (total+1)/2 {
		t.Fatalf("density is %d bytes, want %d for %d cells", len(raw), (total+1)/2, total)
	}

	// Every non-zero nibble must map back to a cell holding smoke at the same
	// world position, and the counts must agree.
	set := 0
	for k := 0; k < ex.DimZ; k++ {
		for j := 0; j < ex.DimY; j++ {
			for i := 0; i < ex.DimX; i++ {
				n := (k*ex.DimY+j)*ex.DimX + i
				q := raw[n>>1] & 0x0f
				if n&1 == 1 {
					q = raw[n>>1] >> 4
				}
				if q == 0 {
					continue
				}
				set++
				wx := float64(ex.OriginX) + (float64(i)+0.5)*float64(ex.VoxelSize)
				wy := float64(ex.OriginY) + (float64(j)+0.5)*float64(ex.VoxelSize)
				wz := float64(ex.OriginZ) + (float64(k)+0.5)*float64(ex.VoxelSize)
				gi := int(math.Floor((wx - v.origin.X) / v.size))
				gj := int(math.Floor((wy - v.origin.Y) / v.size))
				gk := int(math.Floor((wz - v.origin.Z) / v.size))
				if !v.get(gi, gj, gk) {
					t.Fatalf("exported cell %d,%d,%d maps to an empty source cell", i, j, k)
				}
			}
		}
	}
	if set != v.count() {
		t.Fatalf("exported %d cells, volume holds %d", set, v.count())
	}
}

// Trimming to the occupied bounding box is what keeps the blob small.
func TestExportTrimsToOccupiedBounds(t *testing.T) {
	mesh := meshFromBlob(t, bigWallTriBlob())
	v, _ := buildSmokeVolume(mesh, r3.Vector{X: -40})
	ex := v.export(1, 1, 1)
	if ex.DimX >= v.dim[0] {
		t.Fatalf("a wall-clipped cloud should trim its X extent: exported %d of %d", ex.DimX, v.dim[0])
	}
	if ex.DimX <= 0 || ex.DimY <= 0 || ex.DimZ <= 0 {
		t.Fatalf("degenerate exported dims: %+v", ex)
	}
}
