package parser

import (
	"math"
	"strings"

	"github.com/golang/geo/r3"
	st "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/sendtables"
)

// Doors are missing from the collision meshes we raycast against: those are
// built from the map's `world_physics` hull, which holds only static world
// geometry. Every closed door on Nuke, Overpass or Cache is therefore a free
// sightline unless we reconstruct it here.
//
// A demo carries everything needed. `m_vecMins`/`m_vecMaxs` give the door
// leaf's box in its own frame with the hinge at the origin (~57 wide × 2 thick
// × 112 tall), and `CBodyComponent.m_angRotation` gives the live orientation —
// a continuous angle, not a boolean, so a half-open door occludes exactly the
// half it covers. `m_eDoorState` is deliberately not used as the source of
// truth for that reason.
type doorLeaf struct {
	ent    st.Entity
	origin r3.Vector
	mins   r3.Vector
	maxs   r3.Vector

	// Rotation the cached transform was built for, so a stationary door costs
	// nothing per frame.
	ang     r3.Vector
	angSet  bool
	axes    [3]r3.Vector // world direction of each local axis
	worldLo r3.Vector    // world AABB of the rotated box, for a cheap reject
	worldHi r3.Vector
}

// doorRescanTicks is how often the entity table is re-walked for doors. They
// are created and destroyed over the course of a match rather than existing
// from the start, so a single scan misses most of them.
const doorRescanTicks = 128

// scanDoors rebuilds the tracked door list from the entity table.
func (s *state) scanDoors() {
	s.doors = s.doors[:0]
	for _, e := range s.parser.GameState().Entities() {
		if e == nil || e.ServerClass() == nil {
			continue
		}
		if !strings.Contains(e.ServerClass().Name(), "Door") {
			continue
		}
		s.doorSeen[e.ID()] = true
		mins, okMin := vecProp(e, "m_vecMins")
		maxs, okMax := vecProp(e, "m_vecMaxs")
		if !okMin || !okMax {
			s.doorsNoBounds++
			continue
		}
		// A leaf with no span can't occlude anything.
		if maxs.X-mins.X < 1 || maxs.Z-mins.Z < 1 {
			s.doorsDegenerate++
			continue
		}
		origin := e.Position()
		if origin.X == 0 && origin.Y == 0 && origin.Z == 0 {
			if p, ok := vecProp(e, "m_closedPosition"); ok {
				origin = p
			} else {
				s.doorsNoOrigin++
				continue
			}
		}
		s.doors = append(s.doors, &doorLeaf{ent: e, origin: origin, mins: mins, maxs: maxs})
	}
	if len(s.doors) > s.doorsTracked {
		s.doorsTracked = len(s.doors)
	}
}

// updateDoors refreshes each door's transform when its rotation has changed,
// re-scanning the entity table periodically so doors created mid-match are
// picked up.
func (s *state) updateDoors() {
	// Rescan strictly on the timer. Keying off an empty door list instead would
	// walk the whole entity table every frame on maps that have no doors at all.
	tick := s.parser.GameState().IngameTick()
	if tick-s.doorLastScan >= doorRescanTicks {
		s.doorLastScan = tick
		s.scanDoors()
	}
	for _, d := range s.doors {
		ang, ok := vecProp(d.ent, "CBodyComponent.m_angRotation")
		if !ok {
			continue
		}
		if d.angSet && ang == d.ang {
			continue
		}
		if d.angSet {
			s.doorsMoved++
		}
		d.ang, d.angSet = ang, true
		d.rebuild()
	}
}

