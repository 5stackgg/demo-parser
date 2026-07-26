package parser

import (
	"math"
	"testing"

	"github.com/golang/geo/r3"
)

// openSpaceVolume builds a cloud with nothing around it to obstruct it.
func openSpaceVolume(t *testing.T) (*smokeVolume, r3.Vector) {
	t.Helper()
	mesh := meshFromBlob(t, triBlob([3]r3.Vector{
		{X: 9000, Y: 9000, Z: 9000},
		{X: 9100, Y: 9000, Z: 9000},
		{X: 9100, Y: 9100, Z: 9000},
	}))
	center := r3.Vector{}
	v, _ := buildSmokeVolume(mesh, center)
	return v, center
}

// A settled cloud is an oblate spheroid, not a ball: it spreads sideways
// further than it climbs. Occupancy is decided per cell centre, which is
// unbiased in volume — cells protruding past the surface are balanced by
// notches where it is clipped — so an unobstructed cloud should land near the
// analytic volume of that spheroid.
//
// Worth pinning, because the obvious "fix" for boundary cells protruding
// (pulling the test in by half a cell) silently costs 16% of the cloud.
func TestOpenSpaceCloudMatchesSpheroidVolume(t *testing.T) {
	v, _ := openSpaceVolume(t)

	r := smokeRadius / smokeVoxelSize
	ideal := (4.0 / 3.0) * math.Pi * r * r * (r * smokeVerticalScale)
	got := float64(v.count())
	// The faint-haze cutoff in the blur trims the outermost shell, so the count
	// sits a little under the analytic figure.
	if err := (ideal - got) / ideal; err < -0.05 || err > 0.15 {
		t.Fatalf("open-space cloud is %.0f cells, spheroid is %.0f (%.1f%% off)",
			got, ideal, err*100)
	}
}

// The cloud's own extent should reach its radius sideways and fall short of it
// vertically, since it settles rather than floating. Measured on the density
// field itself, independent of how much of it is needed to hide someone.
func TestCloudExtentIsWiderThanItIsTall(t *testing.T) {
	v, center := openSpaceVolume(t)

	extent := func(step r3.Vector) float64 {
		furthest := 0.0
		for d := 0.0; d <= smokeRadius*1.5; d += v.size / 2 {
			p := r3.Vector{X: center.X + step.X*d, Y: center.Y + step.Y*d, Z: center.Z + step.Z*d}
			i := int(math.Floor((p.X - v.origin.X) / v.size))
			j := int(math.Floor((p.Y - v.origin.Y) / v.size))
			k := int(math.Floor((p.Z - v.origin.Z) / v.size))
			if v.get(i, j, k) {
				furthest = d
			}
		}
		return furthest
	}

	horizontal := extent(r3.Vector{X: 1})
	vertical := extent(r3.Vector{Z: 1})

	if horizontal < smokeRadius-2*smokeVoxelSize || horizontal > smokeRadius+2*smokeVoxelSize {
		t.Fatalf("horizontal reach %.0f, want about %.0f", horizontal, smokeRadius)
	}
	if vertical >= horizontal {
		t.Fatalf("a settled cloud should be wider (%.0f) than tall (%.0f)", horizontal, vertical)
	}
	wantVertical := smokeRadius * smokeVerticalScale
	if math.Abs(vertical-wantVertical) > 2*smokeVoxelSize {
		t.Fatalf("vertical reach %.0f, want about %.0f", vertical, wantVertical)
	}
}

// A sightline through the core must be blocked and one clipping the rim must
// not — that separation is the whole point of accumulating depth rather than
// stopping at the first cell.
func TestCoreBlocksAndRimDoesNot(t *testing.T) {
	v, center := openSpaceVolume(t)

	through := v.opticalDepth(r3.Vector{X: -500}, r3.Vector{X: 500}, center, smokeRadius, nil)
	if through < blockingDepth {
		t.Fatalf("a sightline through the core has depth %.2f, expected it to block (>= %.1f)",
			through, blockingDepth)
	}

	// Out near the edge, where the cloud is thin enough to see through.
	rim := v.opticalDepth(
		r3.Vector{X: -500, Y: smokeRadius * 0.9},
		r3.Vector{X: 500, Y: smokeRadius * 0.9},
		center, smokeRadius, nil)
	if rim >= blockingDepth {
		t.Fatalf("a sightline clipping the rim has depth %.2f, expected it not to block", rim)
	}
	if rim <= 0 {
		t.Fatal("a sightline clipping the rim should still pass through some smoke")
	}
}
