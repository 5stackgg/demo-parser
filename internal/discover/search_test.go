package discover

import (
	"context"
	"math"
	"testing"

	"github.com/golang/geo/r3"
)

// A floor 3000 across with a 16-unit step on one side of x = 0.
func steppedFloor() []tri {
	out := rect(-1500, 0, -1500, 1500, 0)
	out = append(out, rect(0, 1500, -1500, 1500, 16)...)
	out = append(out, wallX(0, -1500, 1500, 0, 16)...)
	return out
}

// A player is a box, and on a stair edge it stands on the step: one ray
// down the middle would drop it to the floor below and put the eye 16 units
// under where a person standing there sees from.
func TestAStanceOnAnEdgeStandsOnTheStep(t *testing.T) {
	world := synthWorld(t, steppedFloor())

	feet, ok := floorAt(world.Hull(), -4, 0, -64, 64)
	if !ok || feet.Z != 16 {
		t.Fatalf("a player straddling the edge stands at %v, want z 16", feet)
	}
	feet, ok = floorAt(world.Hull(), -200, 0, -64, 64)
	if !ok || feet.Z != 0 {
		t.Fatalf("clear of the step a player stands on the floor, got %v", feet)
	}
}

// The area's Z range picks the floor: a stance on the floor below a ledge
// is not a stance on the ledge.
func TestTheAreaPicksTheFloor(t *testing.T) {
	tris := rect(-500, 500, -500, 500, 0)
	tris = append(tris, rect(-500, 500, -500, 500, 300)...)
	world := synthWorld(t, tris)

	if _, ok := floorAt(world.Hull(), 0, 0, -32, 32); !ok {
		t.Fatal("the ground floor is found under a ceiling above the range")
	}
	feet, ok := floorAt(world.Hull(), 0, 0, 268, 332)
	if !ok || feet.Z != 300 {
		t.Fatalf("the upper floor is found when the range is up there, got %v", feet)
	}
}

func TestAStanceNeedsRoomForAPlayer(t *testing.T) {
	tris := rect(-500, 500, -500, 500, 0)
	tris = append(tris, rect(100, 200, -100, 100, 50)...)
	tris = append(tris, wallX(-300, -500, 500, 0, 200)...)
	world := synthWorld(t, tris)
	hull := world.Hull()

	if fits(hull, v(150, 0, 0)) {
		t.Fatal("a 50-unit ceiling has no room for a 72-unit player")
	}
	if fits(hull, v(-292, 0, 0)) {
		t.Fatal("a player cannot stand half inside a wall")
	}
	if !fits(hull, v(0, 0, 0)) {
		t.Fatal("open floor is somewhere to stand")
	}
}

// A strafe pushes the grenade sideways, so the yaw has to lead the target
// for the grenade itself to head at it.
func TestAStrafeLeadsTheTarget(t *testing.T) {
	target := v(1000, 0, 0)
	for _, technique := range []Technique{{}, {Direction: Left, Gait: Run}, {Direction: Right, Gait: Walk, JumpThrow: true}} {
		spec := throwSpec{stand: v(0, 0, 0), technique: technique, strength: Full}
		yaw := yawFor(spec, -30, target)
		_, vel := technique.Seed(spec.stand, -30, yaw, Full)
		if heading := math.Atan2(vel.Y, vel.X) * 180 / math.Pi; math.Abs(heading) > 0.01 {
			t.Fatalf("%s: grenade heads %.3f degrees off the target", technique.Name(), heading)
		}
	}
}

// On open ground every candidate is a throw the model itself lands on the
// target, and the seed it carries is the launch for its own aim.
func TestSearchFindsThrowsThatLandOnTheTarget(t *testing.T) {
	world := synthWorld(t, rect(-3000, 3000, -3000, 3000, 0))
	req := Request{
		Map:        "de_test",
		Target:     v(900, 300, 0),
		Radius:     24,
		Area:       Area{Min: v(-64, -64, -32), Max: v(64, 64, 32)},
		Techniques: []Technique{{}, {JumpThrow: true}},
		Strengths:  []Strength{Full, Half},
		BudgetMs:   20000,
	}
	res, err := Search(context.Background(), world, req, 4)
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated || len(res.Candidates) == 0 {
		t.Fatalf("found %d candidates (truncated %v) from %d stands", len(res.Candidates), res.Truncated, res.Stands)
	}
	m := DefaultModel()
	for _, c := range res.Candidates {
		if c.Distance > req.Radius {
			t.Fatalf("%+v lands %.1f out", c, c.Distance)
		}
		pos, vel := c.Technique.Seed(c.Stand, c.Pitch, c.Yaw, c.Strength)
		near(t, "seed position", c.Position, pos, 0.01)
		near(t, "seed velocity", c.Velocity, vel, 0.01)
		f := m.Fly(world, c.Position, c.Velocity, false)
		if !f.Settled || f.Rest.Sub(req.Target).Norm() > req.Radius+0.5 {
			t.Fatalf("re-flying %s %s lands at %v", c.Name, c.Strength, f.Rest)
		}
	}
}

