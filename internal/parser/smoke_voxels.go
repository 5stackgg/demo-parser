package parser

import (
	"container/heap"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/golang/geo/r3"
	st "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/sendtables"
)

// CS2 networks each smoke's own simulation on CSmokeGrenadeProjectile.
// m_VoxelFrameData is a byte array (m_nVoxelFrameDataSize of it in use) holding
// an append-only log the server extends every few ticks while the cloud grows:
//
//	frame   = u16 frame number, u16 payload length, payload
//	payload = u8 0, u8 flags,
//	          flags&1: u8 n, n × 8-byte source point (x, y, z, v, 4 × 0)
//	          flags&2: u16 n, n × (u16 brick index, u64 solid mask)
//	          u8 0
//
// The grid is 32³ voxels of 20 units whose minimum corner sits 320 units below
// m_vSmokeDetonationPos on every axis. It is split into 8³ bricks of 4³ voxels;
// a brick index is a Morton code over the brick grid and each mask bit, also
// Morton-ordered, marks one voxel of world geometry. The server sends a brick
// once its own cloud gets near it, so the set of bricks — and the frame each
// arrived in — traces where and when the real smoke spread.
//
// The client turns that into the cloud by flooding out from the source points
// through the free voxels until it holds a fixed volume — pushed into a
// corridor, a smoke runs further. This does the same in two passes:
//
//   - Shape: a breadth-first flood from the sources through free voxels of the
//     bricks the server sent, stopped at a voxel budget. The budget is fitted
//     per smoke: the server sends every brick its cloud reached or touched, so
//     the right size is the one whose one-voxel shell covers those bricks and
//     no others.
//   - Timing: when each voxel of that shape filled. It spreads one voxel per
//     step from the sources, and never into a brick before the server sent it,
//     so growth follows the server's own clock at brick resolution.
const (
	voxelGridDim  = 32
	voxelBrickDim = 4
	voxelBrickRow = voxelGridDim / voxelBrickDim
	voxelCells    = voxelGridDim * voxelGridDim * voxelGridDim
	voxelBricks   = voxelBrickRow * voxelBrickRow * voxelBrickRow
	voxelEdge     = 20.0
	voxelHalfSpan = voxelEdge * voxelGridDim / 2

	// voxelFrameDataSlots is the length of the networked array.
	voxelFrameDataSlots = 3072

	// arrPerSecond is the unit of an exported arrival time: 1/40 s, so a
	// byte covers 6.35 s — longer than any cloud takes to finish growing.
	arrPerSecond = 40
	arrMax       = 254
	arrNever     = 255
)

// Calibrated on test.dem's de_inferno smokes; see TestSmokeVoxelCalibration.
var defaultVoxelFlood = voxelFloodParams{
	budget:    800,
	budgetMin: 600,
	budgetMax: 1100,
	stepSecs:  6.0 / 64,
}

// voxelFloodParams shapes the reconstruction.
//
// budget is the cloud's volume in voxels when its bricks cannot tell sizes
// apart; budgetMin and budgetMax bound the per-smoke fit. stepSecs is how long
// the cloud takes to advance one voxel where the server's bricks do not hold
// it back: half of a cloud's bricks arrive on the detonation frame, and without
// a pace that half would appear at once.
type voxelFloodParams struct {
	budget    int
	budgetMin int
	budgetMax int
	stepSecs  float64
}

type voxelSource struct {
	X, Y, Z, V uint8
}

type voxelBrick struct {
	Index uint16
	Mask  uint64
}

type voxelFrame struct {
	Number  int
	Sources []voxelSource
	Bricks  []voxelBrick
}

var errVoxelFrame = errors.New("malformed voxel frame")

// decodeVoxelFrames decodes every complete frame in buf. next is the offset
// just past the last complete frame; a frame still being written is left for
// the next call.
func decodeVoxelFrames(buf []byte) (frames []voxelFrame, next int, err error) {
	for next+4 <= len(buf) {
		n := int(binary.LittleEndian.Uint16(buf[next:]))
		size := int(binary.LittleEndian.Uint16(buf[next+2:]))
		if next+4+size > len(buf) {
			break
		}
		f, err := decodeVoxelPayload(n, buf[next+4:next+4+size])
		if err != nil {
			return frames, next, fmt.Errorf("frame %d at %d: %w", n, next, err)
		}
		frames = append(frames, f)
		next += 4 + size
	}
	return frames, next, nil
}

