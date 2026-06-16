// Package geometry loads a map's collision mesh (.tri) and answers
// line-of-sight / raycast queries against it, so the demo parser can tell
// whether two players actually had a clear sightline (vs. a "spot" through
// smoke, a thin gap, or the edge of vision). Coordinates are raw CS2 source
// units (Z-up) — the same space as p.PositionEyes(), so no transform is
// needed.
package geometry

import (
	"math"

	"github.com/golang/geo/r3"
)

// endEps pulls a segment's endpoints inward (source units) so the surface a
// player hugs / stands on doesn't register as an occluder of its own sightline.
const endEps = 2.0

type triangle struct {
	v0     r3.Vector
	e1, e2 r3.Vector // v1-v0, v2-v0 (precomputed for Möller–Trumbore)
	min    r3.Vector
	max    r3.Vector
	cx     float64 // centroid (for BVH median split)
	cy     float64
	cz     float64
}

func newTriangle(a, b, c r3.Vector) triangle {
	return triangle{
		v0: a,
		e1: r3.Vector{X: b.X - a.X, Y: b.Y - a.Y, Z: b.Z - a.Z},
		e2: r3.Vector{X: c.X - a.X, Y: c.Y - a.Y, Z: c.Z - a.Z},
		min: r3.Vector{
			X: math.Min(a.X, math.Min(b.X, c.X)),
			Y: math.Min(a.Y, math.Min(b.Y, c.Y)),
			Z: math.Min(a.Z, math.Min(b.Z, c.Z)),
		},
		max: r3.Vector{
			X: math.Max(a.X, math.Max(b.X, c.X)),
			Y: math.Max(a.Y, math.Max(b.Y, c.Y)),
			Z: math.Max(a.Z, math.Max(b.Z, c.Z)),
		},
		cx: (a.X + b.X + c.X) / 3,
		cy: (a.Y + b.Y + c.Y) / 3,
		cz: (a.Z + b.Z + c.Z) / 3,
	}
}

// rayTriangle returns the parametric distance t (point = orig + t*dir) of the
// intersection, double-sided (walls can face either way). ok is false when the
// ray misses or is parallel.
func rayTriangle(orig, dir r3.Vector, tr *triangle) (float64, bool) {
	const eps = 1e-9
	// p = dir × e2
	px := dir.Y*tr.e2.Z - dir.Z*tr.e2.Y
	py := dir.Z*tr.e2.X - dir.X*tr.e2.Z
	pz := dir.X*tr.e2.Y - dir.Y*tr.e2.X
	det := tr.e1.X*px + tr.e1.Y*py + tr.e1.Z*pz
	if det > -eps && det < eps {
		return 0, false // parallel
	}
	inv := 1.0 / det
	tx := orig.X - tr.v0.X
	ty := orig.Y - tr.v0.Y
	tz := orig.Z - tr.v0.Z
	u := (tx*px + ty*py + tz*pz) * inv
	if u < 0 || u > 1 {
		return 0, false
	}
	// q = tvec × e1
	qx := ty*tr.e1.Z - tz*tr.e1.Y
	qy := tz*tr.e1.X - tx*tr.e1.Z
	qz := tx*tr.e1.Y - ty*tr.e1.X
	v := (dir.X*qx + dir.Y*qy + dir.Z*qz) * inv
	if v < 0 || u+v > 1 {
		return 0, false
	}
	t := (tr.e2.X*qx + tr.e2.Y*qy + tr.e2.Z*qz) * inv
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
