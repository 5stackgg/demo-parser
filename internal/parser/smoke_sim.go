package parser

import (
	"math"
	"slices"
)

// The game does not network a smoke's cloud; it networks what its own
// simulation starts from — the emitter voxels and the world geometry around
// them — and server and client each run the same simulation from there. This
// is that simulation, SmokeVolume in the game's code, one step per networked
// frame (every 0.1 s):
//
//   - Emit: the first step is a burst, every emitter half filling the open
//     voxels around it. After that each emitter adds density to its own voxel,
//     up to a cap; one more than 80 units from the detonation stays off until
//     the cloud reaches it, then ramps up.
//   - Flow: every voxel the cloud has reached pulls density from its six
//     neighbours — more from the one above than the one below, so the cloud
//     sinks; more from a neighbour whose density is moving its way — and
//     wakes the neighbours once it holds enough. Each voxel then loses a
//     little, which is what bounds the cloud.
//
// Everything is in the game's own order and single precision: the flow reads
// the previous step's densities, but each voxel's intake depends on what
// earlier voxels in the pass already took from it. Products are rounded
// before they are summed, as the game's are, so a fused multiply-add cannot
// change a result.
const (
	simStepSecs = 0.1

	simEmitRate    = 60
	simBurstShare  = 0.5
	simFullDensity = 50
	simNearEmitter = 80
	simEmitterRamp = 0.8
	simEmitterOn   = 0.01
	simEmitterSlot = 40
	simEarlyBoost  = 3.5
	simReached     = 0.2

	simPullBelow  = 0.79
	simPullAbove  = 1.2
	simPullToward = 1.25
	simPullAside  = 0.9
	simToward     = 0.2
	simWake       = 5
	simEdgeKeep   = 0.3
	simLoss       = 0.5
)

// simSix are a voxel's neighbours in the order the game visits them; the
// fifth is the one below and the sixth the one above.
var simSix = [6][3]float32{{-1, 0, 0}, {1, 0, 0}, {0, -1, 0}, {0, 1, 0}, {0, 0, -1}, {0, 0, 1}}

const (
	simBelow = 4
	simAbove = 5
)

// The game indexes voxels by Morton code and updates them in that order.
// voxelOfMorton is the row-major index of each.
var voxelOfMorton [voxelCells]uint16

func init() {
	for m := range voxelOfMorton {
		voxelOfMorton[m] = uint16(voxelIndex(deinterleave3(m, 5)))
	}
}

// interleave3 is deinterleave3's inverse over the voxel grid. The flow asks
// it for six neighbours of every voxel every step, so the bit spreading is a
// table rather than a loop.
func interleave3(x, y, z int) int {
	return mortonSpread[x&31] | mortonSpread[y&31]<<1 | mortonSpread[z&31]<<2
}

// mortonSpread[v] is v's five bits moved three apart: v's lane of a Morton
// code.
var mortonSpread = func() (t [32]int) {
	for v := range t {
		for b := 0; b < 5; b++ {
			t[v] |= (v >> b & 1) << (3 * b)
		}
	}
	return t
}()

type simCell struct {
	density float32
	// flow is the unit direction density is moving in, by the last step.
	flow [3]float32
}

type simEmitter struct {
	x, y, z int
	// hold is how far the emitter is held back: 0 emits in full.
	hold float32
}

// smokeSim is one smoke's simulation. Its arrays are indexed by Morton code.
type smokeSim struct {
	origin [3]float32
	solid  [voxelCells]bool
	// reached and cell are double-buffered the way the game's are: a step
	// reads cur and writes the other, then they swap.
	reached [2][voxelCells]bool
	cell    [2][voxelCells]simCell
	cur     int
	// active is every voxel reached before this step, ascending — the voxels
	// the step's passes visit, in the order they visited them when they
	// walked the whole grid. A cloud reaches a few thousand of the 32768, and
	// nothing else holds any density to move. emitted and woke are the voxels
	// reached during this step, by emission and by flow, until they join it.
	active   []uint16
	emitted  []uint16
	woke     []uint16
	emitters []simEmitter
	// started is false until the first step, which is the burst.
	started bool

	// touched is called with each brick the step reads geometry from.
	touched func(brick int)
}

func newSmokeSim(origin [3]float32) *smokeSim {
	return &smokeSim{origin: origin}
}

func sqrt32(v float32) float32 { return float32(math.Sqrt(float64(v))) }

func unit32(v float32) float32 {
	if !(v >= 0) {
		return 0
	}
	if v <= 1 {
		return v
	}
	return 1
}

