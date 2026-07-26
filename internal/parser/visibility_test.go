package parser

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/golang/geo/r3"
)

// quadTriBlob serializes an axis-aligned quad in the X=0 plane spanning the
// given y and z ranges into the .tri wire format (9 LE float32 per triangle).
func quadTriBlob(y0, y1, z0, z1 float64) []byte {
	a := r3.Vector{X: 0, Y: y0, Z: z0}
	b := r3.Vector{X: 0, Y: y1, Z: z0}
	c := r3.Vector{X: 0, Y: y1, Z: z1}
	d := r3.Vector{X: 0, Y: y0, Z: z1}
	buf := make([]byte, 0, 2*9*4)
	put := func(f float64) {
		var p [4]byte
		binary.LittleEndian.PutUint32(p[:], math.Float32bits(float32(f)))
		buf = append(buf, p[:]...)
	}
	for _, tr := range [][3]r3.Vector{{a, b, c}, {a, c, d}} {
		for _, v := range tr {
			put(v.X)
			put(v.Y)
			put(v.Z)
		}
	}
	return buf
}

// A chest-high wall hides the head but leaves the body exposed — the case a
// single eye-to-eye ray gets wrong and the body samples are there to catch.
func TestVisibleAtFallsBackToBodyWhenHeadIsHidden(t *testing.T) {
	s := &state{res: &Result{}, meshTried: true, tickRate: testRate, smokeByEnt: map[int]int{}}
	s.mesh = meshFromBlob(t, quadTriBlob(-50, 50, 55, 200))

	from := r3.Vector{X: -100, Y: 0, Z: 64}
	eye := r3.Vector{X: 100, Y: 0, Z: 64}
	feet := r3.Vector{X: 100, Y: 0, Z: 0}

	if s.losAt(0, from, eye) {
		t.Fatal("the eye-to-eye ray should be blocked by the wall")
	}
	if !s.visibleAt(0, from, eye, feet) {
		t.Fatal("the body below the wall should still be visible")
	}
}

func TestVisibleAtFalseWhenWholeBodyHidden(t *testing.T) {
	s := &state{res: &Result{}, meshTried: true, tickRate: testRate, smokeByEnt: map[int]int{}}
	s.mesh = meshFromBlob(t, quadTriBlob(-200, 200, -200, 400))

	from := r3.Vector{X: -100, Y: 0, Z: 64}
	eye := r3.Vector{X: 100, Y: 0, Z: 64}
	feet := r3.Vector{X: 100, Y: 0, Z: 0}

	if s.visibleAt(0, from, eye, feet) {
		t.Fatal("a full wall should hide every body sample")
	}
}

func TestVisibleAtRespectsSmoke(t *testing.T) {
	// No world geometry at all: only the cloud can occlude, and it must occlude
	// every body sample, not just the eyes.
	s := &state{res: &Result{}, meshTried: true, tickRate: testRate, smokeByEnt: map[int]int{}}
	s.smokes = []smokeCloud{{center: r3.Vector{X: 0, Y: 0, Z: 40}, startTick: 0}}

	from := r3.Vector{X: -400, Y: 0, Z: 64}
	eye := r3.Vector{X: 400, Y: 0, Z: 64}
	feet := r3.Vector{X: 400, Y: 0, Z: 0}

	if s.visibleAt(int(testRate*smokeBloomSecs), from, eye, feet) {
		t.Fatal("a grown smoke across the sightline should hide the whole player")
	}
	if !s.visibleAt(0, from, eye, feet) {
		t.Fatal("the player should be visible before the smoke blooms")
	}
}

func TestBodySamplePointsStayWithinPlayerBox(t *testing.T) {
	from := r3.Vector{X: -100}
	eye := r3.Vector{X: 100, Y: 0, Z: 64}
	feet := r3.Vector{X: 100, Y: 0, Z: 0}
	for _, p := range bodySamplePoints(from, eye, feet) {
		if p.Z < feet.Z || p.Z > eye.Z {
			t.Fatalf("sample %v outside the player's vertical extent", p)
		}
		if math.Abs(p.Y-eye.Y) > 16 || math.Abs(p.X-eye.X) > 16 {
			t.Fatalf("sample %v outside the ~32-wide player box", p)
		}
	}
}