func decodeVoxelPayload(n int, p []byte) (voxelFrame, error) {
	f := voxelFrame{Number: n}
	if len(p) < 2 {
		return f, errVoxelFrame
	}
	flags := p[1]
	at := 2
	if flags&1 != 0 {
		if at+1 > len(p) {
			return f, errVoxelFrame
		}
		count := int(p[at])
		at++
		if at+count*8 > len(p) {
			return f, errVoxelFrame
		}
		f.Sources = make([]voxelSource, count)
		for i := range f.Sources {
			r := p[at+i*8:]
			f.Sources[i] = voxelSource{X: r[0], Y: r[1], Z: r[2], V: r[3]}
		}
		at += count * 8
	}
	if flags&2 != 0 {
		if at+2 > len(p) {
			return f, errVoxelFrame
		}
		count := int(binary.LittleEndian.Uint16(p[at:]))
		at += 2
		if at+count*10 > len(p) {
			return f, errVoxelFrame
		}
		f.Bricks = make([]voxelBrick, count)
		for i := range f.Bricks {
			r := p[at+i*10:]
			f.Bricks[i] = voxelBrick{
				Index: binary.LittleEndian.Uint16(r),
				Mask:  binary.LittleEndian.Uint64(r[2:]),
			}
		}
	}
	return f, nil
}

// deinterleave3 splits a Morton code into its x, y and z components, x in the
// lowest bit.
func deinterleave3(code, bits int) (x, y, z int) {
	for b := 0; b < bits; b++ {
		x |= (code >> (3 * b) & 1) << b
		y |= (code >> (3*b + 1) & 1) << b
		z |= (code >> (3*b + 2) & 1) << b
	}
	return x, y, z
}

// brickCoord is a brick's position in the 8³ brick grid.
func brickCoord(index uint16) (x, y, z int) {
	return deinterleave3(int(index)&(voxelBricks-1), 3)
}

// maskVoxel is the voxel a mask bit covers, within its 4³ brick.
func maskVoxel(bit int) (x, y, z int) {
	return deinterleave3(bit, 2)
}

func voxelIndex(x, y, z int) int {
	return (z*voxelGridDim+y)*voxelGridDim + x
}

func brickOf(x, y, z int) int {
	bx, by, bz := x/voxelBrickDim, y/voxelBrickDim, z/voxelBrickDim
	return (bz*voxelBrickRow+by)*voxelBrickRow + bx
}

// voxelLog is everything one smoke networked, with the tick each brick arrived.
type voxelLog struct {
	sources []voxelSource
	// brickTick is the tick, relative to detonation, each brick arrived; -1
	// for bricks the server never sent.
	brickTick [voxelBricks]int
	solid     [voxelCells]bool
	frames    int
}

func newVoxelLog() *voxelLog {
	l := &voxelLog{}
	for i := range l.brickTick {
		l.brickTick[i] = -1
	}
	return l
}

// apply folds a decoded frame in, as having arrived at tick (relative to
// detonation).
func (l *voxelLog) apply(f voxelFrame, tick int) {
	if tick < 0 {
		tick = 0
	}
	l.frames++
	if len(f.Sources) > 0 && len(l.sources) == 0 {
		l.sources = f.Sources
	}
	for _, b := range f.Bricks {
		bx, by, bz := brickCoord(b.Index)
		idx := brickOf(bx*voxelBrickDim, by*voxelBrickDim, bz*voxelBrickDim)
		if l.brickTick[idx] < 0 {
			l.brickTick[idx] = tick
		}
		for bit := 0; bit < 64; bit++ {
			if b.Mask>>bit&1 == 0 {
				continue
			}
			lx, ly, lz := maskVoxel(bit)
			l.solid[voxelIndex(bx*voxelBrickDim+lx, by*voxelBrickDim+ly, bz*voxelBrickDim+lz)] = true
		}
	}
}

func voxelCoord(i int) (x, y, z int) {
	return i % voxelGridDim, i / voxelGridDim % voxelGridDim, i / (voxelGridDim * voxelGridDim)
}

// open reports whether the cloud may occupy a voxel: inside the grid, in a
// brick the server sent, and not world geometry.
func (l *voxelLog) open(x, y, z int) bool {
	if x < 0 || y < 0 || z < 0 || x >= voxelGridDim || y >= voxelGridDim || z >= voxelGridDim {
		return false
	}
	return l.brickTick[brickOf(x, y, z)] >= 0 && !l.solid[voxelIndex(x, y, z)]
}

