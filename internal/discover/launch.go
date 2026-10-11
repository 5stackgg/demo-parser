package discover

import (
	"math"

	"github.com/golang/geo/r3"
)

// The launch model: how an aim, a mouse button and a player's movement become
// the grenade's spawn point and velocity. It is the same function as the
// practice plugin's PracticeLaunchUtility, fitted to the same recorded seeds,
// and the two must agree: the plugin throws the seeds this package proposes,
// and reports the aim back to a player.
const (
	pitchRemapOffset    = -10.0
	fullSpeed           = 675.0
	forwardOffset       = 16.0
	weakDrop            = 12.0
	playerVelocityScale = 1.25
	minStrengthScale    = 0.3
)

// Strength is which mouse buttons release the grenade.
type Strength string

const (
	Full Strength = "Full"
	Half Strength = "Half"
	Drop Strength = "Drop"
)

// Raw is m_flThrowStrength for the release.
func (s Strength) Raw() float64 {
	switch s {
	case Full:
		return 1
	case Half:
		return 0.5
	default:
		return 0
	}
}

func (s Strength) valid() bool {
	return s == Full || s == Half || s == Drop
}

// remapPitch is the pitch the grenade actually leaves at for a view pitch:
// tipped ten degrees up at the horizon, squeezed by 80/90 above it and
// stretched by 100/90 below.
func remapPitch(pitch float64) float64 {
	if pitch < 0 {
		return pitchRemapOffset + pitch*(90+pitchRemapOffset)/90
	}
	return pitchRemapOffset + pitch*(90-pitchRemapOffset)/90
}

// forward is Source's view vector: yaw counter-clockwise from +X, pitch
// positive looking down.
func forward(pitch, yaw float64) r3.Vector {
	p := pitch * math.Pi / 180
	y := yaw * math.Pi / 180
	return r3.Vector{X: math.Cos(p) * math.Cos(y), Y: math.Cos(p) * math.Sin(y), Z: -math.Sin(p)}
}

// Launch is the seed a throw leaves with.
func Launch(eye r3.Vector, pitch, yaw float64, strength Strength, player r3.Vector) (position, velocity r3.Vector) {
	raw := strength.Raw()
	dir := forward(remapPitch(pitch), yaw)
	speed := fullSpeed * (minStrengthScale + (1-minStrengthScale)*raw)
	position = eye.Add(dir.Mul(forwardOffset)).Sub(r3.Vector{Z: weakDrop * (1 - raw)})
	velocity = dir.Mul(speed).Add(player.Mul(playerVelocityScale))
	return position, velocity
}

// Direction is which way the player is moving when they let go, relative to
// where they face. Left and right are strafes.
type Direction string

const (
	Still   Direction = ""
	Forward Direction = "forward"
	Back    Direction = "back"
	Left    Direction = "left"
	Right   Direction = "right"
)

// Gait is how fast.
type Gait string

const (
	Walk Gait = "walk"
	Run  Gait = "run"
)

// Technique is what the player does at release. Its numbers are the plugin's
// TechniqueProfile: standing and the jump throw bind are measured from recorded
// throws, a full-speed walk and run are not yet.
type Technique struct {
	Direction Direction `json:"direction"`
	Gait      Gait      `json:"gait"`
	JumpThrow bool      `json:"jump_throw"`
}

const (
	standingEye   = 63.97
	jumpThrowLift = 218.8
	jumpThrowEye  = 89.91
	runSpeed      = 245.0
	walkSpeed     = 127.4
)

func (t Technique) moving() bool {
	return t.Direction != Still
}

func (t Technique) valid() bool {
	switch t.Direction {
	case Still:
		return true
	case Forward, Back, Left, Right:
		return t.Gait == Walk || t.Gait == Run
	}
	return false
}

// Name is the lineup technique a throw like this saves as.
func (t Technique) Name() string {
	switch {
	case !t.moving() && t.JumpThrow:
		return "Jump"
	case !t.moving():
		return "Stationary"
	case t.Gait == Walk && t.JumpThrow:
		return "WalkJump"
	case t.Gait == Walk:
		return "Walking"
	case t.JumpThrow:
		return "RunJump"
	}
	return "Running"
}

// Measured is whether the technique's release has been measured from real
// throws rather than assumed.
func (t Technique) Measured() bool {
	return !t.moving()
}

func (t Technique) eye() float64 {
	if t.JumpThrow {
		return jumpThrowEye
	}
	return standingEye
}

// velocity is the player's own velocity at release, facing yaw.
func (t Technique) velocity(yaw float64) r3.Vector {
	var lift float64
	if t.JumpThrow {
		lift = jumpThrowLift
	}
	if !t.moving() {
		return r3.Vector{Z: lift}
	}
	speed := walkSpeed
	if t.Gait == Run {
		speed = runSpeed
	}
	ahead := forward(0, yaw)
	right := r3.Vector{X: ahead.Y, Y: -ahead.X}
	var along r3.Vector
	switch t.Direction {
	case Forward:
		along = ahead
	case Back:
		along = ahead.Mul(-1)
	case Right:
		along = right
	case Left:
		along = right.Mul(-1)
	}
	return along.Mul(speed).Add(r3.Vector{Z: lift})
}

// Seed is the launch for this technique from a stance.
func (t Technique) Seed(stance r3.Vector, pitch, yaw float64, strength Strength) (position, velocity r3.Vector) {
	eye := stance.Add(r3.Vector{Z: t.eye()})
	return Launch(eye, pitch, yaw, strength, t.velocity(yaw))
}
