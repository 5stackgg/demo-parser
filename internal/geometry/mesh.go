// Package geometry loads a map's collision mesh (.tri) and answers
// line-of-sight / raycast queries against it, so the demo parser can tell
// whether two players actually had a clear sightline (vs. a "spot" through
// smoke, a thin gap, or the edge of vision). Coordinates are raw CS2 source
// units (Z-up) — the same space as p.PositionEyes(), so no transform is
// needed.
package geometry

import (
	"math"
	"unsafe"

	"github.com/golang/geo/r3"
)

// endEps pulls a segment's endpoints inward (source units) so the surface a
// player hugs / stands on doesn't register as an occluder of its own sightline.
const endEps = 2.0

// vec3 mirrors the .tri wire precision (little-endian float32), so storing a
// triangle costs no accuracy over the source data. A competitive map runs to
// half a million triangles, so the 12 bytes saved per vector over r3.Vector is
// tens of MB on a single mesh. Ray math widens back to float64.
type vec3 struct{ X, Y, Z float32 }

func newVec3(v r3.Vector) vec3 {
	return vec3{X: float32(v.X), Y: float32(v.Y), Z: float32(v.Z)}
}

// triangle carries only what rayTriangle reads. Bounds and centroids are
// needed just once, while the BVH is being built, and are cheap to recover
// from the corners (see triBounds and the split in bvh.go) — storing them would
// quadruple the resident size of every mesh for the life of the process.
//
// The corners are stored rather than the Möller–Trumbore edges, even though
// that means subtracting on every ray test. Storing narrowed edges is what
// breaks a mesh open: two triangles sharing an edge derive it from their own
// origins, so each rounds it slightly differently and the shared boundary stops
// being one line. Rays then slip between two touching walls. Corners are shared
// bit-for-bit, so the edges recovered below are identical from either side —
// and since a corner is float32 to begin with, the float64 difference is exact.
type triangle struct {
	v0, v1, v2 vec3
}

func newTriangle(a, b, c r3.Vector) triangle {
	return triangle{v0: newVec3(a), v1: newVec3(b), v2: newVec3(c)}
}

// corners returns the triangle's three vertices at full width. The widening is
// exact, so callers see precisely the coordinates the .tri file carried.
func (t *triangle) corners() (x0, y0, z0, x1, y1, z1, x2, y2, z2 float64) {
	return float64(t.v0.X), float64(t.v0.Y), float64(t.v0.Z),
		float64(t.v1.X), float64(t.v1.Y), float64(t.v1.Z),
		float64(t.v2.X), float64(t.v2.Y), float64(t.v2.Z)
}

// rayTriangle returns the parametric distance t (point = orig + t*dir) of the
// intersection, double-sided (walls can face either way). ok is false when the
// ray misses or is parallel.
func rayTriangle(orig, dir r3.Vector, tr *triangle) (float64, bool) {
	const eps = 1e-9
	// Recover the edges at full width. Both operands are float32, so each
	// difference is exact — every triangle sharing this edge computes the same
	// one, and the rest of the test keeps its usual conditioning.
	x0, y0, z0, x1, y1, z1, x2, y2, z2 := tr.corners()
	e1x, e1y, e1z := x1-x0, y1-y0, z1-z0
	e2x, e2y, e2z := x2-x0, y2-y0, z2-z0
	// p = dir × e2
	px := dir.Y*e2z - dir.Z*e2y
	py := dir.Z*e2x - dir.X*e2z
	pz := dir.X*e2y - dir.Y*e2x
	det := e1x*px + e1y*py + e1z*pz
	if det > -eps && det < eps {
		return 0, false // parallel
	}
	inv := 1.0 / det
	tx := orig.X - x0
	ty := orig.Y - y0
	tz := orig.Z - z0
	u := (tx*px + ty*py + tz*pz) * inv
	if u < 0 || u > 1 {
		return 0, false
	}
	// q = tvec × e1
	qx := ty*e1z - tz*e1y
	qy := tz*e1x - tx*e1z
	qz := tx*e1y - ty*e1x
	v := (dir.X*qx + dir.Y*qy + dir.Z*qz) * inv
	if v < 0 || u+v > 1 {
		return 0, false
	}
	t := (e2x*qx + e2y*qy + e2z*qz) * inv
	return t, true
}

// Mesh is a map's collision geometry plus a BVH over its triangles.
type Mesh struct {
	tris  []triangle
	nodes []bvhNode
}

