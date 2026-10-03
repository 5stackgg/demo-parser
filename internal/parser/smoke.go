package parser

import (
	"encoding/base64"
	"math"

	"github.com/5stackgg/demo-parser/internal/geometry"
	"github.com/golang/geo/r3"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"
)

// CS2 smoke is a client-side volumetric voxel grid. The server networks only
// the simulation inputs — source points and geometry masks — on
// CSmokeGrenadeProjectile, and the client's fluid sim plus shader turn those
// into the image a player sees. Reproducing the shader would tell us what the
// smoke looked like; we only need to know where it *was*.
//
// So rather than guess a sphere, the volume is derived from the one thing that
// actually determines a smoke's shape: the map. A cloud is the free space
// reachable from its detonation point, computed against the same collision mesh
// the sightline raycasts already use. A smoke thrown into a corridor comes out
// corridor-shaped and one thrown against a wall stops at it, because the
// geometry says so — which is what the game's own "geometry masks" encode.
const (
	// smokeRadius is how far the cloud reaches from its detonation point.
	smokeRadius = 144.0
	// smokeBloomSecs is how long the cloud takes to reach smokeRadius. The
	// volume is built once at full size; the ramp is applied per query, since a
	// smoke does not block a sightline it has not yet grown across.
	smokeBloomSecs = 1.0
	// smokeVoxelSize is the edge length of one occupancy cell, in source units.
	// Small enough to resolve a doorway, coarse enough that a whole cloud costs
	// a few thousand rays once.
	smokeVoxelSize = 16.0
)

// An explosion punches a hole in smoke. Per the article's breakdown of the CS2
// smoke shader, a blast is tracked for about 5 seconds with an influence radius
// of 250 units — full strength within 100, fading to nothing by 250 — and drives
// density down to 2%, which is transparent. So a sightline through a freshly
// nade'd smoke was genuinely clear, and treating it as blocked is wrong.
//
// The spatial numbers below are the article's. The refill time is ours, and is
// the knob worth revisiting if the effect looks too generous or too brief.
const (
	// blockingDepth is how much smoke hides a target, in cell widths of fully
	// dense smoke. Read as Beer-Lambert optical depth: e^-3 leaves about 5% of
	// the target visible, which is the point a silhouette stops being useful.
	//
	// Measured against a clean cloud, a sightline through the centre has a depth
	// of ~13.6, falling to ~1 at nine tenths of the radius. So this blocks out
	// to roughly 80% of the cloud's width and leaves the outer rim translucent —
	// which is how the edge of a smoke actually behaves.
	blockingDepth = 3.0
	// residualDensity is what a blast leaves behind where it is strongest.
	//
	// The shader's term is `density *= 0.02 + 0.98 * occlusion`, so 2% is the
	// floor only where that occlusion term is zero — and we do not model it.
	// Applying a flat 2% erases small clouds outright, which is not what an HE
	// does: it thins a smoke and you can briefly see through, but the cloud is
	// plainly still there and reforms. This sits above the bare floor to stand
	// in for the term we are missing, and still drops a cloud's core depth
	// (~13.6) below the blocking threshold, so a centred blast opens a sightline.
	residualDensity = 0.15

	// Inside the "full" radius the thinning is at full strength; it fades to
	// nothing by the outer radius.
	heBlastRadius     = 250.0
	heBlastFullRadius = 100.0
	// The bomb's blast dwarfs a grenade's. Nothing is scored after it goes off,
	// so this mostly matters to the renderers.
	c4BlastRadius     = 600.0
	c4BlastFullRadius = 240.0

	blastClearSecs = 2.0

	// minCloudCells is the smallest flood that counts as a real cloud. An
	// unobstructed smoke fills a couple of thousand cells and even a tight
	// corridor fills hundreds, so anything under this is a failed reconstruction
	// rather than a genuinely tiny cloud.
	minCloudCells = 48
)

// smokeBlast is one explosion that displaces smoke.
type smokeBlast struct {
	center r3.Vector
	tick   int
	radius float64
	full   float64
}

// clearRadiusAt is how far from the blast smoke is still displaced at a tick,
// shrinking to nothing as the cloud fills back in.
func (b smokeBlast) clearRadiusAt(tick int, rate float64) float64 {
	if tick < b.tick {
		return 0
	}
	if rate <= 0 {
		return 0
	}
	age := float64(tick-b.tick) / rate
	if age >= blastClearSecs {
		return 0
	}
	return b.radius * (1 - age/blastClearSecs)
}

// activeBlast is a blast resolved to a concrete cleared sphere at one tick.
type activeBlast struct {
	center   r3.Vector
	radiusSq float64
	fullSq   float64
}

