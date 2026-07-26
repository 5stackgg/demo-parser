package parser

import (
	"testing"

	"github.com/golang/geo/r3"
)

// testDoor builds a leaf with the dimensions actually observed on de_cache
// (~57 wide × 2 thick × 112 tall), hinged at the world origin, swung to the
// given yaw.
func testDoor(yawDeg float64) *doorLeaf {
	d := &doorLeaf{
		origin: r3.Vector{},
		mins:   r3.Vector{X: -0.125, Y: -0.970, Z: -0.056},
		maxs:   r3.Vector{X: 56.996, Y: 1.052, Z: 112.429},
		ang:    r3.Vector{Y: yawDeg},
		angSet: true,
	}
	d.rebuild()
	return d
}

// A sightline through the doorway at chest height, crossing where a closed
// door's leaf sits.
func throughDoorway() (r3.Vector, r3.Vector) {
	return r3.Vector{X: 30, Y: -100, Z: 60}, r3.Vector{X: 30, Y: 100, Z: 60}
}

func TestClosedDoorBlocksSightline(t *testing.T) {
	s := &state{res: &Result{}, doors: []*doorLeaf{testDoor(0)}}
	from, to := throughDoorway()
	if !s.doorOccluded(from, to) {
		t.Fatal("a closed door should block a sightline through its doorway")
	}
}

func TestOpenDoorDoesNotBlock(t *testing.T) {
	// Swung 90°, the leaf lies along +Y and no longer spans the doorway at X=30.
	s := &state{res: &Result{}, doors: []*doorLeaf{testDoor(90)}}
	from, to := throughDoorway()
	if s.doorOccluded(from, to) {
		t.Fatal("a fully open door should not block the doorway it swung out of")
	}
}

// The angle is continuous, so a half-open door occludes exactly the part of the
// doorway its leaf still covers — the reason m_eDoorState isn't the source of
// truth.
func TestHalfOpenDoorOccludesOnlyWhatItCovers(t *testing.T) {
	s := &state{res: &Result{}, doors: []*doorLeaf{testDoor(45)}}

	// At 45° the leaf reaches about 40 units along each of X and Y, so a
	// sightline at X=30 still crosses it...
	if !s.doorOccluded(r3.Vector{X: 30, Y: -100, Z: 60}, r3.Vector{X: 30, Y: 100, Z: 60}) {
		t.Fatal("a half-open door should still block the part of the doorway it covers")
	}
	// ...while one past the leaf's reach reaches through.
	if s.doorOccluded(r3.Vector{X: 50, Y: -100, Z: 60}, r3.Vector{X: 50, Y: 100, Z: 60}) {
		t.Fatal("a half-open door should not block beyond the leaf's reach")
	}
}

func TestDoorDoesNotBlockOverOrUnderIt(t *testing.T) {
	s := &state{res: &Result{}, doors: []*doorLeaf{testDoor(0)}}
	from, to := r3.Vector{X: 30, Y: -100}, r3.Vector{X: 30, Y: 100}

	over := from
	over.Z, to.Z = 200, 200
	if s.doorOccluded(over, to) {
		t.Fatal("a sightline above the door should not be blocked")
	}
	under := from
	under.Z, to.Z = -50, -50
	if s.doorOccluded(under, to) {
		t.Fatal("a sightline below the door should not be blocked")
	}
}

// A door hung at an arbitrary yaw must still be positioned correctly — the
// transform is a full Source AngleMatrix, not an axis-aligned special case.
func TestDoorTransformRoundTrips(t *testing.T) {
	d := &doorLeaf{
		origin: r3.Vector{X: 204, Y: 2046, Z: 1688},
		mins:   r3.Vector{X: -0.125, Y: -0.970, Z: -0.056},
		maxs:   r3.Vector{X: 56.996, Y: 1.052, Z: 112.429},
		ang:    r3.Vector{X: 5, Y: 37, Z: -11},
		angSet: true,
	}
	d.rebuild()

	local := r3.Vector{X: 40, Y: 0.5, Z: 80}
	back := d.toLocal(d.toWorld(local))
	for _, c := range [][2]float64{{local.X, back.X}, {local.Y, back.Y}, {local.Z, back.Z}} {
		if diff := c[0] - c[1]; diff > 1e-6 || diff < -1e-6 {
			t.Fatalf("toLocal(toWorld(%v)) = %v, want a round trip", local, back)
		}
	}
}

// The world AABB is only a reject filter, so it must never be tighter than the
// rotated box it wraps.
func TestDoorWorldBoundsContainTheRotatedLeaf(t *testing.T) {
	d := testDoor(37)
	for i := 0; i < 8; i++ {
		l := d.mins
		if i&1 != 0 {
			l.X = d.maxs.X
		}
		if i&2 != 0 {
			l.Y = d.maxs.Y
		}
		if i&4 != 0 {
			l.Z = d.maxs.Z
		}
		w := d.toWorld(l)
		if w.X < d.worldLo.X-1e-6 || w.X > d.worldHi.X+1e-6 ||
			w.Y < d.worldLo.Y-1e-6 || w.Y > d.worldHi.Y+1e-6 ||
			w.Z < d.worldLo.Z-1e-6 || w.Z > d.worldHi.Z+1e-6 {
			t.Fatalf("corner %v falls outside world bounds %v..%v", w, d.worldLo, d.worldHi)
		}
	}
}

func TestSegmentAABBEndpointHandling(t *testing.T) {
	lo := r3.Vector{X: -1, Y: -1, Z: -1}
	hi := r3.Vector{X: 1, Y: 1, Z: 1}

	if segmentAABB(r3.Vector{X: 5}, r3.Vector{X: 10}, lo, hi) {
		t.Fatal("a segment entirely past the box should not intersect it")
	}
	if !segmentAABB(r3.Vector{X: -5}, r3.Vector{X: 5}, lo, hi) {
		t.Fatal("a segment passing through the box should intersect it")
	}
	// Stops short — the slab test must respect the segment's extent, not treat
	// it as an infinite ray.
	if segmentAABB(r3.Vector{X: -10}, r3.Vector{X: -5}, lo, hi) {
		t.Fatal("a segment stopping before the box should not intersect it")
	}
	// Degenerate direction on one axis, inside the slab on that axis.
	if !segmentAABB(r3.Vector{X: 0, Y: -5}, r3.Vector{X: 0, Y: 5}, lo, hi) {
		t.Fatal("a segment with no X extent but inside the X slab should intersect")
	}
}
