package parser

import (
	"testing"

	"github.com/golang/geo/r3"
)

// An HE landing in a smoke drives its density to a few percent — transparent —
// so a sightline through the hole was genuinely clear and must not be reported
// as blocked.
func TestBlastOpensAHoleInSmoke(t *testing.T) {
	v, center := openSpaceVolume(t)
	from := r3.Vector{X: -400}
	to := r3.Vector{X: 400}

	if !v.occludedSegment(from, to, center, smokeRadius, nil) {
		t.Fatal("an intact cloud should block the sightline")
	}
	blast := []activeBlast{{center: center, radiusSq: heBlastRadius * heBlastRadius, fullSq: heBlastFullRadius * heBlastFullRadius}}
	if v.occludedSegment(from, to, center, smokeRadius, blast) {
		t.Fatal("a blast in the middle of the cloud should open a sightline through it")
	}
}

// The hole is local: an explosion at the edge of a cloud does not clear a
// sightline running through the far side of it.
func TestBlastOnlyClearsItsOwnRadius(t *testing.T) {
	v, center := openSpaceVolume(t)
	// Blast well outside the cloud, on the +Y side.
	far := r3.Vector{X: 0, Y: 600}
	blast := []activeBlast{{center: far, radiusSq: heBlastRadius * heBlastRadius, fullSq: heBlastFullRadius * heBlastFullRadius}}

	from := r3.Vector{X: -400}
	to := r3.Vector{X: 400}
	if !v.occludedSegment(from, to, center, smokeRadius, blast) {
		t.Fatal("a blast away from the sightline should leave the cloud blocking it")
	}
}

// The cloud fills back in, so the hole is temporary.
func TestBlastClearingDecaysToNothing(t *testing.T) {
	b := smokeBlast{center: r3.Vector{}, tick: 0, radius: heBlastRadius, full: heBlastFullRadius}

	if got := b.clearRadiusAt(0, testRate); got != heBlastRadius {
		t.Fatalf("at the moment of the blast the hole should be full size, got %.0f", got)
	}
	mid := b.clearRadiusAt(int(testRate*blastClearSecs/2), testRate)
	if mid <= 0 || mid >= heBlastRadius {
		t.Fatalf("the hole should be shrinking part-way through, got %.0f", mid)
	}
	if got := b.clearRadiusAt(int(testRate*blastClearSecs)+1, testRate); got != 0 {
		t.Fatalf("the cloud should have filled back in, but the hole is still %.0f", got)
	}
	if got := b.clearRadiusAt(-1, testRate); got != 0 {
		t.Fatal("a blast should not clear smoke before it happens")
	}
}

// blastsAt is what the sightline test consults, so it must drop expired blasts
// and keep live ones.
func TestBlastsAtFiltersByLifetime(t *testing.T) {
	s := &state{res: &Result{}, tickRate: testRate}
	s.blasts = []smokeBlast{
		{center: r3.Vector{X: 100}, tick: 0, radius: heBlastRadius, full: heBlastFullRadius},
		{center: r3.Vector{X: 900}, tick: 1000, radius: c4BlastRadius, full: c4BlastFullRadius},
	}

	live := s.blastsAt(1)
	if len(live) != 1 {
		t.Fatalf("expected only the fresh blast to be live, got %d", len(live))
	}
	if got := s.blastsAt(int(testRate*blastClearSecs) + 2); len(got) != 0 {
		t.Fatalf("expected no live blasts once both have refilled, got %d", len(got))
	}
	if got := s.blastsAt(1001); len(got) != 1 {
		t.Fatalf("expected the later blast to be live at its own tick, got %d", len(got))
	}
}

// A blast at the origin is the common case; guard against it being silently
// dropped as an "unset position".
func TestRecordBlastIgnoresOnlyUnsetPositions(t *testing.T) {
	s := &state{res: &Result{}, tickRate: testRate}
	s.recordBlast(0, r3.Vector{}, heBlastRadius, heBlastFullRadius)
	if len(s.blasts) != 0 {
		t.Fatal("an all-zero position means the coordinate was never resolved")
	}
	s.recordBlast(0, r3.Vector{X: 1}, heBlastRadius, heBlastFullRadius)
	if len(s.blasts) != 1 {
		t.Fatal("a real position should be recorded")
	}
}
