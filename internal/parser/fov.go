package parser

import (
	"math"

	"github.com/golang/geo/r3"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
)

// CS2 renders with Hor+ scaling off a 90° horizontal FOV at 4:3. That keeps the
// vertical field fixed at 73.74° on every aspect ratio while the horizontal
// field widens — 90° at 4:3, 106.26° at 16:9. Half-angles:
//
//	4:3   → 45.00° horizontal, 36.87° vertical
//	16:9  → 53.13° horizontal, 36.87° vertical
//
// A demo doesn't record the player's aspect ratio, so 16:9 is assumed. Testing
// the two axes separately is the point: one cone can't tell "far to the side
// but on screen" apart from "slightly above but off the top of it".
const (
	fovHalfHorizontalDeg = 53.13
	fovHalfVerticalDeg   = 36.87
)

// fovLosEveryTicks throttles the sightline raycasts below. A pair sitting on
// screen but behind a wall would otherwise cost a ray every frame for as long
// as the angle is held; once a pair is confirmed visible it is recorded and
// never probed again.
const fovLosEveryTicks = 4

// onScreen reports whether target falls inside the view frustum of a player at
// eye looking along (yaw, pitch) — i.e. whether it was rendered on their
// monitor at all.
func onScreen(eye r3.Vector, yaw, pitch float32, target r3.Vector) bool {
	fwd := viewVector(yaw, pitch)
	// Right and up complete the view basis. Z is world up and the view is never
	// rolled, so right is the horizontal perpendicular of forward and up
	// follows from the cross product.
	rl := math.Hypot(fwd.X, fwd.Y)
	if rl < 1e-9 {
		return false // straight up or down; no stable basis
	}
	right := r3.Vector{X: fwd.Y / rl, Y: -fwd.X / rl}
	up := r3.Vector{
		X: right.Y*fwd.Z - right.Z*fwd.Y,
		Y: right.Z*fwd.X - right.X*fwd.Z,
		Z: right.X*fwd.Y - right.Y*fwd.X,
	}

	d := r3.Vector{X: target.X - eye.X, Y: target.Y - eye.Y, Z: target.Z - eye.Z}
	fz := d.X*fwd.X + d.Y*fwd.Y + d.Z*fwd.Z
	if fz <= 0 {
		return false // behind the camera
	}
	dx := d.X*right.X + d.Y*right.Y + d.Z*right.Z
	dy := d.X*up.X + d.Y*up.Y + d.Z*up.Z

	const deg = 180 / math.Pi
	return math.Atan2(math.Abs(dx), fz)*deg <= fovHalfHorizontalDeg &&
		math.Atan2(math.Abs(dy), fz)*deg <= fovHalfVerticalDeg
}

// trackFOV records, for every enemy pair, the first tick the target was both
// rendered on the attacker's screen and actually visible through the world.
// That single instant anchors both reaction time (how long from it to the
// damage) and crosshair placement (how far off target the view was at it) —
// they are two readings of the same moment, so they share one anchor.
func (s *state) trackFOV() {
	if !s.matchStarted {
		return
	}
	tick := s.parser.GameState().IngameTick()

	type pinfo struct {
		sid        string
		eye        r3.Vector
		feet       r3.Vector
		yaw, pitch float32
		team       common.Team
		blind      float64
	}
	var infos []pinfo
	for _, p := range s.parser.GameState().Participants().Playing() {
		if p == nil || !p.IsAlive() {
			continue
		}
		sid := steamIDStr(p)
		if sid == "" {
			continue
		}
		eye, _ := p.PositionEyes()
		infos = append(infos, pinfo{
			sid: sid, eye: eye, feet: p.Position(),
			yaw: p.ViewDirectionX(), pitch: p.ViewDirectionY(), team: p.Team,
			blind: p.FlashDurationTimeRemaining().Seconds(),
		})
	}

	seen := map[[2]string]bool{}
	for _, a := range infos {
		// A player still blinded by a flash isn't seeing anyone appear, so no
		// anchor should be recorded against them.
		if a.blind > blindCutoffSecs {
			continue
		}
		for _, v := range infos {
			if a.sid == v.sid || a.team == v.team {
				continue
			}
			if !onScreen(a.eye, a.yaw, a.pitch, v.eye) {
				continue
			}
			key := [2]string{v.sid, a.sid}
			seen[key] = true
			if _, had := s.fovEntry[v.sid][a.sid]; had {
				continue
			}
			// Anchor on the first tick the target is both on screen and
			// actually visible. Recording a bare frustum entry would start the
			// clock while they were still behind a wall or a smoke.
			if tick-s.fovLosProbe[key] < fovLosEveryTicks {
				continue
			}
			s.fovLosProbe[key] = tick
			if !s.visibleAt(tick, a.eye, v.eye, v.feet) {
				continue
			}
			if s.fovEntry[v.sid] == nil {
				s.fovEntry[v.sid] = map[string]visEntry{}
			}
			s.fovEntry[v.sid][a.sid] = visEntry{
				tick: tick, yaw: a.yaw, pitch: a.pitch, eye: a.eye,
				target:     v.eye,
				targetFeet: v.feet,
				targetView: s.eyeAsRendered(v.sid, tick, v.eye),
			}
		}
	}

	// Falling edge: a pair that left the screen loses its buffered anchor, so
	// the next appearance starts a fresh one.
	for vsid, inner := range s.fovEntry {
		for asid := range inner {
			if !seen[[2]string{vsid, asid}] {
				delete(inner, asid)
			}
		}
	}
	for key := range s.fovLosProbe {
		if !seen[key] {
			delete(s.fovLosProbe, key)
		}
	}
}
