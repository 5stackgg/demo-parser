package simulate

import (
	"math"
	"testing"

	"github.com/golang/geo/r3"
)

// A throw across open ground: leaves the hand at eye height, arcs out, bounces
// a few times and rolls to a stop.
var openThrow = Seed{
	Type:     Smoke,
	Position: r3.Vector{X: 0, Y: 0, Z: 64},
	Velocity: r3.Vector{X: 500, Y: 0, Z: 200},
}

func TestFlightIsDeterministic(t *testing.T) {
	mesh, _ := synthRevision(t, floorQuad(0, 0, 0, 2000))
	first, err := SimulateForComparison(mesh, openThrow, DefaultConstants())
	if err != nil {
		t.Fatalf("simulate: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := SimulateForComparison(mesh, openThrow, DefaultConstants())
		if err != nil {
			t.Fatalf("simulate %d: %v", i, err)
		}
		if again != first {
			t.Fatalf("run %d differs:\n first %+v\n again %+v", i, first, again)
		}
	}
}

// The same throw against two meshes built independently from identical bytes
// must agree to the bit. Everything downstream assumes a difference in the
// output means a difference in the mesh, so a simulator that wobbled between
// two builds of the same geometry would report drift that is not there.
func TestIdenticalMeshesGiveIdenticalFlights(t *testing.T) {
	tris := floorQuad(0, 0, 0, 2000)
	a, _ := synthRevision(t, tris)
	b, _ := synthRevision(t, tris)
	if a == b {
		t.Fatal("test setup: wanted two separately built meshes")
	}
	left, err := SimulateForComparison(a, openThrow, DefaultConstants())
	if err != nil {
		t.Fatalf("simulate a: %v", err)
	}
	right, err := SimulateForComparison(b, openThrow, DefaultConstants())
	if err != nil {
		t.Fatalf("simulate b: %v", err)
	}
	if left != right {
		t.Fatalf("identical geometry gave different flights:\n a %+v\n b %+v", left, right)
	}
}

func TestSmokeComesToRestOnTheFloor(t *testing.T) {
	c := DefaultConstants()
	mesh, _ := synthRevision(t, floorQuad(0, 0, 0, 2000))
	out, err := SimulateForComparison(mesh, openThrow, c)
	if err != nil {
		t.Fatalf("simulate: %v", err)
	}
	if !out.Resolved || out.Stop != StopRest {
		t.Fatalf("a smoke on open ground should come to rest, got %+v", out)
	}
	// The flight is integrated as a point held a radius off each surface, and a
	// glancing final contact leaves it lower than a head-on one — so the resting
	// height is somewhere in [0, radius], never inside the floor.
	if out.ComparisonPoint.Z < 0 || out.ComparisonPoint.Z > c.Radius {
		t.Fatalf("rest height %v is not within a radius above the floor", out.ComparisonPoint.Z)
	}
	if out.ComparisonPoint.X < 200 {
		t.Fatalf("a 500 u/s throw should carry further than %v units", out.ComparisonPoint.X)
	}
	if out.Bounces == 0 {
		t.Fatal("expected the throw to bounce at least once")
	}
}

func TestFuseTypesStopOnTheirFuse(t *testing.T) {
	c := DefaultConstants()
	mesh, _ := synthRevision(t, floorQuad(0, 0, 0, 2000))
	for _, tc := range []struct {
		nade NadeType
		fuse float64
	}{
		{HE, c.HEFuseSeconds},
		{Flash, c.FlashFuseSeconds},
	} {
		seed := openThrow
		seed.Type = tc.nade
		out, err := SimulateForComparison(mesh, seed, c)
		if err != nil {
			t.Fatalf("%s: %v", tc.nade, err)
		}
		if !out.Resolved || out.Stop != StopFuse {
			t.Fatalf("%s should detonate on its fuse, got %+v", tc.nade, out)
		}
		if math.Abs(out.FlightSeconds-tc.fuse) > c.TimeStep {
			t.Fatalf("%s detonated at %vs, want %vs", tc.nade, out.FlightSeconds, tc.fuse)
		}
	}
}

// A molotov ignites on the ground, not on a wall — otherwise every throw that
// clipped a doorframe would resolve in mid-air, and a lineup would look broken
// the moment a wall moved a unit.
func TestMolotovIgnitesOnGroundNotWall(t *testing.T) {
	// A wall at x = 300 to bounce off, with the floor a long way below.
	tris := append(
		floorQuad(0, 0, -400, 2000),
		quad(
			r3.Vector{X: 300, Y: -400, Z: -400},
			r3.Vector{X: 300, Y: 400, Z: -400},
			r3.Vector{X: 300, Y: 400, Z: 400},
			r3.Vector{X: 300, Y: -400, Z: 400},
		)...,
	)
	mesh, _ := synthRevision(t, tris)
	seed := Seed{Type: Molotov, Position: r3.Vector{X: 0, Y: 0, Z: 0}, Velocity: r3.Vector{X: 600}}
	out, err := SimulateForComparison(mesh, seed, DefaultConstants())
	if err != nil {
		t.Fatalf("simulate: %v", err)
	}
	if !out.Resolved || out.Stop != StopIgnite {
		t.Fatalf("a molotov should ignite on the floor, got %+v", out)
	}
	if out.ComparisonPoint.Z > -390 {
		t.Fatalf("ignited at z=%v: it should have fallen to the floor, not lit on the wall", out.ComparisonPoint.Z)
	}
	if out.Bounces == 0 {
		t.Fatal("expected the wall bounce to be counted")
	}
}

// A throw that starts inside geometry does not resolve. Against a new mesh this
// is a map update having built something where the player used to stand.
func TestSealedStartDoesNotResolve(t *testing.T) {
	tris := append(
		floorQuad(0, 0, 0, 2000),
		box(r3.Vector{X: -4, Y: -4, Z: 60}, r3.Vector{X: 4, Y: 4, Z: 68})...,
	)
	mesh, _ := synthRevision(t, tris)
	out, err := SimulateForComparison(mesh, openThrow, DefaultConstants())
	if err != nil {
		t.Fatalf("simulate: %v", err)
	}
	if out.Resolved || out.Stop != StopStartSealed {
		t.Fatalf("a throw from inside a solid should not resolve, got %+v", out)
	}
}

// A flight that leaves the map is unresolved, not "landed at the bottom of the
// world". Falling out is exactly what a lineup does when the floor under it is
// removed.
func TestFallingOutOfTheWorldDoesNotResolve(t *testing.T) {
	mesh, _ := synthRevision(t, floorQuad(0, 0, 0, 200))
	seed := Seed{Type: Smoke, Position: r3.Vector{X: 0, Y: 0, Z: 64}, Velocity: r3.Vector{X: 900, Z: 100}}
	out, err := SimulateForComparison(mesh, seed, DefaultConstants())
	if err != nil {
		t.Fatalf("simulate: %v", err)
	}
	if out.Resolved || out.Stop != StopOutOfWorld {
		t.Fatalf("a throw off the edge of the geometry should not resolve, got %+v", out)
	}
}

// The enclosure probe is what turns "resolved" into "resolved somewhere a
// grenade can actually be".
func TestEnclosedDetectsBeingInsideASolid(t *testing.T) {
	mesh, _ := synthRevision(t, box(r3.Vector{X: -10, Y: -10, Z: -10}, r3.Vector{X: 10, Y: 10, Z: 10}))
	if !enclosed(mesh, r3.Vector{}, 16) {
		t.Fatal("a point in the middle of a small sealed box should read as enclosed")
	}
	if enclosed(mesh, r3.Vector{}, 6) {
		t.Fatal("a probe shorter than the walls are away should not read as enclosed")
	}
	open, _ := synthRevision(t, floorQuad(0, 0, 0, 2000))
	if enclosed(open, r3.Vector{Z: 4}, 64) {
		t.Fatal("a point standing on open ground is not enclosed")
	}
}

func TestSimulateRejectsWhatItCannotRun(t *testing.T) {
	mesh, _ := synthRevision(t, floorQuad(0, 0, 0, 2000))
	if _, err := SimulateForComparison(nil, openThrow, DefaultConstants()); err == nil {
		t.Error("a nil mesh should be an error, not a flight through empty space")
	}
	stopped := openThrow
	stopped.Velocity = r3.Vector{}
	if _, err := SimulateForComparison(mesh, stopped, DefaultConstants()); err == nil {
		t.Error("a zero velocity should be an error")
	}
	nan := openThrow
	nan.Velocity = r3.Vector{X: math.NaN()}
	if _, err := SimulateForComparison(mesh, nan, DefaultConstants()); err == nil {
		t.Error("a non-finite velocity should be an error")
	}
	untyped := openThrow
	untyped.Type = ""
	if _, err := SimulateForComparison(mesh, untyped, DefaultConstants()); err == nil {
		t.Error("a seed with no nade type should be an error")
	}
	bad := DefaultConstants()
	bad.TimeStep = 0
	if _, err := SimulateForComparison(mesh, openThrow, bad); err == nil {
		t.Error("a zero timestep should be an error")
	}
}

func TestConstantsValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Constants)
	}{
		{"negative timestep", func(c *Constants) { c.TimeStep = -1 }},
		{"timestep over a second", func(c *Constants) { c.TimeStep = 2 }},
		{"too many steps", func(c *Constants) { c.TimeStep, c.MaxFlightSeconds = 1e-9, 100 }},
		{"negative gravity", func(c *Constants) { c.Gravity = -1 }},
		{"restitution over one", func(c *Constants) { c.Restitution = 1.5 }},
		{"friction over one", func(c *Constants) { c.Friction = 1.5 }},
		{"zero rest steps", func(c *Constants) { c.RestSteps = 0 }},
		{"negative probe", func(c *Constants) { c.EnclosureProbe = -1 }},
	} {
		c := DefaultConstants()
		tc.mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s should not validate", tc.name)
		}
	}
	if err := DefaultConstants().Validate(); err != nil {
		t.Fatalf("the shipped constants must validate: %v", err)
	}
}

