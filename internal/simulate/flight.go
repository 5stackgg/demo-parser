// Package simulate is a deterministic grenade flight model whose output is
// meaningful ONLY as a differential.
//
// # What this is for
//
// When Valve ships a map update we need to know which stored lineups it broke.
// The question is not "where does this nade land" — it is "does this nade land
// somewhere different than it used to". Those are very different problems. The
// second one is tractable without an accurate physics model, because the same
// deterministic simulator runs against the old collision mesh and the new one,
// and every constant error in the model appears identically on both sides and
// cancels. What survives the subtraction is the mesh change, which is the only
// thing we were asking about.
//
// # What this is NOT
//
// This is not CS2's grenade physics. The constants below were picked to be
// plausible and self-consistent, not measured against the game. An absolute
// landing point out of this package is wrong by an unknown amount, and showing
// one to a player as "where your nade lands" would be a confident lie. That is
// why the flight function is SimulateForComparison and the position it returns
// is ComparisonPoint: there is no way to spell a use of this package that reads
// as a prediction. If you want a real landing point, throw the nade on a real
// server and record it — the lineup library already stores those triples.
//
// # Determinism
//
// Everything here is fixed-timestep float64 arithmetic in a fixed order. There
// is no randomness, no map iteration, no time or goroutine dependence, and no
// dependence on how the mesh was loaded beyond its triangles. Identical inputs
// give byte-identical outputs, in this process and the next one. The whole
// method rests on that: if the simulator were noisy, every lineup would look
// like it had drifted.
package simulate

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/5stackgg/demo-parser/internal/geometry"
	"github.com/golang/geo/r3"
)

// NadeType is which grenade is being thrown. The spellings match the parser's
// grenadeTypeCode, so a lineup mined from a demo can be fed straight in.
type NadeType string

const (
	Smoke   NadeType = "Smoke"
	HE      NadeType = "HE"
	Flash   NadeType = "Flash"
	Molotov NadeType = "Molotov"
	Decoy   NadeType = "Decoy"
)

// ParseNadeType accepts the parser's codes plus the spellings the game and the
// API use for the same thing.
func ParseNadeType(s string) (NadeType, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "smoke", "smokegrenade", "smoke_grenade":
		return Smoke, true
	case "he", "hegrenade", "he_grenade", "frag":
		return HE, true
	case "flash", "flashbang":
		return Flash, true
	case "molotov", "incendiary", "incgrenade", "firebomb":
		return Molotov, true
	case "decoy":
		return Decoy, true
	}
	return "", false
}