// blastsAt collects the explosions still holding a hole open at a tick.
func (s *state) blastsAt(tick int) []activeBlast {
	if len(s.blasts) == 0 {
		return nil
	}
	var out []activeBlast
	for _, b := range s.blasts {
		if r := b.clearRadiusAt(tick, s.tickRate); r > 0 {
			// The full-strength core shrinks with the influence radius, so the
			// hole closes from the outside in as the cloud fills back.
			full := b.full * (r / b.radius)
			out = append(out, activeBlast{
				center:   b.center,
				radiusSq: r * r,
				fullSq:   full * full,
			})
		}
	}
	return out
}

// recordBlast registers an explosion. Blasts are cleared per round along with
// the clouds they act on.
func (s *state) recordBlast(tick int, center r3.Vector, radius, full float64) {
	if center.X == 0 && center.Y == 0 && center.Z == 0 {
		return
	}
	s.blasts = append(s.blasts, smokeBlast{center: center, tick: tick, radius: radius, full: full})
	s.blastCount++
}

// A bullet bores a tunnel through smoke. The client clears the cells along
// every tracer that crosses a cloud and lets them fill back in, so a player
// spraying into a smoke opens a sightline down their own line of fire — and,
// since a tunnel runs both ways, the one back at them too.
//
// Unlike the blast numbers, none of these come from the shader breakdown; they
// are ours, and the knobs worth revisiting against real demos. The tracer is
// taken along the shooter's view rather than the bullet's spread or recoil, so
// the full-strength radius is also slack for a line of fire a degree or so off
// the crosshair. It has to be more than half a cell wide regardless: any cell a
// sightline crosses has its centre up to half a cell diagonal (~14u) away, and
// a tunnel narrower than that cannot open even the sightline running straight
// down it.
const (
	bulletHoleRadius     = 40.0
	bulletHoleFullRadius = 20.0
	bulletHoleSecs       = 1.0
	// bulletReach caps a tracer that hits no wall. Past it the round is spent.
	bulletReach = 8192.0
)

// bulletHole is one tracer that crossed a cloud.
type bulletHole struct {
	from, to r3.Vector
	tick     int
}

// radiusAt is how wide the tunnel still is at a tick, closing to nothing as the
// cloud fills back in.
func (h bulletHole) radiusAt(tick int, rate float64) float64 {
	if tick < h.tick || rate <= 0 {
		return 0
	}
	age := float64(tick-h.tick) / rate
	if age >= bulletHoleSecs {
		return 0
	}
	return bulletHoleRadius * (1 - age/bulletHoleSecs)
}

// activeHole is a tunnel resolved to a concrete cleared capsule at one tick.
type activeHole struct {
	from, to r3.Vector
	radiusSq float64
	fullSq   float64
}

// recordBulletHole registers a tracer, but only when it passes near a cloud
// that is up at the time — almost every shot goes nowhere near smoke and should
// cost the sightline tests nothing. Holes are cleared per round along with the
// clouds they act on.
func (s *state) recordBulletHole(tick int, from, to r3.Vector) {
	for i := range s.smokes {
		c := &s.smokes[i]
		if c.vol == nil || c.bloomRadiusAt(tick, s.tickRate) <= 0 {
			continue
		}
		if c.vol.nearSegment(from, to, bulletHoleRadius) {
			s.holes = append(s.holes, bulletHole{from: from, to: to, tick: tick})
			s.holeCount++
			return
		}
	}
}

// holesAt collects the tunnels still open at a tick that could touch a
// sightline. A tunnel only thins cells within its radius of the tracer, and a
// cell the sightline crosses has its centre within a cell width of it, so a
// tunnel further off than the two together is dropped here once rather than
// tested again at every cell.
func (s *state) holesAt(tick int, from, to r3.Vector) []activeHole {
	if len(s.holes) == 0 {
		return nil
	}
	var out []activeHole
	for _, h := range s.holes {
		r := h.radiusAt(tick, s.tickRate)
		if r <= 0 {
			continue
		}
		reach := r + smokeVoxelSize
		if segmentDistSq(from, to, h.from, h.to) > reach*reach {
			continue
		}
		// As with blasts, the full-strength core shrinks with the tunnel.
		full := bulletHoleFullRadius * (r / bulletHoleRadius)
		out = append(out, activeHole{from: h.from, to: h.to, radiusSq: r * r, fullSq: full * full})
	}
	return out
}

// smokeClearing is everything holding smoke open at one tick: explosions and
// bullet tunnels.
type smokeClearing struct {
	blasts []activeBlast
	holes  []activeHole
}

func (cl smokeClearing) empty() bool {
	return len(cl.blasts) == 0 && len(cl.holes) == 0
}

// smokeVolume is a voxel density field. origin is the world position of the
// (0,0,0) cell's minimum corner, so cell (i,j,k) spans
// origin + (i,j,k)*size … origin + (i+1,j+1,k+1)*size.
//
// Density rather than a yes/no bit, because smoke is not binary. A sightline
// through the thin edge of a cloud is not the same as one through its core, and
// an explosion *thins* smoke rather than deleting it — under an occupancy model
// a blast only counts when it clears an entire chord, which measurement showed
// almost never happens. Densities also give the renderers something to shade
// with, so the drawn cloud has a soft edge instead of a hard silhouette.
type smokeVolume struct {
	origin r3.Vector
	size   float64
	dim    [3]int
	// density is 0 (clear) to densityMax (opaque core), one byte per cell.
	density []uint8
}