func TestConstantOverridesLeaveTheRestAlone(t *testing.T) {
	g := 111.0
	steps := 9
	got := (&ConstantOverrides{Gravity: &g, RestSteps: &steps}).Apply(DefaultConstants())
	if got.Gravity != g || got.RestSteps != steps {
		t.Fatalf("overrides not applied: %+v", got)
	}
	want := DefaultConstants()
	want.Gravity, want.RestSteps = g, steps
	if got != want {
		t.Fatalf("an override changed something it was not asked to:\n got %+v\nwant %+v", got, want)
	}
	if (*ConstantOverrides)(nil).Apply(DefaultConstants()) != DefaultConstants() {
		t.Fatal("nil overrides should leave the defaults alone")
	}
}

func TestParseNadeType(t *testing.T) {
	for in, want := range map[string]NadeType{
		"Smoke":       Smoke,
		"smoke":       Smoke,
		"HE":          HE,
		"hegrenade":   HE,
		"Flash":       Flash,
		"flashbang":   Flash,
		"Molotov":     Molotov,
		"incendiary":  Molotov,
		"Decoy":       Decoy,
		"  smoke    ": Smoke,
	} {
		got, ok := ParseNadeType(in)
		if !ok || got != want {
			t.Errorf("ParseNadeType(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if _, ok := ParseNadeType("banana"); ok {
		t.Error("an unknown grenade should not parse")
	}
}
