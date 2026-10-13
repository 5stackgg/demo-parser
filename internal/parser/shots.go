package parser

import (
	"math"
	"sort"

	"github.com/golang/geo/r3"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"
)

// tickPlayer is one playing player as the current frame left them, read off
// the entities once and shared by everything onFrameDone drives. Every
// accessor on common.Player resolves the pawn and looks its properties up by
// name, so reading each player once per consumer — the frame sample, the FOV
// pass, the position ring — cost more than the work done with the values.
type tickPlayer struct {
	p     *common.Player
	sid   string
	team  common.Team
	alive bool
	pos   r3.Vector
	yaw   float32
	pitch float32
	// eye and blind are read only for living players; nothing asks them of the
	// dead.
	eye   r3.Vector
	blind float64
}

// snapshotPlayers fills s.framePlayers with every playing player that has a
// SteamID, in Participants().Playing() order — the order each consumer used
// to walk.
func (s *state) snapshotPlayers() {
	s.framePlayers = s.framePlayers[:0]
	for _, p := range s.parser.GameState().Participants().Playing() {
		if p == nil {
			continue
		}
		sid := s.steamID(p)
		if sid == "" {
			continue
		}
		tp := tickPlayer{
			p:     p,
			sid:   sid,
			team:  p.Team,
			alive: p.IsAlive(),
			pos:   p.Position(),
			yaw:   p.ViewDirectionX(),
			pitch: p.ViewDirectionY(),
		}
		if tp.alive {
			tp.eye, _ = p.PositionEyes()
			tp.blind = p.FlashDurationTimeRemaining().Seconds()
		}
		s.framePlayers = append(s.framePlayers, tp)
	}
}

// steamID is steamIDStr without formatting the same handful of IDs every
// frame.
func (s *state) steamID(p *common.Player) string {
	if p == nil || p.SteamID64 == 0 {
		return ""
	}
	sid, ok := s.sids[p.SteamID64]
	if !ok {
		sid = steamIDStr(p)
		s.sids[p.SteamID64] = sid
	}
	return sid
}

// onFrameDone samples per-player position/velocity each tick. Used by
// WeaponFire to annotate shots with speed + counter-strafe state.
func (s *state) onFrameDone(_ events.FrameDone) {
	if !s.matchStarted {
		return
	}
	tickRate := s.parser.TickRate()
	s.tickRate = tickRate
	s.resolveHits()
	curTick := s.parser.GameState().IngameTick()
	s.snapshotPlayers()
	clear(s.frameSeen)
	for i := range s.framePlayers {
		tp := &s.framePlayers[i]
		if !tp.alive {
			continue
		}
		sid := tp.sid
		s.frameSeen[sid] = true
		pos := tp.pos
		prev, hadPrev := s.frames[sid]
		var (
			speed    float32
			hasSpeed bool
		)
		if hadPrev && prev.alive && prev.tick > 0 && tickRate > 0 && curTick > prev.tick {
			dt := float64(curTick-prev.tick) / tickRate
			if dt > 0 {
				dx := pos.X - prev.pos.X
				dy := pos.Y - prev.pos.Y
				speed = float32(math.Sqrt(dx*dx+dy*dy) / dt)
				hasSpeed = true
			}
		}
		s.frames[sid] = playerFrame{
			pos:            pos,
			speed:          speed,
			hasSpeed:       hasSpeed,
			team:           tp.team,
			alive:          true,
			tick:           curTick,
			yaw:            tp.yaw,
			pitch:          tp.pitch,
			eye:            tp.eye,
			blindRemaining: tp.blind,
		}
		s.recordEye(sid, curTick, tp.eye)
		if hasSpeed && speed > 100 {
			s.lastMoveTick[sid] = curTick
		}
	}
	for sid, f := range s.frames {
		if !s.frameSeen[sid] && f.alive {
			f.alive = false
			s.frames[sid] = f
		}
	}

	s.updateDoors()
	s.trackInfernos()
	s.pollSmokeVoxels()
	s.trackFOV()
	s.trackEngagements()

	// Snapshot live projectile positions so the detonate-event handlers
	// can fall back to a reliable coordinate (demoinfocs returns stale
	// or zeroed Position on some CS2 grenade events).
	s.onFrameDoneGrenades()

	// Sample ~4Hz for the 2D replay buffer. 64-tick demo ≈ every 16
	// ticks; tickrate-aware so we still hit 4Hz on 128-tick demos.
	if tickRate <= 0 {
		return
	}
	sampleEvery := int(tickRate / 4)
	if sampleEvery < 1 {
		sampleEvery = 1
	}
	// Asked before the liveRound gate below, so the ~4Hz clock keeps the phase
	// it had before this window existed.
	due := s.positionSampleDue(curTick, sampleEvery)
	// Skip freezetime + end-of-round walkaround — the replay viewer
	// auto-skips both, so persisting them is pure waste.
	if !s.liveRound {
		return
	}
	// Every live tick is captured, whether or not it is due: a grenade thrown
	// in the next few ticks needs the ones already behind it.
	slot := s.capturePositions(curTick)
	if due || curTick <= s.burstUntilTick {
		s.emitPositions(slot)
	}
}