// center is a voxel's centre in the world.
func (s *smokeSim) center(x, y, z int) (float32, float32, float32) {
	return float32(float32(x-16)*voxelEdge) + s.origin[0] + 10,
		float32(float32(y-16)*voxelEdge) + s.origin[1] + 10,
		float32(float32(z-16)*voxelEdge) + s.origin[2] + 10
}

// at is a neighbour's Morton code; ok is false outside the grid. The game
// builds a brick's geometry the first time anything looks into it.
func (s *smokeSim) at(x, y, z int) (m int, ok bool) {
	if x < 0 || y < 0 || z < 0 || x >= voxelGridDim || y >= voxelGridDim || z >= voxelGridDim {
		return 0, false
	}
	m = interleave3(x, y, z)
	if s.touched != nil {
		s.touched(m >> 6)
	}
	return m, true
}

func (s *smokeSim) step() {
	s.emit()
	cur, oth := s.cur, 1-s.cur
	// Emission reached these in cur, so the flow visits them this step.
	s.active = mergeSorted(s.active, s.emitted)
	for _, m := range s.active {
		s.flow(int(m))
	}
	for _, m := range s.active {
		s.cell[cur][m] = s.cell[oth][m]
	}
	// Both buffers end the step reached wherever either was: emission marked
	// cur, the flow marked oth.
	for _, m := range s.emitted {
		s.reached[oth][m] = true
	}
	for _, m := range s.woke {
		s.reached[cur][m] = true
	}
	s.active = mergeSorted(s.active, s.woke)
	s.emitted, s.woke = s.emitted[:0], s.woke[:0]
	s.cur = oth
}

// reach marks voxel m reached in buffer b, noting it in list the first time
// it is reached at all. A voxel neither buffer has reached is one neither the
// last step nor this one has touched yet.
func (s *smokeSim) reach(b, m int, list *[]uint16) {
	if !s.reached[0][m] && !s.reached[1][m] {
		*list = append(*list, uint16(m))
	}
	s.reached[b][m] = true
}

// mergeSorted adds add, in any order and with no voxel already in into, to the
// ascending into.
func mergeSorted(into, add []uint16) []uint16 {
	if len(add) == 0 {
		return into
	}
	slices.Sort(add)
	n := len(into)
	into = slices.Grow(into, len(add))[:n+len(add)]
	i, j, k := n-1, len(add)-1, len(into)-1
	for j >= 0 {
		if i >= 0 && into[i] > add[j] {
			into[k] = into[i]
			i--
		} else {
			into[k] = add[j]
			j--
		}
		k--
	}
	return into
}

// simAround are a voxel's 26 neighbours in the order the game visits them.
var simAround = [26][3]int{
	{-1, 0, 0}, {-1, 1, 0}, {0, 1, 0}, {1, 1, 0}, {1, 0, 0}, {1, -1, 0}, {0, -1, 0}, {-1, -1, 0},
	{-1, 0, 1}, {-1, 1, 1}, {0, 1, 1}, {1, 1, 1}, {1, 0, 1}, {1, -1, 1}, {0, -1, 1}, {-1, -1, 1}, {0, 0, 1},
	{-1, 0, -1}, {-1, 1, -1}, {0, 1, -1}, {1, 1, -1}, {1, 0, -1}, {1, -1, -1}, {0, -1, -1}, {-1, -1, -1}, {0, 0, -1},
}

// burst is the first step's emission: every emitter, near or far, half fills
// the open voxels around it at once.
func (s *smokeSim) burst() {
	cur, oth := s.cur, 1-s.cur
	for i := range s.emitters {
		e := &s.emitters[i]
		if e.x >= voxelGridDim || e.y >= voxelGridDim || e.z >= voxelGridDim {
			continue
		}
		for _, d := range simAround {
			nx, ny, nz := e.x+d[0], e.y+d[1], e.z+d[2]
			if nx < 0 || ny < 0 || nz < 0 || nx >= voxelGridDim || ny >= voxelGridDim || nz >= voxelGridDim {
				continue
			}
			n := interleave3(nx, ny, nz)
			if s.reached[cur][n] || s.solid[n] {
				continue
			}
			density := float32(simEmitRate*simBurstShare) + s.cell[cur][n].density
			s.cell[cur][n].density = density
			s.cell[oth][n].density = density
			s.reach(cur, n, &s.emitted)
		}
		s.reach(cur, interleave3(e.x, e.y, e.z), &s.emitted)
	}
}

