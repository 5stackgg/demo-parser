package parser

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"

	"github.com/golang/geo/r3"
	st "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/sendtables"
)

// CS2 networks each smoke's own simulation on CSmokeGrenadeProjectile.
// m_VoxelFrameData is a byte array (m_nVoxelFrameDataSize of it in use) holding
// an append-only log, one frame for every step the server's simulation takes
// (ten a second):
//
//	frame   = u16 frame number, u16 payload length, payload
//	payload = u8 fading, u8 flags,
//	          flags&1: u8 n, n × emitter (u8 x, y, z, level, f32 hold)
//	          flags&2: u16 n, n × (u16 brick index, u64 solid mask)
//	          u8 n, n × 20-byte hole
//
// The grid is 32³ voxels of 20 units whose minimum corner sits 320 units below
// m_vSmokeDetonationPos on every axis. It is split into 8³ bricks of 4³ voxels;
// a brick index is a Morton code over the brick grid and each mask bit, also
// Morton-ordered, marks one voxel of world geometry.
//
// None of that is the cloud. The emitters are where the server decided the
// smoke comes out — around the detonation, and down to the floor below when it
// goes off on a ledge — and a brick is sent the first time the simulation
// looks into it. The client runs the same simulation from the same emitters
// and geometry, and so does this (smokeSim), a step for every frame; the cloud
// is where it ends up holding density.
//
// Holes, which a grenade or bullets punch in the cloud, are not replayed.
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
	// byte covers 6.35 s; the little that fills later is clamped to that.
	arrPerSecond = 40
	arrMax       = 254
	arrNever     = 255

	// smokeVisible is the density a voxel must hold to be part of the cloud:
	// the game's own line for a point being in smoke
	// (inferno_smoke_volume_density, 0.05 of a full voxel).
	smokeVisible = 0.05 * simFullDensity

	// smokeSettleSteps is the most steps of a log that are replayed: ten
	// seconds, by when a cloud has stopped growing.
	smokeSettleSteps = 100
)

type voxelSource struct {
	X, Y, Z, V uint8
	// Hold is how far the emitter is held back, 0 for one emitting in full.
	Hold float32
}

type voxelBrick struct {
	Index uint16
	Mask  uint64
}

