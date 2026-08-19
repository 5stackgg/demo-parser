package parser

import (
	"math"
	"testing"

	"github.com/5stackgg/demo-parser/internal/geometry"
)

// The premise the whole one-way test rests on: the line between two eyes is the
// same line whichever end you start from, so any endpoint that only ever
// compares eye to eye can never find an asymmetry.
func TestEyeToEyeIsSymmetric(t *testing.T) {
	mesh := openSpaceMesh(t)
	at := Point{Z: 120}
	a, b := Point{Z: 64}, Point{X: -500, Z: 64}
	res, err := Sightlines(mesh, SightlineRequest{
		Map: "de_test", At: &at,
		Pairs: []SightlinePair{{From: a, To: b}, {From: b, To: a}},
	})
	if err != nil {
		t.Fatalf("sightlines: %v", err)
	}
	// Not bit-identical: the voxel walk enters the grid from a different
	// corner each way round, so the sum lands in a different order.
	if math.Abs(res.Results[0].Depth-res.Results[1].Depth) > 1e-9 {
		t.Fatalf("depth is direction-dependent: %v vs %v", res.Results[0].Depth, res.Results[1].Depth)
	}
	if res.Results[0].Blocked != res.Results[1].Blocked {
		t.Fatal("a single line cannot be blocked one way and not the other")
	}
}

// oneWayHeadInCloud is the classic: a cloud sitting high enough that a standing
// player's eyes are inside its lower half while their legs are below it. They
// see nothing; the player across the way sees their legs and shoots them. It is
// also the case crouching solves, which is what the stance sweep is for.
//
// Modelled here as a smoke resting 120 units up — on a box, a rail, a ledge —
// with player A standing directly under it and player B out in clear air.
func oneWayHeadInCloud(t *testing.T) (*geometry.Mesh, OneWayRequest) {
	t.Helper()
	mesh := openSpaceMesh(t)
	at := Point{Z: 120}
	return mesh, OneWayRequest{
		Map:   "de_test",
		At:    &at,
		Pairs: []SightlinePair{{From: Point{}, To: Point{X: -500}}},
	}
}

func TestOneWayFindsHeadInCloud(t *testing.T) {
	mesh, req := oneWayHeadInCloud(t)
	res, err := OneWay(mesh, req)
	if err != nil {
		t.Fatalf("oneway: %v", err)
	}
	got := res.Results[0]
	if !got.OneWay {
		t.Fatalf("expected an asymmetry: %+v", got)
	}
	if got.Favors != "b" {
		t.Fatalf("the player outside the cloud should be the one who can see, got %q", got.Favors)
	}
	if got.Cause != "smoke" {
		t.Fatalf("cause = %q, want smoke", got.Cause)
	}
	if got.Confidence == "none" || got.Best == nil {
		t.Fatalf("a one-way verdict needs a grade and a stance to stand in: %+v", got)
	}
	if len(res.Caveats) == 0 {
		t.Fatal("the caveats are part of the contract; they are always returned")
	}
	if len(got.Stances) != 4 {
		t.Fatalf("expected both stances on both sides, got %d combinations", len(got.Stances))
	}
}

// Crouching is the counter, and the model has to show it: the same pair is
// one-way while A stands and symmetric once A drops under the cloud.
func TestOneWayDependsOnStance(t *testing.T) {
	mesh, req := oneWayHeadInCloud(t)
	res, err := OneWay(mesh, req)
	if err != nil {
		t.Fatalf("oneway: %v", err)
	}
	byStance := map[[2]string]OneWayStance{}
	for _, st := range res.Results[0].Stances {
		byStance[[2]string{st.AStance, st.BStance}] = st
	}

	standing := byStance[[2]string{stanceStand, stanceStand}]
	if !standing.OneWay || standing.AToB.Visible || !standing.BToA.Visible {
		t.Fatalf("standing under the cloud should be blind while being seen: %+v", standing)
	}
	if standing.Margin <= 0 {
		t.Fatalf("a one-way with no margin is a coin flip: %+v", standing)
	}

	crouched := byStance[[2]string{stanceCrouch, stanceStand}]
	if crouched.OneWay {
		t.Fatalf("crouching under the cloud should even the fight up: %+v", crouched)
	}
	if !crouched.AToB.Visible {
		t.Fatalf("crouching should get A's eyes under the cloud: %+v", crouched.AToB)
	}
	if crouched.AToB.Depth >= standing.AToB.Depth {
		t.Fatalf("crouching should put less smoke on the line, got %.2f crouched vs %.2f standing",
			crouched.AToB.Depth, standing.AToB.Depth)
	}
}

// A pair with clear air between them is not a one-way, and must not be dressed
// up as one.
func TestOneWayReportsSymmetryHonestly(t *testing.T) {
	mesh := openSpaceMesh(t)
	res, err := OneWay(mesh, OneWayRequest{
		Map:   "de_test",
		Pairs: []SightlinePair{{From: Point{}, To: Point{X: -500}}},
	})
	if err != nil {
		t.Fatalf("oneway: %v", err)
	}
	got := res.Results[0]
	if got.OneWay || got.Favors != "" || got.Best != nil {
		t.Fatalf("open ground is not a one-way: %+v", got)
	}
	if got.Confidence != "none" {
		t.Fatalf("confidence = %q, want none", got.Confidence)
	}
	for _, st := range got.Stances {
		if !st.AToB.Visible || !st.BToA.Visible {
			t.Fatalf("both players should see each other in every stance: %+v", st)
		}
		if st.AToB.SamplesVisible != st.AToB.Samples {
			t.Fatalf("every body sample should be visible in the open: %+v", st.AToB)
		}
	}
}