// densityMax is full opacity in the stored field.
const densityMax = 255

func (v *smokeVolume) index(i, j, k int) int {
	return (k*v.dim[1]+j)*v.dim[0] + i
}

func (v *smokeVolume) inBounds(i, j, k int) bool {
	return i >= 0 && j >= 0 && k >= 0 && i < v.dim[0] && j < v.dim[1] && k < v.dim[2]
}

// at returns a cell's density, 0 outside the grid.
func (v *smokeVolume) at(i, j, k int) uint8 {
	if !v.inBounds(i, j, k) {
		return 0
	}
	return v.density[v.index(i, j, k)]
}

// get reports whether a cell holds any smoke at all. Used for the flood and for
// the footprint, where presence rather than thickness is the question.
func (v *smokeVolume) get(i, j, k int) bool {
	return v.at(i, j, k) > 0
}

// cellCenter is the world position of a cell's midpoint.
func (v *smokeVolume) cellCenter(i, j, k int) r3.Vector {
	return r3.Vector{
		X: v.origin.X + (float64(i)+0.5)*v.size,
		Y: v.origin.Y + (float64(j)+0.5)*v.size,
		Z: v.origin.Z + (float64(k)+0.5)*v.size,
	}
}

// smokeNeighbors is the 6-connected neighbourhood used for both the flood fill
// and the creep pass. Smoke spreads through faces, not diagonally through the
// edge where two walls meet.
var smokeNeighbors = [6][3]int{{1, 0, 0}, {-1, 0, 0}, {0, 1, 0}, {0, -1, 0}, {0, 0, 1}, {0, 0, -1}}

// Shape of a settled cloud. A smoke is not a ball: it pools on the floor and
// spreads sideways further than it climbs, so the reach is scaled per axis.
// Vertical reach is measured from the detonation point, which is where the
// grenade came to rest — usually on the ground.
const (
	smokeVerticalScale = 0.78
	// smokeCoreFrac: within this fraction of the reach the cloud is at full
	// density; beyond it density falls off to nothing at the edge. Gives the
	// cloud an opaque core and a soft rim rather than a hard boundary.
	smokeCoreFrac = 0.55
)

// shapedDistance maps a cell offset onto a 0→1 radius where 1 is the cloud's
// edge, squashing the vertical axis so the cloud sits rather than floats.
func shapedDistance(dx, dy, dz float64) float64 {
	vz := dz / smokeVerticalScale
	return math.Sqrt(dx*dx+dy*dy+vz*vz) / smokeRadius
}

