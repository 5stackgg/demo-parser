package geometry

import (
	"encoding/binary"
	"math"
	"math/rand"
	"testing"
	"unsafe"

	"github.com/golang/geo/r3"
)

// meshFromTris builds a Mesh directly from triangle vertices, bypassing the
// .tri loader so the raycaster can be tested without network.
func meshFromTris(verts ...[3]r3.Vector) *Mesh {
	m := &Mesh{}
	for _, v := range verts {
		m.tris = append(m.tris, newTriangle(v[0], v[1], v[2]))
	}
	m.build()
	return m
}

// A wall is a quad (two triangles) in the X=0 plane spanning y,z ∈ [-50,50].
func wallMesh() *Mesh {
	a := r3.Vector{X: 0, Y: -50, Z: -50}
	b := r3.Vector{X: 0, Y: 50, Z: -50}
	c := r3.Vector{X: 0, Y: 50, Z: 50}
	d := r3.Vector{X: 0, Y: -50, Z: 50}
	return meshFromTris([3]r3.Vector{a, b, c}, [3]r3.Vector{a, c, d})
}

func TestOccludedCrossingWall(t *testing.T) {
	m := wallMesh()
	from := r3.Vector{X: -100, Y: 0, Z: 0}
	to := r3.Vector{X: 100, Y: 0, Z: 0}
	if !m.Occluded(from, to) {
		t.Fatal("segment crossing the wall should be occluded")
	}
}

func TestNotOccludedBesideWall(t *testing.T) {
	m := wallMesh()
	// Both endpoints on the same side of the wall — never crosses X=0.
	from := r3.Vector{X: -100, Y: 0, Z: 0}
	to := r3.Vector{X: -10, Y: 0, Z: 0}
	if m.Occluded(from, to) {
		t.Fatal("segment that never crosses the wall should be clear")
	}
}

func TestNotOccludedPastWallEdge(t *testing.T) {
	m := wallMesh()
	// Crosses X=0 but well outside the wall's y extent (y=200).
	from := r3.Vector{X: -100, Y: 200, Z: 0}
	to := r3.Vector{X: 100, Y: 200, Z: 0}
	if m.Occluded(from, to) {
		t.Fatal("segment crossing outside the wall bounds should be clear")
	}
}

func TestEndpointEpsilonDoesNotSelfOcclude(t *testing.T) {
	m := wallMesh()
	// An endpoint sitting essentially on the wall surface must not count as
	// occluding its own short sightline away from the wall.
	from := r3.Vector{X: 0.5, Y: 0, Z: 0}
	to := r3.Vector{X: 60, Y: 0, Z: 0}
	if m.Occluded(from, to) {
		t.Fatal("a sightline starting at the wall and going away should be clear")
	}
}

func TestRayHitDist(t *testing.T) {
	m := wallMesh()
	dist, ok := m.RayHitDist(r3.Vector{X: -30, Y: 0, Z: 0}, r3.Vector{X: 1, Y: 0, Z: 0})
	if !ok {
		t.Fatal("ray pointing at the wall should hit")
	}
	if math.Abs(dist-30) > 1e-3 {
		t.Fatalf("expected hit distance ~30, got %v", dist)
	}
	if _, ok := m.RayHitDist(r3.Vector{X: -30, Y: 0, Z: 0}, r3.Vector{X: -1, Y: 0, Z: 0}); ok {
		t.Fatal("ray pointing away from the wall should miss")
	}
}

func TestNilMeshIsVisible(t *testing.T) {
	var m *Mesh
	if m.Occluded(r3.Vector{}, r3.Vector{X: 100}) {
		t.Fatal("nil mesh must report no occlusion")
	}
}

// triBlob serializes whole triangles into the .tri wire format (9 LE float32
// per triangle) so buildMesh can be exercised without the network.
func triBlob(tris ...[3]r3.Vector) []byte {
	buf := make([]byte, 0, len(tris)*9*4)
	put := func(f float64) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], math.Float32bits(float32(f)))
		buf = append(buf, b[:]...)
	}
	for _, t := range tris {
		for _, v := range t {
			put(v.X)
			put(v.Y)
			put(v.Z)
		}
	}
	return buf
}