// Constants are the model's physics knobs.
//
// THEY ARE APPROXIMATIONS, NOT MEASUREMENTS. Each is a plausible number chosen
// so the model behaves like a grenade and behaves the same way every time; none
// was fitted to recorded CS2 throws. That is sound for the differential — both
// sides of a comparison carry the same error — and unsound for anything else.
//
// They are a value rather than package constants precisely so they can be
// calibrated later: 5stack now records exact initial_position, initial_velocity
// and detonation triples from real servers, so a fit against real throws is a
// realistic follow-up. When that happens, only DefaultConstants changes.
type Constants struct {
	// TimeStep is the fixed integration step, in seconds. 1/128 is a CS2
	// server tick at the higher of the two common rates; a grenade's whole
	// flight is then a thousand-odd steps, which is cheap and fine-grained
	// enough that a bounce lands within a couple of units of the surface.
	TimeStep float64 `json:"time_step"`
	// Gravity is the downward acceleration, in source units/s². Source
	// projectiles run at a fraction of world gravity (sv_gravity 800 with a
	// 0.4 projectile scale is where 320 comes from), which is also what makes
	// a max-range throw carry the ~1700 units it does rather than ~700.
	Gravity float64 `json:"gravity"`
	// Radius is the grenade's collision radius, in source units. The flight is
	// integrated as a point and backed off from each surface by this, which is
	// exact for a head-on impact and slightly early for a glancing one.
	Radius float64 `json:"radius"`
	// Restitution is the fraction of the normal-direction speed kept through a
	// real bounce, and Friction the fraction of the tangential speed lost in
	// the same bounce. A grenade that hits a wall hard comes off it at a bit
	// under half speed and noticeably deflected.
	Restitution float64 `json:"restitution"`
	Friction    float64 `json:"friction"`
	// ContactNormalSpeed separates a bounce from a resting contact. Below it
	// the grenade is settling onto the surface rather than hitting it: the
	// normal component is simply removed instead of being reflected, which is
	// what keeps a grenade on a slope rolling down it instead of chattering
	// against it and freezing in place.
	ContactNormalSpeed float64 `json:"contact_normal_speed"`
	// RollingFriction is the tangential decay while in resting contact, per
	// second (not per bounce). Low, because grenades roll.
	RollingFriction float64 `json:"rolling_friction"`
	// FloorNormalZ is how "up" a surface must face to count as ground: a
	// grenade rests on a floor and bounces off a wall, and a molotov ignites
	// on the first and not the second.
	FloorNormalZ float64 `json:"floor_normal_z"`
	// RestSpeed and RestSteps are the sleep condition: this slow, in contact
	// with the ground, for this many consecutive steps. The step count exists
	// so one grazing touch on the way past is not read as having landed.
	RestSpeed float64 `json:"rest_speed"`
	RestSteps int     `json:"rest_steps"`
	// MaxFlightSeconds bounds the integration. A high lob that bounces a few
	// times and then rolls takes eight or nine seconds to settle in this model,
	// so the cap has to sit well clear of that; a flight still moving at it is
	// falling out of the map or wedged somewhere the model cannot settle, and
	// is reported unresolved rather than guessed at.
	MaxFlightSeconds float64 `json:"max_flight_seconds"`
	// HEFuseSeconds and FlashFuseSeconds are how long after leaving the hand
	// those two detonate — in the air, on the ground, wherever they are.
	HEFuseSeconds    float64 `json:"he_fuse_seconds"`
	FlashFuseSeconds float64 `json:"flash_fuse_seconds"`
	// MolotovArmSeconds is the delay before a molotov will ignite on ground
	// contact; before it, an early clip of the thrower's own feet does not
	// count.
	MolotovArmSeconds float64 `json:"molotov_arm_seconds"`
	// EnclosureProbe is how far the six axis rays are cast when deciding a
	// resolved point is inside geometry rather than in the world. A pocket
	// smaller than this on every axis is not somewhere a grenade can be.
	EnclosureProbe float64 `json:"enclosure_probe"`
	// WorldMargin is how far outside the mesh's own bounding box a flight may
	// stray before it is called out of the world.
	WorldMargin float64 `json:"world_margin"`
}

// DefaultConstants is the shipped model. See the Constants doc: approximations
// chosen for self-consistency, not measurements.
func DefaultConstants() Constants {
	return Constants{
		TimeStep:           1.0 / 128.0,
		Gravity:            320.0,
		Radius:             2.0,
		Restitution:        0.45,
		Friction:           0.30,
		ContactNormalSpeed: 20.0,
		RollingFriction:    3.0,
		FloorNormalZ:       0.7,
		RestSpeed:          20.0,
		RestSteps:          4,
		MaxFlightSeconds:   15.0,
		HEFuseSeconds:      1.5,
		FlashFuseSeconds:   1.5,
		MolotovArmSeconds:  0.1,
		EnclosureProbe:     6.0,
		WorldMargin:        1024.0,
	}
}