// buildSmokeVolume computes the density field for a cloud centred on the
// detonation point.
//
// Four passes:
//
//  1. Sight — a cell within reach is a candidate when its centre can see the
//     detonation point through the world. One ray per candidate. This is what
//     stops a cloud at the wall it was thrown against.
//  2. Fill — flood from the centre through candidates, so a pocket that is
//     technically visible through a crack but not part of the same body of gas
//     is dropped.
//  3. Creep — one expansion into cells the centre cannot see directly, allowed
//     only when the short hop from an occupied neighbour is itself clear. Real
//     smoke rolls a little way around a corner; pure line-of-sight from a point
//     can never do that. The clearance test keeps the creep from stepping
//     through a thin wall.
//  4. Shade — assign density by shaped distance, then blur. The blur is what
//     turns a stack of cubes into something that reads as gas, and it softens
//     the boundary where the cloud was cut off by geometry.
//
// Returns nil when there is no mesh, so callers fall back to the sphere.
func buildSmokeVolume(mesh *geometry.Mesh, center r3.Vector) (*smokeVolume, bool) {
	if mesh == nil {
		return nil, false
	}
	n := int(math.Ceil(smokeRadius / smokeVoxelSize))
	dim := 2*n + 1
	v := &smokeVolume{
		size: smokeVoxelSize,
		dim:  [3]int{dim, dim, dim},
		origin: r3.Vector{
			X: center.X - (float64(n)+0.5)*smokeVoxelSize,
			Y: center.Y - (float64(n)+0.5)*smokeVoxelSize,
			Z: center.Z - (float64(n)+0.5)*smokeVoxelSize,
		},
	}
	total := dim * dim * dim
	v.density = make([]uint8, total)

	// reach is the shaped 0→1 radius of each cell, cached for the shading pass.
	reach := make([]float64, total)
	inReach := func(i, j, k int) bool {
		c := v.cellCenter(i, j, k)
		r := shapedDistance(c.X-center.X, c.Y-center.Y, c.Z-center.Z)
		reach[v.index(i, j, k)] = r
		return r <= 1
	}

	// Pass 1: visibility from the detonation point.
	lit := make([]bool, total)
	for k := 0; k < dim; k++ {
		for j := 0; j < dim; j++ {
			for i := 0; i < dim; i++ {
				if !inReach(i, j, k) {
					continue
				}
				if !mesh.Occluded(center, v.cellCenter(i, j, k)) {
					lit[v.index(i, j, k)] = true
				}
			}
		}
	}

	// Pass 2: flood from the centre cell through lit cells.
	type cell struct{ i, j, k int }
	filled := make([]bool, total)
	start := cell{n, n, n}
	// Always seed the centre. Whether the detonation point was somewhere
	// sensible is judged at the end from how much cloud actually formed —
	// testing the centre cell's own visibility cannot work, since the ray from
	// the detonation point to itself has zero length and never hits anything.
	lit[v.index(start.i, start.j, start.k)] = true
	queue := make([]cell, 0, 512)
	queue = append(queue, start)
	filled[v.index(start.i, start.j, start.k)] = true
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		for _, d := range smokeNeighbors {
			ni, nj, nk := c.i+d[0], c.j+d[1], c.k+d[2]
			if !v.inBounds(ni, nj, nk) {
				continue
			}
			idx := v.index(ni, nj, nk)
			if filled[idx] || !lit[idx] {
				continue
			}
			filled[idx] = true
			queue = append(queue, cell{ni, nj, nk})
		}
	}

	// Pass 3: creep one cell into shadowed space, only across clear hops.
	var creep []int
	for k := 0; k < dim; k++ {
		for j := 0; j < dim; j++ {
			for i := 0; i < dim; i++ {
				if !filled[v.index(i, j, k)] {
					continue
				}
				for _, d := range smokeNeighbors {
					ni, nj, nk := i+d[0], j+d[1], k+d[2]
					if !v.inBounds(ni, nj, nk) {
						continue
					}
					idx := v.index(ni, nj, nk)
					if filled[idx] || reach[idx] > 1 {
						continue
					}
					if mesh.Occluded(v.cellCenter(i, j, k), v.cellCenter(ni, nj, nk)) {
						continue
					}
					creep = append(creep, idx)
				}
			}
		}
	}
	for _, idx := range creep {
		filled[idx] = true
	}

	// A smoke always expands to fill something. If the flood found almost
	// nothing, the detonation point resolved inside geometry — the mesh is
	// coarse or missing a surface there, or the networked position was off — and
	// the result is a couple of cells that render as nothing at all. Report it
	// and fall back to the unshaped sphere, which is wrong but visible.
	reached := 0
	for _, ok := range filled {
		if ok {
			reached++
		}
	}
	if reached < minCloudCells {
		return nil, true
	}

	// Pass 4: shade by shaped distance, then blur.
	raw := make([]float64, total)
	for idx, ok := range filled {
		if !ok {
			continue
		}
		d := 1.0
		if r := reach[idx]; r > smokeCoreFrac {
			d = 1 - (r-smokeCoreFrac)/(1-smokeCoreFrac)
			if d < 0 {
				d = 0
			}
			// Smoothstep, so the rim fades rather than ramping linearly.
			d = d * d * (3 - 2*d)
		}
		raw[idx] = d
	}
	v.blurInto(raw, filled)
	return v, false
}

// blurInto writes a 3×3×3 box-blurred copy of raw into the density field.
//
// Only cells the flood actually reached are written. Blurring into the rest
// would grow the cloud by a cell in every direction — including straight
// through walls, which is the one property this whole model exists to get
// right. Neighbours outside the cloud still contribute zero to the average, so
// the boundary fades rather than ending abruptly; the extent just stops where
// the geometry says it does.
func (v *smokeVolume) blurInto(raw []float64, filled []bool) {
	get := func(i, j, k int) float64 {
		if !v.inBounds(i, j, k) {
			return 0
		}
		return raw[v.index(i, j, k)]
	}
	for k := 0; k < v.dim[2]; k++ {
		for j := 0; j < v.dim[1]; j++ {
			for i := 0; i < v.dim[0]; i++ {
				if !filled[v.index(i, j, k)] {
					continue
				}
				sum, wsum := 0.0, 0.0
				for dk := -1; dk <= 1; dk++ {
					for dj := -1; dj <= 1; dj++ {
						for di := -1; di <= 1; di++ {
							// Centre weighted highest so the core stays dense.
							w := 1.0
							if di == 0 && dj == 0 && dk == 0 {
								w = 4
							}
							sum += get(i+di, j+dj, k+dk) * w
							wsum += w
						}
					}
				}
				d := sum / wsum
				// Drop the faintest haze; it is below anything a viewer or a
				// sightline would notice and it keeps the export sparse.
				if d < 0.06 {
					continue
				}
				if d > 1 {
					d = 1
				}
				v.density[v.index(i, j, k)] = uint8(d*densityMax + 0.5)
			}
		}
	}
}

