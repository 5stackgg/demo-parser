// Package discover finds grenade throws that land on a chosen spot.
//
// Unlike package simulate, whose output is only ever meaningful as a
// difference between two meshes, this package predicts: its flight model is
// fitted to throws the engine recorded (see testdata and calibration_test.go),
// and what it returns is a ranked list of candidates for a game server to throw
// for real. A candidate is a proposal, never a result; the server's landing is.
package discover

import (
	"math"

	"github.com/5stackgg/demo-parser/internal/geometry"
	"github.com/golang/geo/r3"
)

// Model is the grenade physics, as CS2 runs it for a thrown grenade: a toss
// integrated per server tick under a fraction of world gravity, reflected off
// whatever it hits and scaled down by the elasticity, until it touches a floor
// slowly enough to stop.
type Model struct {
	// Gravity, in u/s². Every recorded arc fits 320 to a hundredth: sv_gravity
	// 800 at the projectile's 0.4.
	Gravity float64
	// TickRate is how often the server moves a grenade.
	TickRate float64
	// Elasticity is the fraction of the whole reflected velocity a bounce
	// keeps: the grenade's own elasticity times the surface's. The mesh has no
	// surface materials, so it is one number, fitted.
	Elasticity float64
	// StopSpeed: a bounce that leaves less than this stops the grenade dead,
	// on a floor for good and against a wall until gravity takes it again.
	StopSpeed float64
	// FloorNormalZ is how upright a surface must face to be a floor.
	FloorNormalZ float64
	// HalfExtent is half the grenade's collision box. The world is traced
	// with rays from the box's centre and corners, which catches the ledges a
	// single centre ray would slip past.
	HalfExtent float64
	// MaxSeconds bounds a flight that never settles.
	MaxSeconds float64
	// CentreOnly traces the box's centre alone: about a ninth of the cost,
	// for a coarse search that only ranks.
	CentreOnly bool
}

// DefaultModel is the fitted model. TestModelLandsWhereTheEngineDid holds it to
// the recorded throws.
func DefaultModel() Model {
	return Model{
		Gravity:      320,
		TickRate:     64,
		Elasticity:   0.45,
		StopSpeed:    30,
		FloorNormalZ: 0.7,
		HalfExtent:   2,
		MaxSeconds:   20,
	}
}

// Flight is one predicted grenade flight.
type Flight struct {
	// Rest is where the grenade came to a stop, which for a smoke is where it
	// pops. Meaningful only when Settled.
	Rest    r3.Vector
	Settled bool
	// FirstImpact is the first thing it hit, which is exact whenever the
	// mesh is: everything before it is a parabola.
	FirstImpact    r3.Vector
	HasFirstImpact bool
	Bounces        int
	Seconds        float64
	// Path is the grenade's position each tick, when asked for.
	Path []r3.Vector
}

// corners are the sweep points of the collision box, centre first. A ray from
// each, along the move, stands in for a box trace the BVH cannot do.
var corners = [9]r3.Vector{
	{X: 0, Y: 0, Z: 0},
	{X: 1, Y: 1, Z: 1}, {X: 1, Y: 1, Z: -1}, {X: 1, Y: -1, Z: 1}, {X: 1, Y: -1, Z: -1},
	{X: -1, Y: 1, Z: 1}, {X: -1, Y: 1, Z: -1}, {X: -1, Y: -1, Z: 1}, {X: -1, Y: -1, Z: -1},
}

// sweep returns how far the box gets along dir before it touches something,
// and that surface's normal, within limit.
func (m Model) sweep(world *geometry.GrenadeWorld, from, dir r3.Vector, limit float64) (float64, r3.Vector, bool) {
	best := math.Inf(1)
	var normal r3.Vector
	sweepFrom := corners[:]
	if m.CentreOnly {
		sweepFrom = corners[:1]
	}
	for _, c := range sweepFrom {
		origin := from.Add(c.Mul(m.HalfExtent))
		hit, ok := world.RayHitSurface(origin, dir)
		if !ok || hit.Distance > limit || hit.Distance >= best {
			continue
		}
		best = hit.Distance
		normal = hit.Normal
	}
	if math.IsInf(best, 1) {
		return 0, r3.Vector{}, false
	}
	return best, normal, true
}

// Impact is where a grenade first hits anything, which is all the parabola
// decides; everything after it is bounces. Cheap, and exact wherever the mesh
// is.
func (m Model) Impact(world *geometry.GrenadeWorld, position, velocity r3.Vector) (r3.Vector, bool) {
	dt := 1 / m.TickRate
	ticks := int(m.MaxSeconds * m.TickRate)
	pos, vel := position, velocity
	for tick := 1; tick <= ticks; tick++ {
		vz := vel.Z - m.Gravity*dt
		move := r3.Vector{X: vel.X * dt, Y: vel.Y * dt, Z: (vel.Z + vz) / 2 * dt}
		vel.Z = vz
		length := move.Norm()
		if length <= 0 {
			continue
		}
		dir := move.Mul(1 / length)
		distance, _, hit := m.sweep(world, pos, dir, length)
		if hit {
			return pos.Add(dir.Mul(distance)), true
		}
		pos = pos.Add(move)
	}
	return pos, false
}

// Fly integrates one grenade from its launch seed.
func (m Model) Fly(world *geometry.GrenadeWorld, position, velocity r3.Vector, keepPath bool) Flight {
	var flight Flight
	dt := 1 / m.TickRate
	ticks := int(m.MaxSeconds * m.TickRate)
	pos, vel := position, velocity

	for tick := 1; tick <= ticks; tick++ {
		// A toss moves at the average of this tick's start and end vertical
		// speed, so the arc is the exact parabola the recordings show.
		vz := vel.Z - m.Gravity*dt
		move := r3.Vector{X: vel.X * dt, Y: vel.Y * dt, Z: (vel.Z + vz) / 2 * dt}
		vel.Z = vz
		flight.Seconds = float64(tick) * dt

		length := move.Norm()
		if length <= 0 {
			if keepPath {
				flight.Path = append(flight.Path, pos)
			}
			continue
		}
		dir := move.Mul(1 / length)
		distance, normal, hit := m.sweep(world, pos, dir, length)
		if !hit {
			pos = pos.Add(move)
			if keepPath {
				flight.Path = append(flight.Path, pos)
			}
			continue
		}

		pos = pos.Add(dir.Mul(distance))
		if !flight.HasFirstImpact {
			flight.FirstImpact, flight.HasFirstImpact = pos, true
		}
		if keepPath {
			flight.Path = append(flight.Path, pos)
		}

		// A backoff of two is a reflection; the elasticity then scales all of
		// it, along the surface as well as off it.
		reflected := vel.Sub(normal.Mul(2 * vel.Dot(normal))).Mul(m.Elasticity)
		slow := reflected.Norm() < m.StopSpeed

		if normal.Z > m.FloorNormalZ {
			if slow {
				flight.Rest, flight.Settled = pos, true
				return flight
			}
			flight.Bounces++
			vel = reflected
			continue
		}

		flight.Bounces++
		if slow {
			vel = r3.Vector{}
			continue
		}
		vel = reflected
	}

	flight.Rest = pos
	return flight
}