// ConstantOverrides is a partial Constants, as a request may carry it. Nil
// fields keep the default, so a caller can move one knob without restating the
// model — and so adding a knob does not break existing callers.
type ConstantOverrides struct {
	TimeStep           *float64 `json:"time_step,omitempty"`
	Gravity            *float64 `json:"gravity,omitempty"`
	Radius             *float64 `json:"radius,omitempty"`
	Restitution        *float64 `json:"restitution,omitempty"`
	Friction           *float64 `json:"friction,omitempty"`
	ContactNormalSpeed *float64 `json:"contact_normal_speed,omitempty"`
	RollingFriction    *float64 `json:"rolling_friction,omitempty"`
	FloorNormalZ       *float64 `json:"floor_normal_z,omitempty"`
	RestSpeed          *float64 `json:"rest_speed,omitempty"`
	RestSteps          *int     `json:"rest_steps,omitempty"`
	MaxFlightSeconds   *float64 `json:"max_flight_seconds,omitempty"`
	HEFuseSeconds      *float64 `json:"he_fuse_seconds,omitempty"`
	FlashFuseSeconds   *float64 `json:"flash_fuse_seconds,omitempty"`
	MolotovArmSeconds  *float64 `json:"molotov_arm_seconds,omitempty"`
	EnclosureProbe     *float64 `json:"enclosure_probe,omitempty"`
	WorldMargin        *float64 `json:"world_margin,omitempty"`
}

// Apply lays the overrides over a base set.
func (o *ConstantOverrides) Apply(base Constants) Constants {
	if o == nil {
		return base
	}
	set := func(dst *float64, src *float64) {
		if src != nil {
			*dst = *src
		}
	}
	set(&base.TimeStep, o.TimeStep)
	set(&base.Gravity, o.Gravity)
	set(&base.Radius, o.Radius)
	set(&base.Restitution, o.Restitution)
	set(&base.Friction, o.Friction)
	set(&base.ContactNormalSpeed, o.ContactNormalSpeed)
	set(&base.RollingFriction, o.RollingFriction)
	set(&base.FloorNormalZ, o.FloorNormalZ)
	set(&base.RestSpeed, o.RestSpeed)
	set(&base.MaxFlightSeconds, o.MaxFlightSeconds)
	set(&base.HEFuseSeconds, o.HEFuseSeconds)
	set(&base.FlashFuseSeconds, o.FlashFuseSeconds)
	set(&base.MolotovArmSeconds, o.MolotovArmSeconds)
	set(&base.EnclosureProbe, o.EnclosureProbe)
	set(&base.WorldMargin, o.WorldMargin)
	if o.RestSteps != nil {
		base.RestSteps = *o.RestSteps
	}
	return base
}

// maxContactsPerStep bounds how many surfaces one step may resolve against. A
// grenade in a tight corner can legitimately touch two or three; past that it
// is wedged, and the remaining motion for the step is dropped rather than
// looped on.
const maxContactsPerStep = 4

// maxSimulationSteps is a hard ceiling on the integration independent of
// MaxFlightSeconds and TimeStep, so a request cannot ask for a billion steps by
// naming a tiny timestep.
const maxSimulationSteps = 1 << 16

// Validate rejects a constant set the integrator cannot run. It is a separate
// method so a caller supplying overrides gets one clear error rather than a
// simulation that quietly does nothing.
func (c Constants) Validate() error {
	switch {
	case !(c.TimeStep > 0) || c.TimeStep > 1:
		return errors.New("time_step must be > 0 and <= 1 second")
	case !(c.MaxFlightSeconds > 0) || c.MaxFlightSeconds > 120:
		return errors.New("max_flight_seconds must be > 0 and <= 120")
	case c.MaxFlightSeconds/c.TimeStep > maxSimulationSteps:
		return fmt.Errorf("max_flight_seconds / time_step is more than %d steps", maxSimulationSteps)
	case c.Gravity < 0:
		return errors.New("gravity must not be negative")
	case c.Radius < 0:
		return errors.New("radius must not be negative")
	case c.Restitution < 0 || c.Restitution > 1:
		return errors.New("restitution must be in [0, 1]")
	case c.Friction < 0 || c.Friction > 1:
		return errors.New("friction must be in [0, 1]")
	case c.RollingFriction < 0:
		return errors.New("rolling_friction must not be negative")
	case c.FloorNormalZ < -1 || c.FloorNormalZ > 1:
		return errors.New("floor_normal_z must be in [-1, 1]")
	case c.RestSpeed < 0 || c.ContactNormalSpeed < 0:
		return errors.New("rest_speed and contact_normal_speed must not be negative")
	case c.RestSteps < 1:
		return errors.New("rest_steps must be at least 1")
	case c.EnclosureProbe < 0 || c.WorldMargin < 0:
		return errors.New("enclosure_probe and world_margin must not be negative")
	}
	return nil
}