func TestBuildMeshTrailingPartialTriangle(t *testing.T) {
	full := triBlob([3]r3.Vector{
		{X: 0, Y: -50, Z: -50},
		{X: 0, Y: 50, Z: -50},
		{X: 0, Y: 50, Z: 50},
	})
	// Append a partial triangle (fewer than 36 bytes); it must be dropped, not
	// read out of bounds.
	data := append(full, full[:20]...)
	m := buildMesh(data)
	if m == nil {
		t.Fatal("one full triangle plus a partial should still build a mesh")
	}
	if m.Triangles() != 1 {
		t.Fatalf("expected 1 triangle (partial dropped), got %d", m.Triangles())
	}
}

func TestBuildMeshPartialOnlyIsNoMesh(t *testing.T) {
	full := triBlob([3]r3.Vector{{}, {X: 1}, {Y: 1}})
	if m := buildMesh(full[:20]); m != nil {
		t.Fatalf("a sub-triangle blob must yield no mesh, got %d triangles", m.Triangles())
	}
}

func TestRayTriangleDegenerate(t *testing.T) {
	// Zero-area (collinear) triangle: a valid ray straight at it must miss
	// rather than report a hit.
	tr := newTriangle(
		r3.Vector{X: 0, Y: 0, Z: 0},
		r3.Vector{X: 0, Y: 0, Z: 0},
		r3.Vector{X: 0, Y: 0, Z: 0},
	)
	if _, ok := rayTriangle(r3.Vector{X: -10, Y: 0, Z: 0}, r3.Vector{X: 1, Y: 0, Z: 0}, &tr); ok {
		t.Fatal("degenerate (zero-area) triangle should not register a hit")
	}
}

// A cached mesh is the single largest thing this process holds — six maps at
// the old 144-byte triangle and 2N node reservation was ~600 MiB resident.
// These two guard the layout so that cannot creep back.
func TestTriangleStaysSmall(t *testing.T) {
	const want = 36 // 3 vectors × 3 float32
	if got := unsafe.Sizeof(triangle{}); got != want {
		t.Fatalf("triangle is %d bytes, want %d — did a build-only field or a float64 come back?", got, want)
	}
}

// spreadOn builds n triangles strung out along one axis, so the median split
// picks that axis and exercises that axis's comparator.
func spreadOn(axis, n int) *Mesh {
	verts := make([][3]r3.Vector, 0, n)
	for i := 0; i < n; i++ {
		d := float64(i)
		at := func(u, v float64) r3.Vector {
			switch axis {
			case 0:
				return r3.Vector{X: d, Y: u, Z: v}
			case 1:
				return r3.Vector{X: u, Y: d, Z: v}
			default:
				return r3.Vector{X: u, Y: v, Z: d}
			}
		}
		verts = append(verts, [3]r3.Vector{at(0, 0), at(1, 0), at(1, 1)})
	}
	return meshFromTris(verts...)
}

func TestBVHDoesNotOverReserve(t *testing.T) {
	// Every axis: the split has one comparator per axis, and a copy-paste slip
	// in one of them yields a valid but badly split tree — no test would fail,
	// the ray queries would just quietly get slower. Triangle counts are chosen
	// to straddle the node-count ratio, which peaks at 4/5 near n = 5·2^k.
	for _, n := range []int{4000, 5120, 10240, 20480} {
		for axis := 0; axis < 3; axis++ {
			m := spreadOn(axis, n)
			if slack := cap(m.nodes) - len(m.nodes); slack > len(m.nodes)/8 {
				t.Errorf("n=%d axis=%d: %d nodes in a slice of cap %d — %d wasted",
					n, axis, len(m.nodes), cap(m.nodes), slack)
			}
			// The reservation must never have had to grow mid-build.
			if len(m.nodes) > len(m.tris)*4/5+1 {
				t.Errorf("n=%d axis=%d: %d nodes exceeds the 4/5 reservation for %d triangles",
					n, axis, len(m.nodes), len(m.tris))
			}
			if len(m.nodes) == 0 {
				t.Fatalf("n=%d axis=%d: no BVH was built", n, axis)
			}
		}
	}
}

// The median split must actually partition on each axis, not just compile. A
// comparator reading the wrong field still builds a correct tree, so this
// checks the tree is *tight*: a well-split run of triangles gives leaves whose
// boxes are small along the spread axis.
func TestSplitPartitionsOnEachAxis(t *testing.T) {
	const n = 4096
	for axis := 0; axis < 3; axis++ {
		m := spreadOn(axis, n)
		widest := 0.0
		for i := range m.nodes {
			if m.nodes[i].left >= 0 {
				continue // internal
			}
			e := m.nodes[i].max.Sub(m.nodes[i].min)
			w := []float64{e.X, e.Y, e.Z}[axis]
			if w > widest {
				widest = w
			}
		}
		// Leaves hold ~4 of n triangles spaced one unit apart, so a leaf that
		// split on the right axis spans a handful of units, not the full range.
		if widest > 16 {
			t.Errorf("axis=%d: widest leaf spans %.0f units of a %d-unit range — "+
				"the split is not partitioning on this axis", axis, widest, n)
		}
	}
}