func (s *smokeSim) emit() {
	if !s.started {
		s.started = true
		s.burst()
		return
	}
	if len(s.emitters) == 0 {
		return
	}
	on := 0
	for i := range s.emitters {
		if s.emitters[i].hold < simEmitterOn {
			on++
		}
	}
	// The fewer emitters are running, the more each may hold.
	boost := float32(float32(1-float32(on)/simEmitterSlot) * simEarlyBoost)
	if boost <= 1 {
		boost = 1
	}
	cur, oth := s.cur, 1-s.cur
	for i := range s.emitters {
		e := &s.emitters[i]
		if e.x >= voxelGridDim || e.y >= voxelGridDim || e.z >= voxelGridDim {
			continue
		}
		m := interleave3(e.x, e.y, e.z)
		c := &s.cell[cur][m]
		px, py, pz := s.center(e.x, e.y, e.z)
		dx, dy, dz := px-s.origin[0], py-s.origin[1], pz-s.origin[2]
		hold := e.hold
		var share float32
		switch {
		case sqrt32(float32(dz*dz)+float32(dy*dy)+float32(dx*dx)) <= simNearEmitter:
			e.hold = float32(hold * simEmitterRamp)
			share = 1 - e.hold
		case s.around(e.x, e.y, e.z)/6 <= simReached:
			e.hold = 1
			share = 0
		case hold <= 0:
			share = 1 - hold
		default:
			e.hold = float32(hold * simEmitterRamp)
			share = 1 - e.hold
		}
		density := float32(share*simEmitRate) + c.density
		if most := float32(simFullDensity * boost); density > most {
			density = most
		}
		c.density = density
		s.reach(cur, m, &s.emitted)
		s.cell[oth][m] = *c
	}
}

// around is how full an emitter's six neighbours are, each 0 to 1.
func (s *smokeSim) around(x, y, z int) float32 {
	var sum float32
	for _, d := range [6][3]int{{-1, 0, 0}, {1, 0, 0}, {0, -1, 0}, {0, 1, 0}, {0, 0, -1}, {0, 0, 1}} {
		nx, ny, nz := x+d[0], y+d[1], z+d[2]
		if nx < 0 || ny < 0 || nz < 0 || nx >= voxelGridDim || ny >= voxelGridDim || nz >= voxelGridDim {
			continue
		}
		n := interleave3(nx, ny, nz)
		if s.reached[s.cur][n] {
			sum += unit32(s.cell[s.cur][n].density / simFullDensity)
		}
	}
	return sum
}

func (s *smokeSim) flow(m int) {
	cur, oth := s.cur, 1-s.cur
	x, y, z := deinterleave3(m, 5)
	me := &s.cell[oth][m]
	if s.solid[m] {
		me.density = 0
		return
	}
	room := 1 - unit32(me.density/simFullDensity)
	held := s.cell[cur][m].density

	var beside [6]float32
	var walls [6][3]float32
	blocked := 0
	var taken float32
	for k, d := range simSix {
		n, ok := s.at(x+int(d[0]), y+int(d[1]), z+int(d[2]))
		if !ok || s.solid[n] {
			walls[blocked] = [3]float32{-d[0], -d[1], -d[2]}
			blocked++
			continue
		}
		from := &s.cell[cur][n]
		beside[k] = from.density
		var pull float32
		switch k {
		case simBelow:
			pull = simPullBelow
		case simAbove:
			pull = simPullAbove
		default:
			toward := float32(float32(-d[1]*from.flow[1])-float32(d[2]*from.flow[2])) - float32(d[0]*from.flow[0])
			if toward <= simToward {
				pull = simPullAside
			} else {
				pull = simPullToward
			}
		}
		take := float32(float32(room*pull) * float32(from.density/6))
		left := s.cell[oth][n].density - take
		taken += take
		if !(left >= 0) {
			left = 0
		}
		s.cell[oth][n].density = left
		if !s.reached[cur][n] && held > simWake {
			s.reach(oth, n, &s.woke)
		}
	}

	if x == 0 || y == 0 || z == 0 || x == voxelGridDim-1 || y == voxelGridDim-1 || z == voxelGridDim-1 {
		me.density = me.density * simEdgeKeep
		return
	}
	me.density += taken

	// Density moves from the fuller side to the emptier one, along any wall.
	gx, gy, gz := beside[0]-beside[1], beside[2]-beside[3], float32(0)
	for _, w := range walls[:blocked] {
		into := -(float32(w[2]*gz) + float32(gy*w[1]) + float32(gx*w[0]))
		gz += float32(into * w[2])
		gx += float32(into * w[0])
		gy += float32(into * w[1])
	}
	if length := sqrt32(float32(gz*gz) + float32(gy*gy) + float32(gx*gx)); length != 0 {
		inv := 1 / length
		me.flow = [3]float32{float32(inv * gx), float32(inv * gy), float32(inv * gz)}
	} else {
		me.flow = [3]float32{}
	}

	me.density -= simLoss
	if !(me.density > 0) {
		me.density = 0
	}
}