func (c Constants) fuseFor(t NadeType) (float64, bool) {
	switch t {
	case HE:
		return c.HEFuseSeconds, true
	case Flash:
		return c.FlashFuseSeconds, true
	}
	return 0, false
}

// Point is a world position in raw CS2 source units (Z up), matching the wire
// shape the other map endpoints use. r3.Vector is the internal type; this is
// the one that crosses the wire, because r3.Vector marshals to X/Y/Z and every
// other endpoint here speaks x/y/z.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func (p Point) vec() r3.Vector { return r3.Vector{X: p.X, Y: p.Y, Z: p.Z} }

func pointOf(v r3.Vector) Point { return Point{X: v.X, Y: v.Y, Z: v.Z} }

// Seed is the throw a lineup records: where the grenade left the hand and how
// fast, which together with the map is everything the flight depends on.
type Seed struct {
	Type     NadeType
	Position r3.Vector
	Velocity r3.Vector
}

// StopReason is why the integration ended.
type StopReason string

const (
	// StopRest — the grenade settled on the ground. Smokes and decoys.
	StopRest StopReason = "rest"
	// StopFuse — the timer ran out, in the air or on the ground. HE and flash.
	StopFuse StopReason = "fuse"
	// StopIgnite — a molotov touched ground after its arm delay.
	StopIgnite StopReason = "ignite"
	// StopInsideGeometry — the flight ended somewhere a grenade cannot be:
	// every direction is walled within a few units. Against a new mesh this is
	// the signature of a lineup the map update sealed off.
	StopInsideGeometry StopReason = "inside_geometry"
	// StopStartSealed — the throw ORIGIN is inside geometry. Against a new
	// mesh, something was built where the player used to stand.
	StopStartSealed StopReason = "start_sealed"
	// StopOutOfWorld — the flight left the map's bounding box. Usually a seed
	// that does not belong to this map at all.
	StopOutOfWorld StopReason = "out_of_world"
	// StopMaxFlight — still moving when the clock ran out. The model could not
	// settle it, so it has no opinion about where it went.
	StopMaxFlight StopReason = "max_flight"
)

// ComparableOutcome is the end of one simulated flight.
//
// Read it in pairs. A single outcome is not where a grenade lands — see the
// package doc. Two outcomes from the same Seed and Constants against two meshes
// differ only where the meshes do, and that difference is the product.
type ComparableOutcome struct {
	// ComparisonPoint is where this model's flight ended, in source units. It
	// is named for the only thing it may be used for. It is NOT a landing
	// spot, NOT a detonation position, and must never be rendered to a user as
	// either.
	ComparisonPoint Point `json:"comparison_point"`
	// Resolved is whether the flight reached a definite end (rest, fuse or
	// ignition) somewhere a grenade can actually be. An unresolved outcome
	// carries a ComparisonPoint anyway, for debugging, and it means nothing.
	Resolved bool       `json:"resolved"`
	Stop     StopReason `json:"stop"`
	Bounces  int        `json:"bounces"`
	// FlightSeconds is simulated time, a multiple of TimeStep.
	FlightSeconds float64 `json:"flight_seconds"`
	Steps         int     `json:"steps"`
}

