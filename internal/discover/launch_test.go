package discover

import (
	"math"
	"testing"

	"github.com/golang/geo/r3"
)

func v(x, y, z float64) r3.Vector { return r3.Vector{X: x, Y: y, Z: z} }

func near(t *testing.T, what string, got, want r3.Vector, tolerance float64) {
	t.Helper()
	if d := got.Sub(want).Norm(); d > tolerance {
		t.Fatalf("%s: got %v, want %v (%.3f off, allowed %.3f)", what, got, want, d, tolerance)
	}
}

// Seeds the engine wrote for real throws on de_mirage, the same ones the
// practice plugin's launch model is held to: the two have to agree, because
// the plugin throws what this package proposes.
func TestLaunchReproducesRecordedSeeds(t *testing.T) {
	cases := []struct {
		name      string
		stance    r3.Vector
		technique Technique
		pitch     float64
		yaw       float64
		strength  Strength
		position  r3.Vector
		velocity  r3.Vector
	}{
		{
			"Top of Mid Smoke from T Spawn", v(1422.9536, 36.457386, -104-standingEye), Technique{},
			-42.33331, -160.5022, Full,
			v(1412.7892, 32.858425, -92.17914), v(-428.81012, -151.83116, 498.69247),
		},
		{
			"Jungle Smoke from Palace Alley", v(908.68005, -1036.0312, -251.96875), Technique{JumpThrow: true},
			-55.324947, -161.02596, Full,
			v(900.9274, -1038.6968, -148.32233), v(-327.06262, -112.45098, 853.15137),
		},
		{
			"Middle Smoke from Top of Mid, both buttons", v(343.30295, -621.6206, -162.3318), Technique{JumpThrow: true},
			-8.508511, -168.26045, Half,
			v(328.36786, -624.72424, -73.59377), v(-409.54797, -85.108154, 405.88336),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos, vel := tc.technique.Seed(tc.stance, tc.pitch, tc.yaw, tc.strength)
			near(t, "velocity", vel, tc.velocity, 0.5)
			near(t, "position", pos, tc.position, 0.15)
		})
	}
}

// Clear Smoke in Snipers Nest: a right click looking down, the other half of
// the pitch remap and the weakest throw.
func TestLaunchLookingDownWithARightClick(t *testing.T) {
	_, vel := Launch(v(0, 0, 0), 22.598167, 57.574562, Drop, v(0, 0, 221.9))
	near(t, "horizontal velocity", v(vel.X, vel.Y, 0), v(104.82733, 165.01947, 0), 0.5)
}

func TestTechniqueNamesAreTheLineupTechniques(t *testing.T) {
	cases := map[string]Technique{
		"Stationary": {},
		"Jump":       {JumpThrow: true},
		"Walking":    {Direction: Left, Gait: Walk},
		"Running":    {Direction: Forward, Gait: Run},
		"WalkJump":   {Direction: Back, Gait: Walk, JumpThrow: true},
		"RunJump":    {Direction: Right, Gait: Run, JumpThrow: true},
	}
	for want, technique := range cases {
		if got := technique.Name(); got != want {
			t.Fatalf("%+v is %s, want %s", technique, got, want)
		}
	}
}

// Left and right are strafes: the player faces the throw and moves across it.
func TestATechniqueMovesTheWayThePlayerPressed(t *testing.T) {
	cases := []struct {
		direction Direction
		yaw       float64
		want      r3.Vector
	}{
		{Forward, 0, v(runSpeed, 0, 0)},
		{Back, 0, v(-runSpeed, 0, 0)},
		{Right, 0, v(0, -runSpeed, 0)},
		{Left, 0, v(0, runSpeed, 0)},
		{Forward, 90, v(0, runSpeed, 0)},
	}
	for _, tc := range cases {
		got := Technique{Direction: tc.direction, Gait: Run}.velocity(tc.yaw)
		near(t, string(tc.direction), got, tc.want, 1e-6)
	}
	jump := Technique{JumpThrow: true}.velocity(30)
	if jump.X != 0 || jump.Y != 0 || jump.Z != jumpThrowLift {
		t.Fatalf("a jump throw rises straight up, got %v", jump)
	}
}

// Until a full-speed walk and run are recorded, a moving candidate rests on an
// assumed speed and has to say so.
func TestOnlyMeasuredTechniquesSayTheyAre(t *testing.T) {
	if !(Technique{}).Measured() || !(Technique{JumpThrow: true}).Measured() {
		t.Fatal("standing and the jump throw bind are measured")
	}
	if (Technique{Direction: Forward, Gait: Run}).Measured() {
		t.Fatal("a run is not measured yet")
	}
}

func TestRemapMeetsAtTheHorizon(t *testing.T) {
	if math.Abs(remapPitch(-1e-9)-remapPitch(1e-9)) > 1e-6 {
		t.Fatal("the two halves of the remap must meet at the horizon")
	}
	if math.Abs(remapPitch(-45)-(-50)) > 1e-9 || math.Abs(remapPitch(45)-40) > 1e-9 {
		t.Fatalf("remap(-45) = %v, remap(45) = %v", remapPitch(-45), remapPitch(45))
	}
}