// occludedSegment reports whether the segment crosses an occupied cell that has
// also grown by the given tick. bloomRadius shrinks the effective cloud while it
// is still billowing out.
//
// The traversal is Amanatides–Woo: clip the segment to the grid, then step cell
// to cell along the ray. Cost is proportional to the cells actually crossed
// rather than the length of the sightline.
// opticalDepth accumulates how much smoke a sightline passes through, in units
// of "cell widths of fully dense smoke". Above blockingDepth the far end is
// considered hidden.
//
// Accumulating rather than stopping at the first cell is what lets a thinned
// cloud be seen through: an explosion that halves the density along a chord
// halves the depth, and a sightline clipping the soft rim of a cloud costs far
// less than one through its core.
// limit lets the boolean test stop early once the answer is settled; pass
// math.Inf(1) for an exact depth.
func (v *smokeVolume) opticalDepthLimit(from, to r3.Vector, center r3.Vector, bloomRadius float64, clr smokeClearing, limit float64) float64 {
	if v == nil || bloomRadius <= 0 {
		return 0
	}
	dir := r3.Vector{X: to.X - from.X, Y: to.Y - from.Y, Z: to.Z - from.Z}
	hi := r3.Vector{
		X: v.origin.X + float64(v.dim[0])*v.size,
		Y: v.origin.Y + float64(v.dim[1])*v.size,
		Z: v.origin.Z + float64(v.dim[2])*v.size,
	}
	t0, t1, ok := clipSegmentBox(from, dir, v.origin, hi)
	if !ok {
		return 0
	}
	segLen := math.Sqrt(dir.X*dir.X + dir.Y*dir.Y + dir.Z*dir.Z)

	o := [3]float64{from.X, from.Y, from.Z}
	d := [3]float64{dir.X, dir.Y, dir.Z}
	lo := [3]float64{v.origin.X, v.origin.Y, v.origin.Z}

	var (
		cur    [3]int
		step   [3]int
		tMax   [3]float64
		tDelta [3]float64
	)
	for a := 0; a < 3; a++ {
		p := o[a] + d[a]*t0 - lo[a]
		c := int(math.Floor(p / v.size))
		if c < 0 {
			c = 0
		}
		if c >= v.dim[a] {
			c = v.dim[a] - 1
		}
		cur[a] = c
		switch {
		case d[a] > 1e-12:
			step[a] = 1
			tMax[a] = (lo[a] + float64(c+1)*v.size - o[a]) / d[a]
			tDelta[a] = v.size / d[a]
		case d[a] < -1e-12:
			step[a] = -1
			tMax[a] = (lo[a] + float64(c)*v.size - o[a]) / d[a]
			tDelta[a] = -v.size / d[a]
		default:
			step[a] = 0
			tMax[a] = math.Inf(1)
			tDelta[a] = math.Inf(1)
		}
	}

	bloomSq := bloomRadius * bloomRadius
	depth := 0.0
	t := t0
	for t <= t1 {
		// Advance to the nearest cell boundary first, so the distance actually
		// travelled inside this cell is known before it is weighted.
		a := 0
		if tMax[1] < tMax[a] {
			a = 1
		}
		if tMax[2] < tMax[a] {
			a = 2
		}
		tNext := tMax[a]
		if step[a] == 0 || tNext > t1 {
			tNext = t1
		}

		if d := v.at(cur[0], cur[1], cur[2]); d > 0 {
			c := v.cellCenter(cur[0], cur[1], cur[2])
			dx, dy, dz := c.X-center.X, c.Y-center.Y, c.Z-center.Z
			if dx*dx+dy*dy+dz*dz <= bloomSq {
				// Path length through this cell, expressed in cell widths so
				// the result does not depend on the grid's resolution.
				span := (tNext - t) * segLen / v.size
				depth += float64(d) / densityMax * span * clr.thinning(c)
				if depth >= limit {
					return depth
				}
			}
		}

		if tNext >= t1 || step[a] == 0 {
			break
		}
		cur[a] += step[a]
		if cur[a] < 0 || cur[a] >= v.dim[a] {
			break
		}
		t = tMax[a]
		tMax[a] += tDelta[a]
	}
	return depth
}

// opticalDepth is the exact amount of smoke on a sightline.
func (v *smokeVolume) opticalDepth(from, to r3.Vector, center r3.Vector, bloomRadius float64, clr smokeClearing) float64 {
	return v.opticalDepthLimit(from, to, center, bloomRadius, clr, math.Inf(1))
}

// occludedSegment reports whether a sightline is hidden by this cloud. Stops
// accumulating as soon as the threshold is passed, since the exact figure does
// not matter once the answer is known.
func (v *smokeVolume) occludedSegment(from, to r3.Vector, center r3.Vector, bloomRadius float64, clr smokeClearing) bool {
	return v.opticalDepthLimit(from, to, center, bloomRadius, clr, blockingDepth) >= blockingDepth
}