// ErrNoMesh is returned when there is no collision mesh to simulate against.
// Without geometry a flight has nothing to bounce off, so the answer is not
// "it flew forever" but "this cannot be asked".
var ErrNoMesh = errors.New("no collision mesh to simulate against")

// SimulateForComparison integrates one grenade flight against one mesh.
//
// The name is the warning: this function's output exists to be subtracted from
// another run of the same function against a different mesh. On its own it is
// a plausible-looking number with unquantified error. See the package doc.
//
// The error is for a request that cannot be run at all (no mesh, a seed that is
// not a number, constants the integrator rejects). A flight that runs but does
// not resolve is not an error — it is an outcome with Resolved false, because
// "this lineup no longer works" is a result and not a failure.
func SimulateForComparison(mesh *geometry.Mesh, seed Seed, c Constants) (ComparableOutcome, error) {
	if mesh == nil || mesh.Triangles() == 0 {
		return ComparableOutcome{}, ErrNoMesh
	}
	if err := c.Validate(); err != nil {
		return ComparableOutcome{}, err
	}
	if !finite(seed.Position) || !finite(seed.Velocity) {
		return ComparableOutcome{}, errors.New("seed position and velocity must be finite")
	}
	if seed.Velocity.Norm() <= 0 {
		return ComparableOutcome{}, errors.New("seed velocity must not be zero")
	}
	if seed.Type == "" {
		return ComparableOutcome{}, errors.New("seed nade type is required")
	}

	if enclosed(mesh, seed.Position, c.EnclosureProbe) {
		return ComparableOutcome{
			ComparisonPoint: pointOf(seed.Position),
			Stop:            StopStartSealed,
		}, nil
	}

	lo, hi, hasBounds := mesh.Bounds()
	fuse, hasFuse := c.fuseFor(seed.Type)
	dt := c.TimeStep
	steps := int(math.Ceil(c.MaxFlightSeconds / dt))

	pos, vel := seed.Position, seed.Velocity
	out := ComparableOutcome{}
	slow := 0

	for step := 1; step <= steps; step++ {
		// Semi-implicit Euler: gravity is applied to the velocity first and the
		// position is moved at the new velocity. Fixed order, fixed step — this
		// is the whole reason two runs agree to the bit.
		vel.Z -= c.Gravity * dt
		var contact contactInfo
		pos, vel, contact = advance(mesh, pos, vel, dt, c)
		out.Bounces += contact.bounces
		out.Steps = step
		out.FlightSeconds = float64(step) * dt
		out.ComparisonPoint = pointOf(pos)

		if !finite(pos) || (hasBounds && outsideBounds(pos, lo, hi, c.WorldMargin)) {
			out.Stop = StopOutOfWorld
			return out, nil
		}
		// A fuse beats everything: an HE at rest still goes off on time.
		if hasFuse && out.FlightSeconds >= fuse {
			out.Stop, out.Resolved = StopFuse, true
			break
		}
		if seed.Type == Molotov && contact.floor && out.FlightSeconds >= c.MolotovArmSeconds {
			out.Stop, out.Resolved = StopIgnite, true
			break
		}
		if contact.floor && vel.Norm() < c.RestSpeed {
			slow++
		} else {
			slow = 0
		}
		if slow >= c.RestSteps {
			out.Stop, out.Resolved = StopRest, true
			break
		}
	}
	if !out.Resolved {
		if out.Stop == "" {
			out.Stop = StopMaxFlight
		}
		return out, nil
	}
	// A resolved flight still has to have resolved somewhere a grenade can be.
	// This is the check that catches a lineup the map update walled off: the
	// flight runs fine and comes to rest inside the new geometry.
	if enclosed(mesh, pos, c.EnclosureProbe) {
		out.Stop, out.Resolved = StopInsideGeometry, false
	}
	return out, nil
}

