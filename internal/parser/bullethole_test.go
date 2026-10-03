package parser

import (
	"math"
	"testing"

	"github.com/golang/geo/r3"
)

// fullHole is a tunnel at the strength it has the moment the shot is fired.
func fullHole(from, to r3.Vector) smokeClearing {
	return smokeClearing{holes: []activeHole{{
		from:     from,
		to:       to,
		radiusSq: bulletHoleRadius * bulletHoleRadius,
		fullSq:   bulletHoleFullRadius * bulletHoleFullRadius,
	}}}
}

// Spraying into a smoke opens a sightline straight down the line of fire, in
// both directions.
func TestBulletHoleOpensTheLineOfFire(t *testing.T) {
	v, center := openSpaceVolume(t)
	// Off the grid's axes, so the line does not run through cell centres.
	from := r3.Vector{X: -400, Y: 7, Z: 5}
	to := r3.Vector{X: 400, Y: 7, Z: 5}

	if !v.occludedSegment(from, to, center, smokeRadius, smokeClearing{}) {
		t.Fatal("an intact cloud should block the sightline")
	}
	hole := fullHole(from, to)
	if v.occludedSegment(from, to, center, smokeRadius, hole) {
		t.Fatal("a tracer down the sightline should open it")
	}
	if v.occludedSegment(to, from, center, smokeRadius, hole) {
		t.Fatal("the tunnel should open the sightline back at the shooter too")
	}
	// The line of fire is the crosshair, not the bullet, so a target a few
	// units off it is still seen down the tunnel.
	if v.occludedSegment(from, r3.Vector{X: 400, Y: 19, Z: 5}, center, smokeRadius, hole) {
		t.Fatal("a sightline a few units off the tracer should still be open")
	}
}

// A tunnel is narrow: it does not open sightlines that cross it, or run beside
// it, the way a blast opens everything nearby.
func TestBulletHoleIsATunnelNotABlast(t *testing.T) {
	v, center := openSpaceVolume(t)
	hole := fullHole(r3.Vector{X: -400}, r3.Vector{X: 400})

	across := [2]r3.Vector{{Y: -400}, {Y: 400}}
	if !v.occludedSegment(across[0], across[1], center, smokeRadius, hole) {
		t.Fatal("a sightline crossing the tunnel should still be blocked")
	}
	beside := [2]r3.Vector{{X: -400, Y: 64}, {X: 400, Y: 64}}
	if !v.occludedSegment(beside[0], beside[1], center, smokeRadius, smokeClearing{}) {
		t.Fatal("the sightline beside the tunnel should be blocked by an intact cloud")
	}
	if !v.occludedSegment(beside[0], beside[1], center, smokeRadius, hole) {
		t.Fatal("a sightline running beside the tunnel should still be blocked")
	}
}

// The cloud fills back in, so the tunnel is brief.
func TestBulletHoleClosesAgain(t *testing.T) {
	h := bulletHole{tick: 100}

	if got := h.radiusAt(100, testRate); got != bulletHoleRadius {
		t.Fatalf("the tunnel should be full width when fired, got %.0f", got)
	}
	mid := h.radiusAt(100+int(testRate*bulletHoleSecs/2), testRate)
	if mid <= 0 || mid >= bulletHoleRadius {
		t.Fatalf("the tunnel should be closing part-way through, got %.0f", mid)
	}
	if got := h.radiusAt(100+int(testRate*bulletHoleSecs)+1, testRate); got != 0 {
		t.Fatalf("the tunnel should have closed, but is still %.0f wide", got)
	}
	if got := h.radiusAt(99, testRate); got != 0 {
		t.Fatal("a tunnel should not exist before the shot that makes it")
	}
}

// End to end through the state: only tracers near a live cloud are kept, and
// the sightline they open is open only while the tunnel lasts — including for
// callers that re-check a tick from before the shot.
func TestRecordedBulletHoleOpensThenCloses(t *testing.T) {
	v, center := openSpaceVolume(t)
	s := &state{res: &Result{}, tickRate: testRate, smokeByEnt: map[int]int{}}
	s.smokes = []smokeCloud{{center: center, startTick: 0, endTick: 100000, vol: v, exportIdx: -1}}

	shot := int(testRate * smokeBloomSecs * 2)
	from := r3.Vector{X: -400}
	to := r3.Vector{X: 400}

	s.recordBulletHole(shot, r3.Vector{X: -400, Y: 3000}, r3.Vector{X: 400, Y: 3000})
	if len(s.holes) != 0 {
		t.Fatal("a tracer nowhere near the smoke should not be recorded")
	}
	s.recordBulletHole(0, from, to)
	if len(s.holes) != 0 {
		t.Fatal("a tracer through a cloud that has not bloomed yet should not be recorded")
	}

	s.recordBulletHole(shot, from, to)
	if len(s.holes) != 1 {
		t.Fatalf("a tracer through the smoke should be recorded, have %d", len(s.holes))
	}
	if !s.smokeOccluded(shot-1, from, to) {
		t.Fatal("the sightline should be blocked the tick before the shot")
	}
	if s.smokeOccluded(shot, from, to) {
		t.Fatal("the sightline should be open the moment the tracer passes")
	}
	if s.holeLetTh == 0 {
		t.Fatal("the opened sightline should be credited to the tunnel")
	}
	if !s.smokeOccluded(shot+int(testRate*bulletHoleSecs)+1, from, to) {
		t.Fatal("the sightline should be blocked again once the tunnel closes")
	}

	s.resetSmokes()
	if len(s.holes) != 0 {
		t.Fatal("holes should be dropped at the round boundary")
	}
}

func TestSegmentDistSq(t *testing.T) {
	cases := []struct {
		name           string
		p1, q1, p2, q2 r3.Vector
		want           float64
	}{
		{"crossing", r3.Vector{X: -1}, r3.Vector{X: 1}, r3.Vector{Y: -1}, r3.Vector{Y: 1}, 0},
		{"skew", r3.Vector{X: -1}, r3.Vector{X: 1}, r3.Vector{Y: -1, Z: 5}, r3.Vector{Y: 1, Z: 5}, 5},
		{"parallel", r3.Vector{X: -1}, r3.Vector{X: 1}, r3.Vector{X: -3, Y: 2}, r3.Vector{X: 3, Y: 2}, 2},
		{"clamped past the end", r3.Vector{}, r3.Vector{X: 1}, r3.Vector{X: 4, Y: -1}, r3.Vector{X: 4, Y: 1}, 3},
		{"point to segment", r3.Vector{X: 2, Y: 3}, r3.Vector{X: 2, Y: 3}, r3.Vector{}, r3.Vector{X: 4}, 3},
		{"two points", r3.Vector{}, r3.Vector{}, r3.Vector{X: 3, Y: 4}, r3.Vector{X: 3, Y: 4}, 5},
	}
	for _, c := range cases {
		got := math.Sqrt(segmentDistSq(c.p1, c.q1, c.p2, c.q2))
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: distance %.6f, want %.6f", c.name, got, c.want)
		}
		if back := math.Sqrt(segmentDistSq(c.p2, c.q2, c.p1, c.q1)); math.Abs(back-c.want) > 1e-9 {
			t.Errorf("%s (swapped): distance %.6f, want %.6f", c.name, back, c.want)
		}
	}
	if got := math.Sqrt(pointSegmentDistSq(r3.Vector{X: 2, Y: 3}, r3.Vector{}, r3.Vector{X: 4})); got != 3 {
		t.Errorf("point to segment: %.6f, want 3", got)
	}
}