// fillOrder is up to limit voxels in the order a breadth-first flood from the
// sources reaches them. Any prefix of it is the cloud at that volume.
func (l *voxelLog) fillOrder(limit int) []int {
	seen := make([]bool, voxelCells)
	order := make([]int, 0, limit)
	for _, s := range l.sources {
		x, y, z := int(s.X), int(s.Y), int(s.Z)
		if !l.open(x, y, z) {
			continue
		}
		if i := voxelIndex(x, y, z); !seen[i] {
			seen[i] = true
			order = append(order, i)
		}
	}
	for head := 0; head < len(order) && len(order) < limit; head++ {
		x, y, z := voxelCoord(order[head])
		for _, d := range smokeNeighbors {
			nx, ny, nz := x+d[0], y+d[1], z+d[2]
			if !l.open(nx, ny, nz) {
				continue
			}
			if n := voxelIndex(nx, ny, nz); !seen[n] {
				seen[n] = true
				order = append(order, n)
				if len(order) == limit {
					break
				}
			}
		}
	}
	return order
}

// brickCover scores a growing cloud against the bricks the server sent. The
// server sends every brick its cloud occupied or bordered, so the comparison is
// with the bricks ours occupies or borders: intersection over union.
type brickCover struct {
	log       *voxelLog
	touched   [voxelBricks]bool
	sent      int
	predicted int
	hit       int
}

func newBrickCover(l *voxelLog) *brickCover {
	c := &brickCover{log: l}
	for _, t := range l.brickTick {
		if t >= 0 {
			c.sent++
		}
	}
	return c
}

func (c *brickCover) add(cell int) {
	x, y, z := voxelCoord(cell)
	c.touch(x, y, z)
	for _, d := range smokeNeighbors {
		c.touch(x+d[0], y+d[1], z+d[2])
	}
}

func (c *brickCover) touch(x, y, z int) {
	if x < 0 || y < 0 || z < 0 || x >= voxelGridDim || y >= voxelGridDim || z >= voxelGridDim {
		return
	}
	b := brickOf(x, y, z)
	if c.touched[b] {
		return
	}
	c.touched[b] = true
	c.predicted++
	if c.log.brickTick[b] >= 0 {
		c.hit++
	}
}

func (c *brickCover) iou() float64 {
	union := c.predicted + c.sent - c.hit
	if union == 0 {
		return 0
	}
	return float64(c.hit) / float64(union)
}

func (l *voxelLog) brickIoU(cells []int) float64 {
	c := newBrickCover(l)
	for _, cell := range cells {
		c.add(cell)
	}
	return c.iou()
}

// fitBudget picks how much of order is the cloud: the volume within the bounds
// that best matches the bricks the server sent, nearest the default on a tie.
func (l *voxelLog) fitBudget(order []int, p voxelFloodParams) (budget int, iou float64) {
	if len(order) <= p.budgetMin {
		return len(order), l.brickIoU(order)
	}
	c := newBrickCover(l)
	best, bestDist := -1.0, 0
	for i, cell := range order {
		c.add(cell)
		n := i + 1
		if n < p.budgetMin {
			continue
		}
		if n > p.budgetMax {
			break
		}
		score := c.iou()
		dist := n - p.budget
		if dist < 0 {
			dist = -dist
		}
		if score > best+1e-9 || (score > best-1e-9 && dist < bestDist) {
			best, bestDist, budget = score, dist, n
		}
	}
	return budget, best
}

// timeShape is when each voxel of the cloud filled, in ticks after
// detonation, or -1 outside it: a Dijkstra from the sources over the cloud
// where a step costs stepTicks and no voxel fills before its brick arrived.
func (l *voxelLog) timeShape(cells []int, stepTicks int) []int {
	in := make([]bool, voxelCells)
	for _, c := range cells {
		in[c] = true
	}
	arrival := make([]int, voxelCells)
	best := make([]int, voxelCells)
	for i := range arrival {
		arrival[i] = -1
		best[i] = math.MaxInt
	}
	q := &voxelQueue{}
	push := func(cell, tick int) {
		if b := l.brickTick[brickOf(voxelCoord(cell))]; b > tick {
			tick = b
		}
		if tick < best[cell] {
			best[cell] = tick
			heap.Push(q, voxelEntry{tick: tick, cell: cell, seq: q.seq})
			q.seq++
		}
	}
	for _, s := range l.sources {
		if i := voxelIndex(int(s.X), int(s.Y), int(s.Z)); in[i] {
			push(i, 0)
		}
	}
	for q.Len() > 0 {
		e := heap.Pop(q).(voxelEntry)
		if arrival[e.cell] >= 0 {
			continue
		}
		arrival[e.cell] = e.tick
		x, y, z := voxelCoord(e.cell)
		for _, d := range smokeNeighbors {
			nx, ny, nz := x+d[0], y+d[1], z+d[2]
			if nx < 0 || ny < 0 || nz < 0 || nx >= voxelGridDim || ny >= voxelGridDim || nz >= voxelGridDim {
				continue
			}
			if n := voxelIndex(nx, ny, nz); in[n] && arrival[n] < 0 {
				push(n, e.tick+stepTicks)
			}
		}
	}
	return arrival
}