// The map can produce the same asymmetry on its own — crouching to see under a
// gap the standing player cannot see back through. That is a property of the
// ledge, not of anyone's lineup, so it is labelled as such. It is also the case
// where the advantage belongs to whoever crouches rather than to either
// position, which the verdict has to say out loud.
func TestOneWayAttributesAGapToTheWorld(t *testing.T) {
	// A wall hanging from z=58 upward, i.e. a gap along the ground, with B
	// standing on a 34-unit step on the far side of it.
	mesh := meshFromBlob(t, quadTriBlob(-400, 400, 58, 400))
	res, err := OneWay(mesh, OneWayRequest{
		Map:   "de_test",
		Pairs: []SightlinePair{{From: Point{X: -100}, To: Point{X: 100, Z: 34}}},
	})
	if err != nil {
		t.Fatalf("oneway: %v", err)
	}
	got := res.Results[0]
	if !got.OneWay {
		t.Fatalf("crouching under the gap should beat standing over it: %+v", got)
	}
	if got.Cause != "world" {
		t.Fatalf("cause = %q, want world — there is no smoke in this scene", got.Cause)
	}
	if !got.Contested {
		t.Fatalf("whoever crouches wins here, so the verdict is contested: %+v", got)
	}
	if got.Best.Margin != 0 {
		t.Fatalf("a world-caused verdict has no depth margin to report: %+v", got.Best)
	}
	if !seeingView(*got.Best).Visible || seeingView(*got.Best).WorldBlocked {
		t.Fatalf("the favoured side sees under the gap: %+v", got.Best)
	}
	// Both crouched, the gap is mutual — the geometry is reciprocal, and the
	// model must not invent an advantage that stance alone removes.
	for _, st := range got.Stances {
		if st.AStance == stanceCrouch && st.BStance == stanceCrouch && st.OneWay {
			t.Fatalf("with both players crouched the gap works both ways: %+v", st)
		}
	}
}

// "eyes" positions are the same query with the feet worked back out, so the two
// forms have to agree — a caller getting this wrong would move every eye by 64
// units and never know.
func TestOneWayPositionsModes(t *testing.T) {
	mesh, req := oneWayHeadInCloud(t)
	feet, err := OneWay(mesh, req)
	if err != nil {
		t.Fatalf("oneway: %v", err)
	}
	asEyes := req
	asEyes.Positions = "eyes"
	asEyes.Pairs = []SightlinePair{{
		From: Point{Z: standEyeHeight},
		To:   Point{X: -500, Z: standEyeHeight},
	}}
	eyes, err := OneWay(mesh, asEyes)
	if err != nil {
		t.Fatalf("oneway: %v", err)
	}
	if feet.Results[0].Favors != eyes.Results[0].Favors {
		t.Fatalf("feet and eye positions disagree: %q vs %q",
			feet.Results[0].Favors, eyes.Results[0].Favors)
	}
	for i := range feet.Results[0].Stances {
		a, b := feet.Results[0].Stances[i], eyes.Results[0].Stances[i]
		if math.Abs(a.AToB.Depth-b.AToB.Depth) > 1e-9 {
			t.Fatalf("stance %d: depth %v vs %v", i, a.AToB.Depth, b.AToB.Depth)
		}
	}

	bad := req
	bad.Positions = "shoulders"
	if _, err := OneWay(mesh, bad); err == nil {
		t.Fatal("an unknown positions mode should be rejected rather than guessed")
	}
}

func TestOneWayEyeHeightsAreOverridable(t *testing.T) {
	mesh, req := oneWayHeadInCloud(t)
	// Put both "stances" at the same height: the stance sweep then has nothing
	// left to vary and the pairing must come out symmetric in the same way for
	// all four combinations.
	same := 64.0
	req.StandEyeHeight, req.CrouchEyeHeight = &same, &same
	res, err := OneWay(mesh, req)
	if err != nil {
		t.Fatalf("oneway: %v", err)
	}
	first := res.Results[0].Stances[0]
	for _, st := range res.Results[0].Stances[1:] {
		if st.AToB.Depth != first.AToB.Depth || st.BToA.Depth != first.BToA.Depth {
			t.Fatalf("with one eye height every stance is the same query: %+v vs %+v", st, first)
		}
	}
}

func TestOneWayRequestValidation(t *testing.T) {
	mesh := openSpaceMesh(t)
	if _, err := OneWay(nil, OneWayRequest{Map: "x", Pairs: []SightlinePair{{}}}); err != ErrNoMesh {
		t.Fatal("a missing mesh should be ErrNoMesh")
	}
	if _, err := OneWay(mesh, OneWayRequest{Map: "x"}); err == nil {
		t.Fatal("an empty pair list should be rejected")
	}
	if _, err := OneWay(mesh, OneWayRequest{
		Map:   "x",
		Pairs: []SightlinePair{{From: Point{X: math.NaN()}}},
	}); err == nil {
		t.Fatal("non-finite coordinates should be rejected")
	}
}

// A sliver of a shoulder is not a confident read, however far the depths are
// apart: it says more about where this model put its body samples than about
// the smoke.
func TestOneWayGradeCapsSliverVisibility(t *testing.T) {
	sliver := OneWayStance{
		Favors: "a",
		AToB:   OneWayView{Visible: true, SamplesVisible: 1, Samples: 7},
		Margin: 10,
	}
	if got := grade(sliver); got != "marginal" {
		t.Fatalf("grade = %q, want marginal for a single visible sample", got)
	}
	solid := sliver
	solid.AToB.SamplesVisible = 4
	if got := grade(solid); got != "strong" {
		t.Fatalf("grade = %q, want strong for a wide margin on a visible body", got)
	}
	narrow := solid
	narrow.Margin = 0.2
	if got := grade(narrow); got != "marginal" {
		t.Fatalf("grade = %q, want marginal for a margin inside the noise", got)
	}
}