// thinning returns the fraction of a cell's density that survives the
// explosions and bullet tunnels currently acting on it.
//
// The article's breakdown of the shader has a blast drive density to 2% at full
// strength, with the effect at full strength inside 100 units and fading out by
// 250. That gradient is reproduced here rather than collapsed to a hole, since
// the whole point of a density field is that partial thinning counts. A tunnel
// gets the same profile, measured from the tracer rather than a point.
func (cl smokeClearing) thinning(c r3.Vector) float64 {
	survive := 1.0
	for _, b := range cl.blasts {
		dx, dy, dz := c.X-b.center.X, c.Y-b.center.Y, c.Z-b.center.Z
		survive *= 1 - clearStrength(dx*dx+dy*dy+dz*dz, b.radiusSq, b.fullSq)*(1-residualDensity)
	}
	for _, h := range cl.holes {
		survive *= 1 - clearStrength(pointSegmentDistSq(c, h.from, h.to), h.radiusSq, h.fullSq)*(1-residualDensity)
	}
	return survive
}

// clearStrength is how hard a clearing acts at a squared distance from it: 1
// inside the full radius, easing to 0 at the outer one. residualDensity is the
// floor that leaves behind at full strength — thinned, not erased.
func clearStrength(distSq, radiusSq, fullSq float64) float64 {
	if distSq >= radiusSq {
		return 0
	}
	if distSq <= fullSq {
		return 1
	}
	full := math.Sqrt(fullSq)
	f := (math.Sqrt(distSq) - full) / (math.Sqrt(radiusSq) - full)
	return 1 - f*f*(3-2*f)
}

// pointSegmentDistSq is the squared distance from p to the segment a → b.
func pointSegmentDistSq(p, a, b r3.Vector) float64 {
	ab := b.Sub(a)
	t := 0.0
	if l := ab.Norm2(); l > 1e-12 {
		t = math.Max(0, math.Min(1, p.Sub(a).Dot(ab)/l))
	}
	return p.Sub(a.Add(ab.Mul(t))).Norm2()
}

// segmentDistSq is the squared distance between the closest points of the
// segments p1 → q1 and p2 → q2: solve for the closest pair on the two infinite
// lines, then clamp each parameter to its segment, re-solving the other after
// each clamp.
func segmentDistSq(p1, q1, p2, q2 r3.Vector) float64 {
	d1, d2, r := q1.Sub(p1), q2.Sub(p2), p1.Sub(p2)
	a, e, f := d1.Norm2(), d2.Norm2(), d2.Dot(r)
	clamp := func(x float64) float64 { return math.Max(0, math.Min(1, x)) }
	var s, t float64
	switch {
	case a <= 1e-12 && e <= 1e-12:
		return r.Norm2()
	case a <= 1e-12:
		t = clamp(f / e)
	case e <= 1e-12:
		s = clamp(-d1.Dot(r) / a)
	default:
		b, c := d1.Dot(d2), d1.Dot(r)
		if denom := a*e - b*b; denom > 1e-12 {
			s = clamp((b*f - c*e) / denom)
		}
		t = (b*s + f) / e
		if t < 0 {
			t, s = 0, clamp(-c/a)
		} else if t > 1 {
			t, s = 1, clamp((b-c)/a)
		}
	}
	return p1.Add(d1.Mul(s)).Sub(p2.Add(d2.Mul(t))).Norm2()
}

// nearSegment reports whether the segment from → to passes within margin of the
// grid's bounds.
func (v *smokeVolume) nearSegment(from, to r3.Vector, margin float64) bool {
	lo := r3.Vector{X: v.origin.X - margin, Y: v.origin.Y - margin, Z: v.origin.Z - margin}
	hi := r3.Vector{
		X: v.origin.X + float64(v.dim[0])*v.size + margin,
		Y: v.origin.Y + float64(v.dim[1])*v.size + margin,
		Z: v.origin.Z + float64(v.dim[2])*v.size + margin,
	}
	_, _, ok := clipSegmentBox(from, to.Sub(from), lo, hi)
	return ok
}

// clipSegmentBox intersects the segment origin+t*dir, t ∈ [0,1], with an
// axis-aligned box, returning the surviving parametric range.
func clipSegmentBox(origin, dir, lo, hi r3.Vector) (float64, float64, bool) {
	t0, t1 := 0.0, 1.0
	o := [3]float64{origin.X, origin.Y, origin.Z}
	d := [3]float64{dir.X, dir.Y, dir.Z}
	mn := [3]float64{lo.X, lo.Y, lo.Z}
	mx := [3]float64{hi.X, hi.Y, hi.Z}
	for a := 0; a < 3; a++ {
		if math.Abs(d[a]) < 1e-12 {
			if o[a] < mn[a] || o[a] > mx[a] {
				return 0, 0, false
			}
			continue
		}
		inv := 1 / d[a]
		p := (mn[a] - o[a]) * inv
		q := (mx[a] - o[a]) * inv
		if p > q {
			p, q = q, p
		}
		if p > t0 {
			t0 = p
		}
		if q < t1 {
			t1 = q
		}
		if t0 > t1 {
			return 0, 0, false
		}
	}
	return t0, t1, true
}