// flood reconstructs the cloud: ticks after detonation each voxel filled, -1
// for voxels it never reached.
func (l *voxelLog) flood(p voxelFloodParams, tickRate float64) []int {
	order := l.fillOrder(p.budgetMax)
	budget, _ := l.fitBudget(order, p)
	return l.timeShape(order[:budget], int(math.Round(p.stepSecs*tickRate)))
}

type voxelEntry struct {
	tick int
	cell int
	// seq breaks ties first-in first-out, so the queue pops deterministically.
	seq int
}

type voxelQueue struct {
	items []voxelEntry
	seq   int
}

func (q *voxelQueue) Len() int { return len(q.items) }
func (q *voxelQueue) Less(i, j int) bool {
	if q.items[i].tick != q.items[j].tick {
		return q.items[i].tick < q.items[j].tick
	}
	return q.items[i].seq < q.items[j].seq
}
func (q *voxelQueue) Swap(i, j int) { q.items[i], q.items[j] = q.items[j], q.items[i] }
func (q *voxelQueue) Push(x any)    { q.items = append(q.items, x.(voxelEntry)) }
func (q *voxelQueue) Pop() any {
	n := len(q.items) - 1
	e := q.items[n]
	q.items = q.items[:n]
	return e
}

// exportVoxels trims the flood to its bounding box and packs it for the blob.
// ok is false when nothing filled.
func exportVoxels(arrival []int, det r3.Vector, tickRate float64) (out EventSmokeVoxels, ok bool) {
	lo := [3]int{voxelGridDim, voxelGridDim, voxelGridDim}
	hi := [3]int{-1, -1, -1}
	for i, t := range arrival {
		if t < 0 {
			continue
		}
		x, y, z := voxelCoord(i)
		c := [3]int{x, y, z}
		for a := 0; a < 3; a++ {
			lo[a] = min(lo[a], c[a])
			hi[a] = max(hi[a], c[a])
		}
	}
	if hi[0] < 0 {
		return out, false
	}
	dx, dy, dz := hi[0]-lo[0]+1, hi[1]-lo[1]+1, hi[2]-lo[2]+1
	packed := make([]byte, dx*dy*dz)
	for k := 0; k < dz; k++ {
		for j := 0; j < dy; j++ {
			for i := 0; i < dx; i++ {
				v := byte(arrNever)
				if t := arrival[voxelIndex(lo[0]+i, lo[1]+j, lo[2]+k)]; t >= 0 {
					v = byte(min(arrMax, int(math.Round(float64(t)*arrPerSecond/tickRate))))
				}
				packed[(k*dy+j)*dx+i] = v
			}
		}
	}
	out.OriginX = float32(det.X - voxelHalfSpan + float64(lo[0])*voxelEdge)
	out.OriginY = float32(det.Y - voxelHalfSpan + float64(lo[1])*voxelEdge)
	out.OriginZ = float32(det.Z - voxelHalfSpan + float64(lo[2])*voxelEdge)
	out.VoxelSize = voxelEdge
	out.DimX, out.DimY, out.DimZ = dx, dy, dz
	out.Arrival = base64.StdEncoding.EncodeToString(packed)
	return out, true
}

// voxelFrameDataNames are the per-element property names of m_VoxelFrameData,
// built once rather than formatted on every read.
var voxelFrameDataNames = func() []string {
	names := make([]string, voxelFrameDataSlots)
	for i := range names {
		names[i] = fmt.Sprintf("m_VoxelFrameData.%04d", i)
	}
	return names
}()

func propUint(v st.PropertyValue) (uint64, bool) {
	switch x := v.Any.(type) {
	case uint64:
		return x, true
	case uint32:
		return uint64(x), true
	case uint16:
		return uint64(x), true
	case uint8:
		return uint64(x), true
	case int64:
		return uint64(x), x >= 0
	case int32:
		return uint64(x), x >= 0
	}
	return 0, false
}

// smokeVoxelTrack reads one smoke's log off its projectile as it grows.
type smokeVoxelTrack struct {
	entity    st.Entity
	gid       int
	round     int
	startTick int
	det       r3.Vector

	buf  []byte
	next int
	log  *voxelLog
	// broken stops reading a log that failed to decode, keeping what arrived
	// before it.
	broken bool
}