type voxelFrame struct {
	Number int
	// Fading is set once the smoke has stopped emitting, near its end.
	Fading  bool
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
	f.Fading = p[0] != 0
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
			f.Sources[i] = voxelSource{X: r[0], Y: r[1], Z: r[2], V: r[3], Hold: math.Float32frombits(binary.LittleEndian.Uint32(r[4:]))}
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

func voxelCoord(i int) (x, y, z int) {
	return i % voxelGridDim, i / voxelGridDim % voxelGridDim, i / (voxelGridDim * voxelGridDim)
}

// voxelStep is what one frame told the simulation.
type voxelStep struct {
	// tick is when the frame arrived, relative to detonation.
	tick int
	// emitters replaces the emitters when the frame carried any: the first
	// frame's, and any the server moved since.
	emitters []simEmitter
	fading   bool
}

// voxelLog is everything one smoke networked. Its arrays are indexed by
// Morton code, the way the log and the simulation index them.
type voxelLog struct {
	steps []voxelStep
	solid [voxelCells]bool
	// sent are the bricks the server sent, which are the ones its simulation
	// looked into.
	sent [voxelBricks]bool

	// next is the offset just past the last frame read decoded.
	next int
	// broken stops reading a log that failed to decode, keeping what arrived
	// before it.
	broken bool
}

func newVoxelLog() *voxelLog {
	return &voxelLog{}
}

// read decodes the frames buf, the raw log so far, completed since the last
// call, stamping them with tick (relative to detonation). The demo and the
// live endpoint both read through here, so the same reads give the same cloud.
func (l *voxelLog) read(buf []byte, tick int) {
	if l.broken {
		return
	}
	frames, n, err := decodeVoxelFrames(buf[l.next:])
	l.next += n
	for _, f := range frames {
		l.apply(f, tick)
	}
	if err != nil {
		l.broken = true
	}
}

// apply folds a decoded frame in, as having arrived at tick (relative to
// detonation).
func (l *voxelLog) apply(f voxelFrame, tick int) {
	if tick < 0 {
		tick = 0
	}
	step := voxelStep{tick: tick, fading: f.Fading}
	if len(f.Sources) > 0 {
		step.emitters = make([]simEmitter, len(f.Sources))
		for i, s := range f.Sources {
			step.emitters[i] = simEmitter{x: int(s.X), y: int(s.Y), z: int(s.Z), hold: s.Hold}
		}
	}
	l.steps = append(l.steps, step)
	for _, b := range f.Bricks {
		brick := int(b.Index) & (voxelBricks - 1)
		l.sent[brick] = true
		for bit := 0; bit < 64; bit++ {
			if b.Mask>>bit&1 != 0 {
				l.solid[brick<<6|bit] = true
			}
		}
	}
}

// simulation is the game's simulation set up from the log. The game only
// reads geometry out of bricks it has sent, so what an unsent one holds never
// matters to a faithful replay; it counts as wall so that nothing can spread
// where nothing says it can.
func (l *voxelLog) simulation(det r3.Vector) *smokeSim {
	sim := newSmokeSim([3]float32{float32(det.X), float32(det.Y), float32(det.Z)})
	for m := range sim.solid {
		sim.solid[m] = l.solid[m] || !l.sent[m>>6]
	}
	return sim
}

// advance takes the simulation's step n, fed what frame n said. ok is false
// when the smoke had started to fade, which ends the cloud's growth.
func (l *voxelLog) advance(sim *smokeSim, n int) (ok bool) {
	step := l.steps[n]
	if step.fading {
		return false
	}
	if step.emitters != nil {
		sim.emitters = append(sim.emitters[:0], step.emitters...)
	}
	sim.step()
	return true
}

// stepTicks is when each replayed step happened, relative to detonation. A
// frame is stamped with the tick it was read on, which is late whenever
// several are read at once; the game never takes two steps closer together
// than its pace, so each happened at least a pace before the next.
func (l *voxelLog) stepTicks(tickRate float64) []int {
	pace := int(math.Ceil(simStepSecs*tickRate - 1e-9))
	ticks := make([]int, min(len(l.steps), smokeSettleSteps))
	for n := len(ticks) - 1; n >= 0; n-- {
		ticks[n] = l.steps[n].tick
		if n+1 < len(ticks) {
			ticks[n] = max(0, min(ticks[n], ticks[n+1]-pace))
		}
	}
	return ticks
}

// cloud replays the simulation, a step for each frame the log holds, and
// returns, by voxel, the tick after detonation the smoke reached it, or -1
// for a voxel outside the cloud as it finally stood.
func (l *voxelLog) cloud(det r3.Vector, tickRate float64) []int {
	sim := l.simulation(det)
	reached := make([]int, voxelCells)
	for m := range reached {
		reached[m] = -1
	}
	for n, tick := range l.stepTicks(tickRate) {
		if !l.advance(sim, n) {
			break
		}
		// Only a reached voxel holds density, so only those can have filled.
		for _, m := range sim.active {
			if reached[m] < 0 && sim.cell[sim.cur][m].density > smokeVisible {
				reached[m] = tick
			}
		}
	}
	arrival := make([]int, voxelCells)
	for m, tick := range reached {
		if sim.cell[sim.cur][m].density <= smokeVisible {
			tick = -1
		}
		arrival[voxelOfMorton[m]] = tick
	}
	return arrival
}

// export simulates and packs the cloud around det. ok is false when the log
// holds nothing to simulate from.
func (l *voxelLog) export(det r3.Vector, tickRate float64) (EventSmokeVoxels, bool) {
	if len(l.steps) == 0 || len(l.steps[0].emitters) == 0 || (det.X == 0 && det.Y == 0 && det.Z == 0) {
		return EventSmokeVoxels{}, false
	}
	if tickRate <= 0 {
		tickRate = 64
	}
	return exportVoxels(l.cloud(det, tickRate), det, tickRate)
}

// exportVoxels trims the cloud to its bounding box and packs it for the blob.
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

	buf []byte
	log *voxelLog
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
	if t.log.broken {
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
	t.log.read(t.buf, tick-t.startTick)
}

// export simulates and packs the cloud. ok is false when the smoke networked
// nothing to simulate from.
func (t *smokeVoxelTrack) export(endTick int, tickRate float64) (EventSmokeVoxels, bool) {
	out, ok := t.log.export(t.det, tickRate)
	if !ok {
		return out, false
	}
	out.GrenadeID = t.gid
	out.Round = t.round
	out.StartTick = t.startTick
	out.EndTick = endTick
	return out, true
}

// ErrNoSmokeVoxels means a voxel log held nothing to simulate a cloud from: no
// emitters, no detonation position, or no complete frame.
var ErrNoSmokeVoxels = errors.New("voxel log holds no smoke to reconstruct")

// maxReconstructTickRate and maxReconstructTick bound a request's timing:
// well past any real server, well short of overflowing a tick.
const (
	maxReconstructTickRate = 1024
	maxReconstructTick     = maxReconstructTickRate * 600
)

// ReconstructSmokeVoxels rebuilds a smoke from a voxel log read live off its
// projectile instead of out of a demo. buf is the log as it finally stood;
// samples are [length, tick after detonation] for each tick it grew, the reads
// poll makes every demo frame, so the cloud is the one the playback blob would
// carry. StartTick is 0 and EndTick the last sample's tick.
func ReconstructSmokeVoxels(buf []byte, samples [][2]int, det r3.Vector, tickRate float64) (EventSmokeVoxels, error) {
	if len(buf) > voxelFrameDataSlots {
		return EventSmokeVoxels{}, fmt.Errorf("log is %d bytes; the networked array holds %d", len(buf), voxelFrameDataSlots)
	}
	// Bounded so the timing arithmetic cannot overflow: CS2 runs at 64, and a
	// cloud finishes growing within seconds.
	if tickRate > maxReconstructTickRate {
		return EventSmokeVoxels{}, fmt.Errorf("tick rate %v is above %d", tickRate, maxReconstructTickRate)
	}
	l := newVoxelLog()
	seen, end := 0, 0
	for i, s := range samples {
		size, tick := s[0], s[1]
		if size < 0 || size > len(buf) {
			return EventSmokeVoxels{}, fmt.Errorf("sample %d: length %d is outside the %d-byte log", i, size, len(buf))
		}
		if tick < end {
			return EventSmokeVoxels{}, fmt.Errorf("sample %d: tick %d is before %d", i, tick, end)
		}
		if tick > maxReconstructTick {
			return EventSmokeVoxels{}, fmt.Errorf("sample %d: tick %d is more than %d after detonation", i, tick, maxReconstructTick)
		}
		end = tick
		// As in poll, only growth is read.
		if size <= seen {
			continue
		}
		seen = size
		l.read(buf[:size], tick)
	}
	out, ok := l.export(det, tickRate)
	if !ok {
		return out, ErrNoSmokeVoxels
	}
	out.EndTick = end
	return out, nil
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
	// Replaying a cloud is tens of milliseconds of simulation that nothing
	// reads until finalize, and a match throws a hundred of them, so it runs
	// beside the parse rather than in it. The track is no longer reachable
	// from the parse once it is out of voxelTracks, so the replay owns it.
	job := &voxelExport{done: make(chan struct{})}
	s.voxelJobs = append(s.voxelJobs, job)
	rate := s.tickRate
	voxelReplaySlots <- struct{}{}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "[smoke-voxels] replay of grenade %d panicked: %v\n", t.gid, r)
				job.ok = false
			}
			<-voxelReplaySlots
			close(job.done)
		}()
		job.out, job.ok = t.export(endTick, rate)
	}()
}