// Triangles are stored at the .tri file's own float32 precision. Real maps run
// to ±16384 source units, where float32 still resolves ~0.001 — three orders of
// magnitude finer than the 2.0-unit endEps — so occlusion must be unaffected at
// map scale, not just near the origin where the other tests sit.
func TestOcclusionAtMapScaleCoordinates(t *testing.T) {
	const (
		wx = -8000.0 // wall plane, far from the origin
		wy = 7500.0
		wz = 6000.0
	)
	a := r3.Vector{X: wx, Y: wy - 50, Z: wz - 50}
	b := r3.Vector{X: wx, Y: wy + 50, Z: wz - 50}
	c := r3.Vector{X: wx, Y: wy + 50, Z: wz + 50}
	d := r3.Vector{X: wx, Y: wy - 50, Z: wz + 50}
	m := meshFromTris([3]r3.Vector{a, b, c}, [3]r3.Vector{a, c, d})

	through := m.Occluded(
		r3.Vector{X: wx - 300, Y: wy, Z: wz},
		r3.Vector{X: wx + 300, Y: wy, Z: wz},
	)
	if !through {
		t.Error("segment crossing the wall at map-scale coordinates should be occluded")
	}

	beside := m.Occluded(
		r3.Vector{X: wx - 300, Y: wy + 200, Z: wz},
		r3.Vector{X: wx + 300, Y: wy + 200, Z: wz},
	)
	if beside {
		t.Error("segment passing outside the wall at map-scale coordinates should be clear")
	}

	dist, ok := m.RayHitDist(r3.Vector{X: wx - 300, Y: wy, Z: wz}, r3.Vector{X: 1})
	if !ok {
		t.Fatal("ray at map-scale coordinates should hit the wall")
	}
	if math.Abs(dist-300) > 0.05 {
		t.Errorf("expected hit distance ~300, got %v (float32 storage lost too much)", dist)
	}
}

// Adjacent triangles must agree on the geometry they share, bit for bit. They
// do because the corners are what is stored: narrow a vertex and every triangle
// holding it narrows it the same way. Store the Möller–Trumbore edges instead
// and each triangle derives the shared edge from its own origin, rounding it
// differently — the two walls stop meeting exactly and rays slip through the
// gap. Measured at map scale that leaked ~28% of rays fired along a shared
// edge, against ~8% for the ideal, so this is the invariant that keeps the
// float32 storage honest.
func TestSharedGeometryIsIdenticalFromBothSides(t *testing.T) {
	// Two triangles of a quad, deliberately built from different first
	// vertices and in different winding orders, sharing the edge (b, c).
	b := r3.Vector{X: 0, Y: -2999.7331, Z: 1999.113}
	c := r3.Vector{X: 0, Y: 3001.229, Z: -1500.577}
	t1 := newTriangle(r3.Vector{X: 0, Y: -4000.31, Z: -4000.77}, b, c)
	t2 := newTriangle(r3.Vector{X: 0, Y: 4000.13, Z: 4000.91}, c, b)

	// t1 holds (b, c) as (v1, v2); t2 holds them as (v2, v1).
	if t1.v1 != t2.v2 {
		t.Errorf("shared vertex b differs between adjacent triangles: %+v vs %+v", t1.v1, t2.v2)
	}
	if t1.v2 != t2.v1 {
		t.Errorf("shared vertex c differs between adjacent triangles: %+v vs %+v", t1.v2, t2.v1)
	}

	// And behaviourally: firing along the seam of a two-triangle wall must not
	// find a way through. One configuration proves nothing — whether a given
	// seam happens to round open is luck — so sweep a fixed set of them.
	rng := rand.New(rand.NewSource(42))
	q := func(v float64) float64 { return float64(float32(v)) } // as a .tri stores it
	leaks, rays := 0, 0
	for cfg := 0; cfg < 40; cfg++ {
		p := func() r3.Vector {
			return r3.Vector{X: 0, Y: q((rng.Float64() - 0.5) * 16000), Z: q((rng.Float64() - 0.5) * 16000)}
		}
		bb, cc, a1, a2 := p(), p(), p(), p()
		m := meshFromTris([3]r3.Vector{a1, bb, cc}, [3]r3.Vector{a2, cc, bb})
		const samples = 501
		for i := 0; i < samples; i++ {
			f := float64(i) / float64(samples-1)
			y := bb.Y + (cc.Y-bb.Y)*f
			z := bb.Z + (cc.Z-bb.Z)*f
			rays++
			if !m.Occluded(r3.Vector{X: -50, Y: y, Z: z}, r3.Vector{X: 50, Y: y, Z: z}) {
				leaks++
			}
		}
	}
	// Möller–Trumbore rejects a ray landing exactly on a shared boundary from
	// both sides, so even ideal geometry leaks along the seam — about 8% of
	// rays aimed straight down it. Storing narrowed edges instead of corners
	// measured ~28%. The threshold sits between the two.
	if pct := 100 * float64(leaks) / float64(rays); pct > 15 {
		t.Errorf("%.1f%% of rays along shared edges (%d/%d) passed through a solid wall — "+
			"adjacent triangles are not meeting exactly", pct, leaks, rays)
	}
}