// count reports how many cells hold any smoke.
func (v *smokeVolume) count() int {
	n := 0
	for _, d := range v.density {
		if d > 0 {
			n++
		}
	}
	return n
}

// export flattens the volume into the blob form the 2D radar and 3D replay
// consume. The grid is trimmed to its occupied bounding box first — a cloud
// stopped by a wall occupies a fraction of its budget, and the empty margin is
// pure payload.
func (v *smokeVolume) export(gid, round, startTick int) EventSmokeVolume {
	lo := [3]int{v.dim[0], v.dim[1], v.dim[2]}
	hi := [3]int{-1, -1, -1}
	for k := 0; k < v.dim[2]; k++ {
		for j := 0; j < v.dim[1]; j++ {
			for i := 0; i < v.dim[0]; i++ {
				if !v.get(i, j, k) {
					continue
				}
				c := [3]int{i, j, k}
				for a := 0; a < 3; a++ {
					if c[a] < lo[a] {
						lo[a] = c[a]
					}
					if c[a] > hi[a] {
						hi[a] = c[a]
					}
				}
			}
		}
	}
	out := EventSmokeVolume{
		GrenadeID: gid,
		Round:     round,
		StartTick: startTick,
		VoxelSize: float32(v.size),
	}
	if hi[0] < 0 {
		return out // nothing occupied
	}
	dim := [3]int{hi[0] - lo[0] + 1, hi[1] - lo[1] + 1, hi[2] - lo[2] + 1}
	out.DimX, out.DimY, out.DimZ = dim[0], dim[1], dim[2]
	out.OriginX = float32(v.origin.X + float64(lo[0])*v.size)
	out.OriginY = float32(v.origin.Y + float64(lo[1])*v.size)
	out.OriginZ = float32(v.origin.Z + float64(lo[2])*v.size)

	// Two cells per byte. Sixteen levels is more than enough to shade with, and
	// halves what the blob carries.
	total := dim[0] * dim[1] * dim[2]
	packed := make([]byte, (total+1)/2)
	for k := 0; k < dim[2]; k++ {
		for j := 0; j < dim[1]; j++ {
			for i := 0; i < dim[0]; i++ {
				d := v.at(lo[0]+i, lo[1]+j, lo[2]+k)
				if d == 0 {
					continue
				}
				q := byte(int(d)*15/densityMax + 1)
				if q > 15 {
					q = 15
				}
				n := (k*dim[1]+j)*dim[0] + i
				if n&1 == 0 {
					packed[n>>1] |= q
				} else {
					packed[n>>1] |= q << 4
				}
			}
		}
	}
	out.Density = base64.StdEncoding.EncodeToString(packed)
	return out
}

// smokeCloud is one deployed smoke. endTick is 0 while it is still active, and
// vol is nil when no mesh was available (the sphere fallback).
type smokeCloud struct {
	center    r3.Vector
	startTick int
	endTick   int
	vol       *smokeVolume
	// exportIdx is this cloud's slot in Result.SmokeVolumes, so SmokeExpired
	// can backfill the real end tick for the renderers.
	exportIdx int
}

// bloomRadiusAt returns how far the cloud has grown by a tick, or 0 when it is
// not deployed then.
func (c smokeCloud) bloomRadiusAt(tick int, rate float64) float64 {
	if tick < c.startTick || (c.endTick > 0 && tick > c.endTick) {
		return 0
	}
	if rate <= 0 {
		return smokeRadius
	}
	age := float64(tick-c.startTick) / rate
	if age >= smokeBloomSecs {
		return smokeRadius
	}
	return smokeRadius * (age / smokeBloomSecs)
}

// onSmokeStart opens a cloud in the registry alongside emitting the detonation
// event the replay consumes.
func (s *state) onSmokeStart(e events.SmokeStart) {
	if !s.matchStarted {
		return
	}
	s.emitDetonate(e.GrenadeEvent, "Smoke")
	gid := 0
	if n := len(s.res.GrenadeDetonations); n > 0 {
		gid = s.res.GrenadeDetonations[n-1].GrenadeID
	}
	s.openSmoke(e.GrenadeEntityID, e.Position, gid)
}

// onSmokeExpired closes the cloud. The engine fires this when the smoke has
// completely faded, which is a better lifetime than any constant we would pick.
func (s *state) onSmokeExpired(e events.SmokeExpired) {
	idx, ok := s.smokeByEnt[e.GrenadeEntityID]
	if !ok || idx >= len(s.smokes) {
		return
	}
	tick := s.parser.GameState().IngameTick()
	s.smokes[idx].endTick = tick
	if x := s.smokes[idx].exportIdx; x >= 0 && x < len(s.res.SmokeVolumes) {
		s.res.SmokeVolumes[x].EndTick = tick
	}
	delete(s.smokeByEnt, e.GrenadeEntityID)
}