// throwBurstTicks is how far either side of a grenade throw positions are
// emitted at the demo's full tick rate instead of the usual ~4Hz.
//
// At 4Hz the nearest sample to a release can be 125ms stale, which is about 30
// units of drift at run speed — enough to put a mined lineup's standing spot in
// the wrong place and its view angles on the wrong pixel. Ten ticks is 156ms at
// 64 tick and covers the whole run-up-and-release, at a cost of ~20 extra rows
// per player per throw.
const throwBurstTicks = 10

// positionSample is one tick's worth of position rows. Slots live in a short
// ring so a throw can emit the ticks that came before it, and their row slices
// are reused rather than reallocated every tick.
type positionSample struct {
	tick int
	// emitted guards against a row reaching Result.Positions twice, when a
	// due 4Hz sample and a throw window land on the same tick.
	emitted bool
	rows    []EventPosition
}

// positionSampleDue reports whether the ~4Hz clock has come round, advancing it
// when it has.
func (s *state) positionSampleDue(tick, every int) bool {
	if s.lastPositionSampleTick != 0 && tick-s.lastPositionSampleTick < every {
		return false
	}
	s.lastPositionSampleTick = tick
	return true
}

// stagePositionSlot claims this tick's ring slot, dropping whatever tick was
// previously in it.
func (s *state) stagePositionSlot(tick int) *positionSample {
	slot := &s.posRing[tick%len(s.posRing)]
	slot.tick = tick
	slot.emitted = false
	slot.rows = slot.rows[:0]
	return slot
}

// capturePositions fills this tick's ring slot with a row per playing player
// and returns it. Nothing is emitted here.
func (s *state) capturePositions(tick int) *positionSample {
	slot := s.stagePositionSlot(tick)

	// Bomb carrier this sample tick, if any. Match by SteamID rather
	// than pointer — the carrier pointer is generally stable across
	// frames in v5, but some demos churn the participants slice and
	// SteamID is the only identity that survives reliably.
	var carrierSID string
	if b := s.parser.GameState().Bomb(); b != nil && b.Carrier != nil {
		carrierSID = s.steamID(b.Carrier)
	}
	for i := range s.framePlayers {
		tp := &s.framePlayers[i]
		p := tp.p
		slot.rows = append(slot.rows, EventPosition{
			Tick:            tick,
			Round:           s.currentRound,
			AttackerSteamID: tp.sid,
			Team:            teamCode(tp.team),
			Alive:           tp.alive,
			X:               float32(tp.pos.X),
			Y:               float32(tp.pos.Y),
			Z:               float32(tp.pos.Z),
			Yaw:             tp.yaw,
			Pitch:           tp.pitch,
			Health:          p.Health(),
			Armor:           p.Armor(),
			HasHelmet:       p.HasHelmet(),
			HasBomb:         carrierSID != "" && tp.sid == carrierSID,
			HasDefuser:      tp.team == common.TeamCounterTerrorists && p.HasDefuseKit(),
			ActiveWeapon:    activeWeaponName(p),
			Ducked:          p.IsDucking(),
		})
	}
	return slot
}

func (s *state) emitPositions(slot *positionSample) {
	if slot == nil || slot.emitted {
		return
	}
	slot.emitted = true
	s.res.Positions = append(s.res.Positions, slot.rows...)
}

// sortPositions puts the array back in tick order and reports whether it had
// to. A throw burst emits the ticks behind it after later samples have already
// been appended; consumers walk this array as a timeline. Stable, so the
// per-tick block of players keeps the order it was captured in.
func (s *state) sortPositions() bool {
	if s.positionBursts == 0 {
		return false
	}
	sort.SliceStable(s.res.Positions, func(i, j int) bool {
		return s.res.Positions[i].Tick < s.res.Positions[j].Tick
	})
	return true
}

// burstPositions emits the captured window behind a grenade throw and opens the
// window ahead of it. Overlapping throws extend the same run rather than
// duplicating rows.
//
// The rows flushed here are older than what is already in Result.Positions, so
// finalize sorts the array back into tick order.
func (s *state) burstPositions(tick int) {
	if !s.liveRound {
		return
	}
	for t := tick - throwBurstTicks; t <= tick; t++ {
		if t < 0 {
			continue
		}
		slot := &s.posRing[t%len(s.posRing)]
		if slot.tick != t {
			continue // that tick has already rolled out of the ring
		}
		s.emitPositions(slot)
	}
	if until := tick + throwBurstTicks; until > s.burstUntilTick {
		s.burstUntilTick = until
	}
	s.positionBursts++
}