func TestRankPutsTheEasiestFirstAndDropsNearCopies(t *testing.T) {
	base := Candidate{Stand: v(0, 0, 0), Technique: Technique{}, Strength: Full, Pitch: -40, Yaw: 10, Distance: 5, Spread: 30}
	copyOf := base
	copyOf.Stand = v(20, 0, 0)
	copyOf.Pitch = -40.5
	copyOf.Spread = 40
	easier := base
	easier.Pitch = -60
	easier.Spread = 10
	other := base
	other.Technique = Technique{JumpThrow: true}
	other.Spread = 30

	out := rank([]Candidate{base, copyOf, easier, other}, 10)
	if len(out) != 3 {
		t.Fatalf("kept %d, want 3 (the near-copy dropped): %+v", len(out), out)
	}
	if out[0].Spread != 10 {
		t.Fatalf("the most forgiving throw comes first, got %+v", out[0])
	}
	if len(rank([]Candidate{base, easier, other}, 2)) != 2 {
		t.Fatal("the limit is respected")
	}
}

func TestARequestWithoutAnswersIsRefused(t *testing.T) {
	good := func() Request {
		return Request{Map: "de_test", Radius: 40, Area: Area{Min: v(0, 0, 0), Max: v(10, 10, 10)}, Techniques: []Technique{{}}}
	}
	cases := map[string]func(*Request){
		"no radius":            func(r *Request) { r.Radius = 0 },
		"inside-out area":      func(r *Request) { r.Area.Max = v(-1, 0, 0) },
		"no technique":         func(r *Request) { r.Techniques = nil },
		"a move with no speed": func(r *Request) { r.Techniques = []Technique{{Direction: Forward}} },
		"a made-up button":     func(r *Request) { r.Strengths = []Strength{"Middle"} },
	}
	cases["a target off the map"] = func(r *Request) { r.Target = v(1e30, 0, 0) }
	cases["a NaN in the area"] = func(r *Request) { r.Area.Min.X = math.NaN() }
	cases["an infinite area"] = func(r *Request) { r.Area.Max.Y = math.Inf(1) }
	cases["an area wider than any map"] = func(r *Request) { r.Area.Max.X = 5000 }
	cases["a NaN spacing"] = func(r *Request) { r.Spacing = math.NaN() }
	cases["a NaN radius"] = func(r *Request) { r.Radius = math.NaN() }
	for name, mutate := range cases {
		r := good()
		mutate(&r)
		if err := r.Validate(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	r := good()
	if err := r.Validate(); err != nil || r.Spacing != DefaultSpacing || r.Limit != DefaultLimit || len(r.Strengths) != 1 {
		t.Fatalf("defaults: %+v, %v", r, err)
	}
}

// The work a search does is stands x techniques x strengths, so a request
// cannot buy more of it by repeating itself.
func TestRepeatsInARequestAreSearchedOnce(t *testing.T) {
	r := Request{
		Map:        "de_test",
		Radius:     40,
		Area:       Area{Min: v(0, 0, 0), Max: v(10, 10, 10)},
		Techniques: make([]Technique, 1000),
		Strengths:  make([]Strength, 0, 5000),
	}
	for i := 0; i < 5000; i++ {
		r.Strengths = append(r.Strengths, Full)
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(r.Techniques) != 1 || len(r.Strengths) != 1 {
		t.Fatalf("%d techniques and %d strengths after validation", len(r.Techniques), len(r.Strengths))
	}
}

// Far from the origin a float stepped by the spacing stops moving; the stands
// are counted instead, so this returns.
func TestStandsAtTheEdgeOfTheWorldTerminate(t *testing.T) {
	world := synthWorld(t, rect(-3000, 3000, -3000, 3000, 0))
	if got := Stands(world, Area{Min: v(1e300, 1e300, 0), Max: v(1e300, 1e300, 10)}, 32); got != nil {
		t.Fatalf("an area off the map has no stands, got %d", len(got))
	}
}

// What runs out of time says so, and returns what it had.
func TestABudgetThatRunsOutIsReported(t *testing.T) {
	world := synthWorld(t, rect(-3000, 3000, -3000, 3000, 0))
	req := Request{
		Map:        "de_test",
		Target:     v(900, 300, 0),
		Radius:     24,
		Area:       Area{Min: v(-600, -600, -32), Max: v(600, 600, 32)},
		Techniques: []Technique{{}, {JumpThrow: true}},
		Strengths:  []Strength{Full, Half, Drop},
		BudgetMs:   1,
	}
	res, err := Search(context.Background(), world, req, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatal("a 1ms budget over hundreds of stances must report it ran out")
	}
	if res.Candidates == nil {
		t.Fatal("candidates is an empty list, never null")
	}
}

// Every stance comes back inside the area, a circle trims the box's corners,
// and the count stays within the limit.
func TestStandsStayInTheArea(t *testing.T) {
	world := synthWorld(t, rect(-3000, 3000, -3000, 3000, 0))
	stands := Stands(world, Area{Min: v(-128, -128, -32), Max: v(128, 128, 32), Radius: 128}, 32)
	if len(stands) == 0 || len(stands) > MaxStands {
		t.Fatalf("%d stands", len(stands))
	}
	for _, s := range stands {
		if math.Hypot(s.X, s.Y) > 128+1e-6 {
			t.Fatalf("%v is outside the circle", s)
		}
	}
	many := Stands(world, Area{Min: v(-2900, -2900, -32), Max: v(2900, 2900, 32)}, 8)
	if len(many) > MaxStands {
		t.Fatalf("%d stands, more than %d", len(many), MaxStands)
	}
	_ = r3.Vector{}
}