// contactInfo is what one step's collision resolution reports back.
type contactInfo struct {
	bounces int
	// floor is whether any contact this step was with a surface facing up
	// enough to stand on. Rest and molotov ignition both key off it; a wall is
	// not somewhere a grenade lands.
	floor bool
	// resting is whether any contact this step was a settling one rather than
	// a bounce, which is what earns the rolling-friction decay.
	resting bool
}

// advance moves the grenade for one timestep, resolving up to
// maxContactsPerStep surfaces along the way.
func advance(mesh *geometry.Mesh, pos, vel r3.Vector, dt float64, c Constants) (r3.Vector, r3.Vector, contactInfo) {
	var info contactInfo
	remaining := dt
	for i := 0; i < maxContactsPerStep; i++ {
		speed := vel.Norm()
		if remaining <= 0 || speed <= 0 {
			break
		}
		dist := speed * remaining
		dir := vel.Mul(1 / speed)
		hit, ok := mesh.RayHitSurface(pos, dir)
		if !ok || hit.Distance > dist+c.Radius {
			pos = pos.Add(dir.Mul(dist))
			remaining = 0
			break
		}
		// Stop a radius short of the surface. Exact head-on, early on a
		// glancing hit — and consistently so, which is what matters here.
		travel := hit.Distance - c.Radius
		if travel < 0 {
			travel = 0
		}
		if travel > dist {
			travel = dist
		}
		pos = pos.Add(dir.Mul(travel))
		remaining -= travel / speed

		n := hit.Normal // oriented against dir, so vn below is never positive
		if n.Z >= c.FloorNormalZ {
			info.floor = true
		}
		vn := vel.Dot(n)
		vt := vel.Sub(n.Mul(vn))
		if -vn < c.ContactNormalSpeed {
			// Settling onto the surface: drop the normal component instead of
			// reflecting it. Reflecting a near-zero approach speed is what
			// makes a grenade jitter forever on a floor.
			vel = vt
			info.resting = true
		} else {
			vel = vt.Mul(1 - c.Friction).Sub(n.Mul(vn * c.Restitution))
			info.bounces++
		}
	}
	if info.resting {
		// Per second, not per contact: a grenade in continuous contact touches
		// down once per step, and charging it a bounce's worth of friction each
		// time would glue it to the first flat surface it found.
		decay := 1 - c.RollingFriction*dt
		if decay < 0 {
			decay = 0
		}
		vel = vel.Mul(decay)
	}
	return pos, vel, info
}

// axes are the six directions the enclosure probe casts along. Fixed order, so
// the early exit below cannot make the answer depend on anything.
var axes = [6]r3.Vector{
	{X: 1}, {X: -1}, {Y: 1}, {Y: -1}, {Z: 1}, {Z: -1},
}

// enclosed reports whether a point is buried in geometry: every one of the six
// axes hits a surface within probe units, so there is no room around it for a
// grenade to be.
//
// This is a cheap stand-in for a real inside/outside test, which a collision
// mesh cannot support anyway — the .tri sets are soups of triangles, not closed
// solids, so ray parity says nothing. It answers the question that actually
// matters ("is there space here") rather than the one that does not ("is this
// point within a volume").
func enclosed(mesh *geometry.Mesh, at r3.Vector, probe float64) bool {
	if probe <= 0 {
		return false
	}
	for _, d := range axes {
		hit, ok := mesh.RayHitSurface(at, d)
		if !ok || hit.Distance > probe {
			return false
		}
	}
	return true
}

func outsideBounds(p, lo, hi r3.Vector, margin float64) bool {
	return p.X < lo.X-margin || p.X > hi.X+margin ||
		p.Y < lo.Y-margin || p.Y > hi.Y+margin ||
		p.Z < lo.Z-margin || p.Z > hi.Z+margin
}

func finite(v r3.Vector) bool {
	for _, c := range [3]float64{v.X, v.Y, v.Z} {
		if math.IsNaN(c) || math.IsInf(c, 0) {
			return false
		}
	}
	return true
}