func newSmokeVoxelTrack(entity st.Entity, gid, round, startTick int) *smokeVoxelTrack {
	return &smokeVoxelTrack{entity: entity, gid: gid, round: round, startTick: startTick, log: newVoxelLog()}
}

// poll reads whatever the server appended since the last call and decodes the
// frames it completed, stamping them with tick.
func (t *smokeVoxelTrack) poll(tick int) {
	if v, ok := t.entity.PropertyValue("m_vSmokeDetonationPos"); ok {
		if d := v.R3VecOrNil(); d != nil && (d.X != 0 || d.Y != 0 || d.Z != 0) {
			t.det = *d
		}
	}
	if t.broken {
		return
	}
	v, ok := t.entity.PropertyValue("m_nVoxelFrameDataSize")
	if !ok {
		return
	}
	size64, ok := propUint(v)
	if !ok {
		return
	}
	size := min(int(size64), voxelFrameDataSlots)
	if size <= len(t.buf) {
		return
	}
	for i := len(t.buf); i < size; i++ {
		b, _ := t.entity.PropertyValue(voxelFrameDataNames[i])
		u, _ := propUint(b)
		t.buf = append(t.buf, byte(u))
	}
	frames, n, err := decodeVoxelFrames(t.buf[t.next:])
	t.next += n
	for _, f := range frames {
		t.log.apply(f, tick-t.startTick)
	}
	if err != nil {
		t.broken = true
	}
}

// export reconstructs and packs the cloud. ok is false when the smoke
// networked nothing to reconstruct from.
func (t *smokeVoxelTrack) export(endTick int, tickRate float64, p voxelFloodParams) (EventSmokeVoxels, bool) {
	if len(t.log.sources) == 0 || (t.det.X == 0 && t.det.Y == 0 && t.det.Z == 0) {
		return EventSmokeVoxels{}, false
	}
	if tickRate <= 0 {
		tickRate = 64
	}
	out, ok := exportVoxels(t.log.flood(p, tickRate), t.det, tickRate)
	if !ok {
		return out, false
	}
	out.GrenadeID = t.gid
	out.Round = t.round
	out.StartTick = t.startTick
	out.EndTick = endTick
	return out, true
}

// openSmokeVoxels starts reading a smoke's voxel log. It is keyed by entity
// alongside the cloud, and closed when the cloud is.
func (s *state) openSmokeVoxels(entID, gid int) {
	ent := s.parser.GameState().Entities()[entID]
	if ent == nil {
		return
	}
	tick := s.parser.GameState().IngameTick()
	s.closeSmokeVoxels(entID, tick)
	s.voxelTracks[entID] = newSmokeVoxelTrack(ent, gid, s.currentRound, tick)
}

// pollSmokeVoxels runs once per frame. A projectile that is gone without a
// SmokeExpired — removed at a round transition — closes its log here.
func (s *state) pollSmokeVoxels() {
	if len(s.voxelTracks) == 0 {
		return
	}
	tick := s.parser.GameState().IngameTick()
	ents := s.parser.GameState().Entities()
	for id, t := range s.voxelTracks {
		if ents[id] != t.entity {
			s.closeSmokeVoxels(id, tick)
			continue
		}
		t.poll(tick)
	}
}

func (s *state) closeSmokeVoxels(entID, endTick int) {
	t, ok := s.voxelTracks[entID]
	if !ok {
		return
	}
	delete(s.voxelTracks, entID)
	out, ok := t.export(endTick, s.tickRate, defaultVoxelFlood)
	if !ok {
		s.voxelSkipped++
		return
	}
	s.res.SmokeVoxels = append(s.res.SmokeVoxels, out)
	s.voxelBoxCells += out.DimX * out.DimY * out.DimZ
}

// closeAllSmokeVoxels flushes every log still open, at a round transition or
// the end of the demo.
func (s *state) closeAllSmokeVoxels(tick int) {
	for id := range s.voxelTracks {
		s.closeSmokeVoxels(id, tick)
	}
}

// sortSmokeVoxels puts the clouds in detonation order. They are appended as
// they close, which is expiry order.
func (s *state) sortSmokeVoxels() {
	sort.SliceStable(s.res.SmokeVoxels, func(i, j int) bool {
		a, b := s.res.SmokeVoxels[i], s.res.SmokeVoxels[j]
		if a.StartTick != b.StartTick {
			return a.StartTick < b.StartTick
		}
		return a.GrenadeID < b.GrenadeID
	})
}