// Triangles reports how many triangles the mesh holds (for logging).
func (m *Mesh) Triangles() int {
	if m == nil {
		return 0
	}
	return len(m.tris)
}

// Bytes estimates the mesh's resident size. Meshes dominate this process's
// memory, so logging it makes a regression in either array obvious without
// having to attach a profiler to a running pod.
func (m *Mesh) Bytes() int {
	if m == nil {
		return 0
	}
	return cap(m.tris)*int(unsafe.Sizeof(triangle{})) +
		cap(m.nodes)*int(unsafe.Sizeof(bvhNode{}))
}

// Occluded reports whether any world triangle lies on the segment between two
// eye points — i.e. there is NO clear line of sight. A nil/empty mesh returns
// false (treat as visible) so callers fall back to unvalidated behaviour.
func (m *Mesh) Occluded(from, to r3.Vector) bool {
	if m == nil || len(m.tris) == 0 {
		return false
	}
	dir := r3.Vector{X: to.X - from.X, Y: to.Y - from.Y, Z: to.Z - from.Z}
	segLen := math.Sqrt(dir.X*dir.X + dir.Y*dir.Y + dir.Z*dir.Z)
	if segLen < 1e-6 {
		return false
	}
	epsT := endEps / segLen
	if epsT > 0.45 {
		epsT = 0.45
	}
	return m.anyHit(from, dir, epsT, 1-epsT)
}

// RayHitDist returns the distance to the nearest world triangle along a ray
// (dir need not be normalized). Kept for on-wall / into-air refinements.
func (m *Mesh) RayHitDist(origin, dir r3.Vector) (float64, bool) {
	if m == nil || len(m.tris) == 0 {
		return 0, false
	}
	l := math.Sqrt(dir.X*dir.X + dir.Y*dir.Y + dir.Z*dir.Z)
	if l < 1e-9 {
		return 0, false
	}
	d := r3.Vector{X: dir.X / l, Y: dir.Y / l, Z: dir.Z / l}
	return m.nearestHit(origin, d)
}

func safeInv(x float64) float64 {
	if x == 0 {
		return math.Inf(1)
	}
	return 1.0 / x
}

// SurfaceHit is the nearest world surface along a ray.
type SurfaceHit struct {
	// Distance is the range from the ray origin, in source units.
	Distance float64
	// Normal is the unit surface normal, always oriented back towards the ray
	// (Normal·dir < 0). The .tri meshes are unwound — a wall's triangles can
	// face either way — so a normal taken straight from the winding is only
	// right half the time, and a bounce computed off a flipped one drives the
	// grenade through the wall instead of off it.
	Normal r3.Vector
}

// RayHitSurface returns the nearest surface along a ray (dir need not be
// normalized), with the normal to bounce off it. ok is false when nothing is
// hit, or when the mesh is empty.
func (m *Mesh) RayHitSurface(origin, dir r3.Vector) (SurfaceHit, bool) {
	if m == nil || len(m.tris) == 0 {
		return SurfaceHit{}, false
	}
	l := math.Sqrt(dir.X*dir.X + dir.Y*dir.Y + dir.Z*dir.Z)
	if l < 1e-9 {
		return SurfaceHit{}, false
	}
	d := r3.Vector{X: dir.X / l, Y: dir.Y / l, Z: dir.Z / l}
	t, tri, ok := m.nearestHitTri(origin, d)
	if !ok {
		return SurfaceHit{}, false
	}
	x0, y0, z0, x1, y1, z1, x2, y2, z2 := tri.corners()
	e1 := r3.Vector{X: x1 - x0, Y: y1 - y0, Z: z1 - z0}
	e2 := r3.Vector{X: x2 - x0, Y: y2 - y0, Z: z2 - z0}
	n := e1.Cross(e2)
	nl := n.Norm()
	if nl < 1e-12 {
		return SurfaceHit{}, false // degenerate triangle: no surface to bounce off
	}
	n = n.Mul(1 / nl)
	if n.Dot(d) > 0 {
		n = n.Mul(-1)
	}
	return SurfaceHit{Distance: t, Normal: n}, true
}

// Bounds is the mesh's world AABB, straight off the BVH root. ok is false for
// an empty mesh. Callers use it to notice a simulation that has left the map
// rather than integrating it forever.
func (m *Mesh) Bounds() (min, max r3.Vector, ok bool) {
	if m == nil || len(m.nodes) == 0 {
		return r3.Vector{}, r3.Vector{}, false
	}
	return m.nodes[0].min, m.nodes[0].max, true
}