// onWeaponFire records one row per shot. Firearms only — knife and
// grenade "fires" would balloon the array and skew accuracy.
func (s *state) onWeaponFire(e events.WeaponFire) {
	if !s.matchStarted || e.Shooter == nil || e.Weapon == nil {
		return
	}
	switch e.Weapon.Class() {
	case common.EqClassPistols, common.EqClassSMG,
		common.EqClassHeavy, common.EqClassRifle:
		// keep
	default:
		return
	}

	isRifle := e.Weapon.Class() == common.EqClassRifle

	// Exact firing geometry at this tick — muzzle origin + view angles — for
	// the 3D replay tracer and the LOS check.
	eye, _ := e.Shooter.PositionEyes()
	yaw := e.Shooter.ViewDirectionX()
	pitch := e.Shooter.ViewDirectionY()

	enemySpotted := false
	for _, p := range s.parser.GameState().Participants().Playing() {
		if p == nil || p == e.Shooter || !p.IsAlive() {
			continue
		}
		if p.Team == e.Shooter.Team {
			continue
		}
		// Require a geometry-confirmed sightline, not just the engine's
		// spotted flag (which leaks through smoke / thin gaps).
		if p.IsSpottedBy(e.Shooter) {
			enemyEye, _ := p.PositionEyes()
			if s.visibleAt(s.parser.GameState().IngameTick(), eye, enemyEye, p.Position()) {
				enemySpotted = true
				break
			}
		}
	}

	curTick := s.parser.GameState().IngameTick()
	attackerID := steamIDStr(e.Shooter)

	// Spray = this shot followed the same attacker's previous shot
	// within 250ms (trigger held). Tap shots fall outside the window.
	isSpray := false
	if rate := s.parser.TickRate(); rate > 0 {
		if prev, ok := s.lastShot[attackerID]; ok {
			if float64(curTick-prev.tick)/rate < sprayWindowSecs {
				isSpray = true
			}
		}
	}

	ev := EventShotFired{
		Tick:            curTick,
		Round:           s.currentRound,
		AttackerSteamID: attackerID,
		AttackerTeam:    teamCode(e.Shooter.Team),
		Weapon:          weaponCanonical(e.Weapon),
		IsRifle:         isRifle,
		IsCrouched:      e.Shooter.IsDucking(),
		EnemySpotted:    enemySpotted,
		IsSpray:         isSpray,
		Yaw:             f32ptr(float64(yaw)),
		Pitch:           f32ptr(float64(pitch)),
		EyeX:            f32ptr(eye.X),
		EyeY:            f32ptr(eye.Y),
		EyeZ:            f32ptr(eye.Z),
	}

	if mt, ok := s.lastMoveTick[attackerID]; ok {
		if rate := s.parser.TickRate(); rate > 0 && float64(curTick-mt)/rate <= 0.4 {
			ev.WasMoving = true
		}
	}

	if sf, ok := s.frames[ev.AttackerSteamID]; ok && sf.alive && sf.hasSpeed {
		speed := sf.speed
		ev.Speed = &speed
		if maxSpd, ok := weaponMaxSpeed[e.Weapon.Type]; ok && maxSpd > 0 {
			stopped := speed < 0.5*maxSpd
			ev.WasStopped = &stopped
		}
	}

	// AmmoInMagazine is captured AFTER the shot fires; add 1 to recover
	// the pre-shot count. Downstream uses a sequence of these values to
	// infer reloads (count increases between consecutive shots).
	// Some weapons (knife, grenades that route through here, edge cases
	// in CS2 demos) return a uint32 sentinel like 0xFFFFFFFF — cap to a
	// sane range so the int4 DB column doesn't overflow.
	if e.Weapon != nil {
		ammoAfter := e.Weapon.AmmoInMagazine()
		if ammoAfter >= 0 && ammoAfter < 1000 {
			ammoBefore := ammoAfter + 1
			ev.AmmoInMagazine = &ammoBefore
		}
	}

	// Where the round actually stopped, when it did not hit anybody. Without
	// this a miss has no end point, so the replay flies it a fixed distance and
	// bullets sail straight through walls. We already have the map's collision
	// mesh and a raycaster for the sightline work, so the real wall is one query
	// away. Hits keep their victim impact point, filled in by onPlayerHurt.
	s.ensureMesh()
	if s.mesh != nil {
		dir := viewVector(yaw, pitch)
		if dist, ok := s.mesh.RayHitDist(eye, dir); ok && dist > 0 {
			ev.MissX = f32ptr(eye.X + dir.X*dist)
			ev.MissY = f32ptr(eye.Y + dir.Y*dist)
			ev.MissZ = f32ptr(eye.Z + dir.Z*dist)
		}
	}

	s.res.ShotsFired = append(s.res.ShotsFired, ev)
	idx := len(s.res.ShotsFired) - 1
	s.lastShot[attackerID] = shotMark{tick: curTick, idx: idx}
	s.recordEngagementShot(attackerID, eye, yaw, pitch, ev.Weapon)
}