// openSmoke records a new cloud and computes its volume. The event position is
// stale or zeroed on some CS2 demos, so it prefers the projectile's own
// networked detonation point — m_vSmokeDetonationPos is the true centre of the
// game's voxel grid — then the position emitDetonate already tracks per frame.
func (s *state) openSmoke(entID int, eventPos r3.Vector, gid int) {
	center := eventPos
	if p := s.parser.GameState().GrenadeProjectiles()[entID]; p != nil && p.Entity != nil {
		if v, ok := p.Entity.PropertyValue("m_vSmokeDetonationPos"); ok {
			if d := v.R3VecOrNil(); d != nil && (d.X != 0 || d.Y != 0 || d.Z != 0) {
				center = *d
			}
		}
	}
	if center.X == 0 && center.Y == 0 && center.Z == 0 {
		if g, ok := s.grenadePos[entID]; ok {
			center = r3.Vector{X: float64(g.x), Y: float64(g.y), Z: float64(g.z)}
		}
	}
	if center.X == 0 && center.Y == 0 && center.Z == 0 {
		return
	}

	s.ensureMesh()
	tick := s.parser.GameState().IngameTick()
	vol, sealed := buildSmokeVolume(s.mesh, center)
	if sealed {
		s.smokeSealed++
	}

	cloud := smokeCloud{center: center, startTick: tick, vol: vol, exportIdx: -1}
	if vol != nil {
		cloud.exportIdx = len(s.res.SmokeVolumes)
		s.res.SmokeVolumes = append(s.res.SmokeVolumes, vol.export(gid, s.currentRound, tick))
		s.smokeVoxels += vol.count()
	}
	s.smokeByEnt[entID] = len(s.smokes)
	s.smokes = append(s.smokes, cloud)
	s.smokeOpened++
}

// smokeOccluded reports whether any cloud deployed at this tick sits across the
// segment from → to.
func (s *state) smokeOccluded(tick int, from, to r3.Vector) bool {
	if len(s.smokes) == 0 {
		return false
	}
	clr := smokeClearing{blasts: s.blastsAt(tick), holes: s.holesAt(tick, from, to)}
	if len(clr.blasts) > 0 {
		s.blastQueries++
	}
	if len(clr.holes) > 0 {
		s.holeQueries++
	}
	for i := range s.smokes {
		c := &s.smokes[i]
		r := c.bloomRadiusAt(tick, s.tickRate)
		if r <= 0 {
			continue
		}
		if c.vol != nil {
			if c.vol.occludedSegment(from, to, c.center, r, clr) {
				s.smokeBlocks++
				return true
			}
			// Would this sightline have been blocked without the explosions
			// and bullet tunnels? That is the honest measure of what they
			// change. A sightline still blocked with the blasts alone was
			// opened by a tunnel.
			if !clr.empty() && c.vol.occludedSegment(from, to, c.center, r, smokeClearing{}) {
				if len(clr.holes) > 0 && c.vol.occludedSegment(from, to, c.center, r, smokeClearing{blasts: clr.blasts}) {
					s.holeLetTh++
				} else {
					s.blastLetTh++
				}
			}
			continue
		}
		// No mesh for this map: fall back to a plain sphere. Without geometry
		// there is nothing to shape the cloud with, and an unshaped cloud still
		// beats pretending smoke is transparent.
		if segmentSphereIntersects(from, to, c.center, r) {
			s.smokeBlocks++
			return true
		}
	}
	return false
}

// segmentSphereIntersects reports whether the segment passes within radius of
// the centre. Only used on maps with no collision mesh.
func segmentSphereIntersects(from, to, center r3.Vector, radius float64) bool {
	d := r3.Vector{X: to.X - from.X, Y: to.Y - from.Y, Z: to.Z - from.Z}
	lenSq := d.X*d.X + d.Y*d.Y + d.Z*d.Z
	t := 0.0
	if lenSq > 1e-9 {
		t = ((center.X-from.X)*d.X + (center.Y-from.Y)*d.Y + (center.Z-from.Z)*d.Z) / lenSq
		t = math.Max(0, math.Min(1, t))
	}
	near := r3.Vector{X: from.X + d.X*t, Y: from.Y + d.Y*t, Z: from.Z + d.Z*t}
	dx, dy, dz := near.X-center.X, near.Y-center.Y, near.Z-center.Z
	return dx*dx+dy*dy+dz*dz <= radius*radius
}

// resetSmokes drops every cloud at round boundaries. Projectiles are removed
// between rounds without always firing SmokeExpired.
func (s *state) resetSmokes() {
	s.smokes = s.smokes[:0]
	s.smokeByEnt = map[int]int{}
	s.blasts = s.blasts[:0]
	s.holes = s.holes[:0]
}