// voxelExport is one cloud being replayed off the parse goroutine.
type voxelExport struct {
	done chan struct{}
	out  EventSmokeVoxels
	ok   bool
}

// voxelReplaySlots bounds the replays in flight across every parse in the
// process, since each holds a couple of MB of simulation state.
var voxelReplaySlots = make(chan struct{}, runtime.GOMAXPROCS(0))

// collectSmokeVoxels waits for every replay started so far and appends the
// clouds in the order their logs closed.
func (s *state) collectSmokeVoxels() {
	for _, job := range s.voxelJobs {
		<-job.done
		if !job.ok {
			s.voxelSkipped++
			continue
		}
		s.res.SmokeVoxels = append(s.res.SmokeVoxels, job.out)
		s.voxelBoxCells += job.out.DimX * job.out.DimY * job.out.DimZ
	}
	s.voxelJobs = nil
}

// closeAllSmokeVoxels flushes every log still open, at a round transition or
// the end of the demo.
func (s *state) closeAllSmokeVoxels(tick int) {
	for id := range s.voxelTracks {
		s.closeSmokeVoxels(id, tick)
	}
}

// sortSmokeVoxels puts the clouds in detonation order. They are appended as
// they close, which is expiry order. Call collectSmokeVoxels first.
func (s *state) sortSmokeVoxels() {
	sort.SliceStable(s.res.SmokeVoxels, func(i, j int) bool {
		a, b := s.res.SmokeVoxels[i], s.res.SmokeVoxels[j]
		if a.StartTick != b.StartTick {
			return a.StartTick < b.StartTick
		}
		return a.GrenadeID < b.GrenadeID
	})
}