func TestEyeAsRenderedLagsBehindTheServer(t *testing.T) {
	s := &state{res: &Result{}, tickRate: testRate, eyeHistory: map[string][]eyeSample{}}
	// A player walking down +X at 64 units/tick, sampled every tick.
	for tick := 0; tick <= 6; tick++ {
		s.recordEye("p", tick, r3.Vector{X: float64(tick) * 64})
	}
	got := s.eyeAsRendered("p", 6, r3.Vector{X: 384})

	// clInterpSecs (0.03125s) at 64 tick = 2 ticks back → X of tick 4.
	if math.Abs(got.X-256) > 1e-6 {
		t.Fatalf("eyeAsRendered X = %v, want the position two ticks back (256)", got.X)
	}
}

func TestEyeAsRenderedFallsBackWithoutHistory(t *testing.T) {
	s := &state{res: &Result{}, tickRate: testRate, eyeHistory: map[string][]eyeSample{}}
	fallback := r3.Vector{X: 7}
	if got := s.eyeAsRendered("nobody", 10, fallback); got != fallback {
		t.Fatalf("eyeAsRendered = %v, want the fallback %v", got, fallback)
	}
}

// The frustum is deliberately not a cone: CS2's field of view is much wider
// horizontally than vertically, so the same angular offset is on screen to the
// side and off it above.
func TestOnScreenIsWiderHorizontallyThanVertically(t *testing.T) {
	eye := r3.Vector{}
	const yaw, pitch = 0, 0 // looking down +X, level

	at := func(h, v float64) r3.Vector {
		// A point 100 units ahead, offset by the given angles.
		return r3.Vector{X: 100, Y: 100 * math.Tan(h*math.Pi/180), Z: 100 * math.Tan(v*math.Pi/180)}
	}

	cases := []struct {
		name string
		h, v float64
		want bool
	}{
		{"dead centre", 0, 0, true},
		{"inside both limits", 40, 30, true},
		{"just inside horizontal limit", 52, 0, true},
		{"beyond horizontal limit", 55, 0, false},
		{"just inside vertical limit", 0, 36, true},
		{"beyond vertical limit", 0, 39, false},
		{"on screen sideways, off screen upward", 50, 45, false},
		{"48 deg is on screen sideways but not upward", 48, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := onScreen(eye, yaw, pitch, at(tc.h, tc.v)); got != tc.want {
				t.Fatalf("onScreen(h=%.0f°, v=%.0f°) = %v, want %v", tc.h, tc.v, got, tc.want)
			}
		})
	}
	// The asymmetry itself: 48° to the side is visible, 48° up is not.
	if !onScreen(eye, yaw, pitch, at(48, 0)) || onScreen(eye, yaw, pitch, at(0, 48)) {
		t.Fatal("expected 48° horizontal on screen and 48° vertical off it")
	}
}

func TestOnScreenRejectsTargetsBehindTheCamera(t *testing.T) {
	if onScreen(r3.Vector{}, 0, 0, r3.Vector{X: -100}) {
		t.Fatal("a target directly behind the player is not on screen")
	}
}

// Pitch rotates the frustum with the view rather than shrinking it: a target
// directly above is on screen once the player looks up at it.
func TestOnScreenFollowsPitch(t *testing.T) {
	target := r3.Vector{X: 100, Z: 100} // 45° above the horizon
	if onScreen(r3.Vector{}, 0, 0, target) {
		t.Fatal("45° above the horizon is outside the vertical field when looking level")
	}
	// ViewDirectionY is positive looking down, so looking up is negative.
	if !onScreen(r3.Vector{}, 0, -45, target) {
		t.Fatal("the same target should be centred once the player looks up at it")
	}
}