// rebuild recomputes the world-space axes and bounding box for the current
// rotation. Angles follow Source's QAngle order — pitch, yaw, roll — and the
// matrix matches the engine's AngleMatrix so a door hung at any orientation
// lands correctly, not just axis-aligned ones.
func (d *doorLeaf) rebuild() {
	const rad = math.Pi / 180
	sp, cp := math.Sincos(d.ang.X * rad)
	sy, cy := math.Sincos(d.ang.Y * rad)
	sr, cr := math.Sincos(d.ang.Z * rad)

	d.axes[0] = r3.Vector{X: cp * cy, Y: cp * sy, Z: -sp}
	d.axes[1] = r3.Vector{X: sr*sp*cy - cr*sy, Y: sr*sp*sy + cr*cy, Z: sr * cp}
	d.axes[2] = r3.Vector{X: cr*sp*cy + sr*sy, Y: cr*sp*sy - sr*cy, Z: cr * cp}

	lo := r3.Vector{X: math.Inf(1), Y: math.Inf(1), Z: math.Inf(1)}
	hi := r3.Vector{X: math.Inf(-1), Y: math.Inf(-1), Z: math.Inf(-1)}
	for i := 0; i < 8; i++ {
		local := r3.Vector{X: d.mins.X, Y: d.mins.Y, Z: d.mins.Z}
		if i&1 != 0 {
			local.X = d.maxs.X
		}
		if i&2 != 0 {
			local.Y = d.maxs.Y
		}
		if i&4 != 0 {
			local.Z = d.maxs.Z
		}
		w := d.toWorld(local)
		lo = r3.Vector{X: math.Min(lo.X, w.X), Y: math.Min(lo.Y, w.Y), Z: math.Min(lo.Z, w.Z)}
		hi = r3.Vector{X: math.Max(hi.X, w.X), Y: math.Max(hi.Y, w.Y), Z: math.Max(hi.Z, w.Z)}
	}
	d.worldLo, d.worldHi = lo, hi
}

func (d *doorLeaf) toWorld(l r3.Vector) r3.Vector {
	return r3.Vector{
		X: d.origin.X + d.axes[0].X*l.X + d.axes[1].X*l.Y + d.axes[2].X*l.Z,
		Y: d.origin.Y + d.axes[0].Y*l.X + d.axes[1].Y*l.Y + d.axes[2].Y*l.Z,
		Z: d.origin.Z + d.axes[0].Z*l.X + d.axes[1].Z*l.Y + d.axes[2].Z*l.Z,
	}
}

// toLocal is the inverse transform. The axis matrix is orthonormal, so its
// inverse is its transpose — a dot product per axis.
func (d *doorLeaf) toLocal(w r3.Vector) r3.Vector {
	rel := r3.Vector{X: w.X - d.origin.X, Y: w.Y - d.origin.Y, Z: w.Z - d.origin.Z}
	return r3.Vector{
		X: rel.X*d.axes[0].X + rel.Y*d.axes[0].Y + rel.Z*d.axes[0].Z,
		Y: rel.X*d.axes[1].X + rel.Y*d.axes[1].Y + rel.Z*d.axes[1].Z,
		Z: rel.X*d.axes[2].X + rel.Y*d.axes[2].Y + rel.Z*d.axes[2].Z,
	}
}

// doorOccluded reports whether any tracked door leaf lies across the segment.
func (s *state) doorOccluded(from, to r3.Vector) bool {
	if len(s.doors) == 0 {
		return false
	}
	for _, d := range s.doors {
		if !d.angSet {
			continue
		}
		// Cheap world-space reject before the per-door frame change.
		if !segmentAABB(from, to, d.worldLo, d.worldHi) {
			continue
		}
		if segmentAABB(d.toLocal(from), d.toLocal(to), d.mins, d.maxs) {
			s.doorBlocks++
			return true
		}
	}
	return false
}

// segmentAABB reports whether the segment from→to intersects the box, using the
// slab method clamped to the segment's own extent.
func segmentAABB(from, to, lo, hi r3.Vector) bool {
	t0, t1 := 0.0, 1.0
	d := [3]float64{to.X - from.X, to.Y - from.Y, to.Z - from.Z}
	o := [3]float64{from.X, from.Y, from.Z}
	mn := [3]float64{lo.X, lo.Y, lo.Z}
	mx := [3]float64{hi.X, hi.Y, hi.Z}
	for i := 0; i < 3; i++ {
		if math.Abs(d[i]) < 1e-9 {
			if o[i] < mn[i] || o[i] > mx[i] {
				return false
			}
			continue
		}
		inv := 1 / d[i]
		a := (mn[i] - o[i]) * inv
		b := (mx[i] - o[i]) * inv
		if a > b {
			a, b = b, a
		}
		if a > t0 {
			t0 = a
		}
		if b < t1 {
			t1 = b
		}
		if t0 > t1 {
			return false
		}
	}
	return true
}

func vecProp(e st.Entity, name string) (r3.Vector, bool) {
	v, ok := e.PropertyValue(name)
	if !ok {
		return r3.Vector{}, false
	}
	p := v.R3VecOrNil()
	if p == nil {
		return r3.Vector{}, false
	}
	return *p, true
}