func TestNormalizeMapName(t *testing.T) {
	cases := map[string]string{
		"de_mirage":                   "de_mirage",
		"DE_Inferno":                  "de_inferno",
		"de_inferno_night":            "de_inferno",
		"workshop/3070821578/de_torn": "de_torn",
		"":                            "",
	}
	for in, want := range cases {
		if got := normalizeMapName(in); got != want {
			t.Errorf("normalizeMapName(%q) = %q, want %q", in, got, want)
		}
	}
}

// A .tri is a soup of unwound triangles: the same wall can be stored facing
// either way, so the normal has to be taken relative to the ray that found it.
// A bounce computed off a flipped normal drives the grenade into the wall.
func TestRayHitSurfaceNormalAlwaysFacesTheRay(t *testing.T) {
	m := wallMesh()
	for _, tc := range []struct {
		name   string
		origin r3.Vector
		dir    r3.Vector
		wantX  float64
	}{
		{"approaching from -X", r3.Vector{X: -30}, r3.Vector{X: 1}, -1},
		{"approaching from +X", r3.Vector{X: 30}, r3.Vector{X: -1}, 1},
	} {
		hit, ok := m.RayHitSurface(tc.origin, tc.dir)
		if !ok {
			t.Fatalf("%s: expected a hit", tc.name)
		}
		if math.Abs(hit.Distance-30) > 1e-6 {
			t.Errorf("%s: distance %v, want 30", tc.name, hit.Distance)
		}
		if math.Abs(hit.Normal.X-tc.wantX) > 1e-9 || hit.Normal.Y != 0 || hit.Normal.Z != 0 {
			t.Errorf("%s: normal %v, want X=%v", tc.name, hit.Normal, tc.wantX)
		}
		if d := hit.Normal.Dot(tc.dir); d >= 0 {
			t.Errorf("%s: normal points along the ray (dot %v)", tc.name, d)
		}
	}
}

func TestRayHitSurfaceMisses(t *testing.T) {
	m := wallMesh()
	if _, ok := m.RayHitSurface(r3.Vector{X: -30}, r3.Vector{X: -1}); ok {
		t.Error("a ray pointing away from the wall should miss")
	}
	if _, ok := m.RayHitSurface(r3.Vector{X: -30}, r3.Vector{}); ok {
		t.Error("a zero-length direction should miss rather than divide by zero")
	}
	var nilMesh *Mesh
	if _, ok := nilMesh.RayHitSurface(r3.Vector{}, r3.Vector{X: 1}); ok {
		t.Error("a nil mesh has no surfaces")
	}
}

func TestBoundsCoverTheGeometry(t *testing.T) {
	m := wallMesh()
	lo, hi, ok := m.Bounds()
	if !ok {
		t.Fatal("a built mesh should report bounds")
	}
	if lo.Y > -50 || lo.Z > -50 || hi.Y < 50 || hi.Z < 50 {
		t.Fatalf("bounds %v..%v do not cover the wall", lo, hi)
	}
	var nilMesh *Mesh
	if _, _, ok := nilMesh.Bounds(); ok {
		t.Error("a nil mesh has no bounds")
	}
}
