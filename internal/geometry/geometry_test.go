package geometry

import (
	"encoding/binary"
	"math"
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

func TestBVHDoesNotOverReserve(t *testing.T) {
	// Enough triangles that the tree is deep enough for the reservation to
	// matter, laid out so the median split has real work to do.
	var verts [][3]r3.Vector
	for i := 0; i < 4000; i++ {
		x := float64(i)
		verts = append(verts, [3]r3.Vector{
			{X: x, Y: 0, Z: 0},
			{X: x, Y: 1, Z: 0},
			{X: x, Y: 1, Z: 1},
		})
	}
	m := meshFromTris(verts...)
	if slack := cap(m.nodes) - len(m.nodes); slack > len(m.nodes)/8 {
		t.Fatalf("BVH holds %d nodes in a slice of cap %d — %d wasted", len(m.nodes), cap(m.nodes), slack)
	}
	// Sanity: the tree really did get built, and is the size the reservation assumes.
	if len(m.nodes) == 0 || len(m.nodes) >= len(m.tris) {
		t.Fatalf("expected under %d nodes for %d triangles, got %d", len(m.tris), len(m.tris), len(m.nodes))
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

func TestNormalizeMapName(t *testing.T) {
	cases := map[string]string{
		"de_mirage":               "de_mirage",
		"DE_Inferno":              "de_inferno",
		"de_inferno_night":        "de_inferno",
		"workshop/3070821578/de_torn": "de_torn",
		"":                        "",
	}
	for in, want := range cases {
		if got := normalizeMapName(in); got != want {
			t.Errorf("normalizeMapName(%q) = %q, want %q", in, got, want)
		}
	}
}
