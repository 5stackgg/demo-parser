package geometry

import (
	"math"

	"github.com/golang/geo/r3"
)

// GrenadeWorld is what a thrown grenade collides with: the collision hull plus
// the map's grenade clips. Clips stop grenades and nothing else, so they are a
// separate mesh that line of sight never sees, and a query here takes the
// nearer hit of the two BVHs rather than building a merged one.
type GrenadeWorld struct {
	hull     *Mesh
	clip     *Mesh
	revision string
}

// NewGrenadeWorld is nil without a hull: grenade clips alone are not a map.
func NewGrenadeWorld(hull, clip *Mesh) *GrenadeWorld {
	if hull == nil {
		return nil
	}
	return &GrenadeWorld{hull: hull, clip: clip}
}

// Revision is the canonical reference the world was loaded at, in the form
// ResolveMeshRevision returns, so a report names exactly what was flown.
func (w *GrenadeWorld) Revision() string {
	if w == nil {
		return ""
	}
	return w.revision
}

func (w *GrenadeWorld) Hull() *Mesh {
	if w == nil {
		return nil
	}
	return w.hull
}

func (w *GrenadeWorld) Triangles() int {
	if w == nil {
		return 0
	}
	return w.hull.Triangles() + w.clip.Triangles()
}

func (w *GrenadeWorld) GrenadeClipTriangles() int {
	if w == nil {
		return 0
	}
	return w.clip.Triangles()
}

// RayHitSurface is Mesh.RayHitSurface over both meshes. A tie goes to the hull,
// so the answer never depends on anything but the geometry.
func (w *GrenadeWorld) RayHitSurface(origin, dir r3.Vector) (SurfaceHit, bool) {
	if w == nil {
		return SurfaceHit{}, false
	}
	hit, ok := w.hull.RayHitSurface(origin, dir)
	if clip, clipOK := w.clip.RayHitSurface(origin, dir); clipOK && (!ok || clip.Distance < hit.Distance) {
		return clip, true
	}
	return hit, ok
}

// Bounds is the union of both meshes' AABBs.
func (w *GrenadeWorld) Bounds() (min, max r3.Vector, ok bool) {
	if w == nil {
		return r3.Vector{}, r3.Vector{}, false
	}
	min, max, ok = w.hull.Bounds()
	cmin, cmax, cok := w.clip.Bounds()
	if !cok {
		return min, max, ok
	}
	if !ok {
		return cmin, cmax, true
	}
	return r3.Vector{X: math.Min(min.X, cmin.X), Y: math.Min(min.Y, cmin.Y), Z: math.Min(min.Z, cmin.Z)},
		r3.Vector{X: math.Max(max.X, cmax.X), Y: math.Max(max.Y, cmax.Y), Z: math.Max(max.Z, cmax.Z)},
		true
}
