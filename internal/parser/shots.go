package parser

import (
	"math"

	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"
)

// onFrameDone samples per-player position/velocity each tick. Used by
// WeaponFire to annotate shots with speed + counter-strafe state.
func (s *state) onFrameDone(_ events.FrameDone) {
	if !s.matchStarted {
		return
	}
	tickRate := s.parser.TickRate()
	curTick := s.parser.GameState().IngameTick()
	seen := map[string]bool{}
	for _, p := range s.parser.GameState().Participants().Playing() {
		if p == nil || !p.IsAlive() {
			continue
		}
		sid := steamIDStr(p)
		if sid == "" {
			continue
		}
		seen[sid] = true
		pos := p.Position()
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
		eye, _ := p.PositionEyes()
		s.frames[sid] = playerFrame{
			pos:      pos,
			speed:    speed,
			hasSpeed: hasSpeed,
			team:     p.Team,
			alive:    true,
			tick:     curTick,
			yaw:      p.ViewDirectionX(),
			pitch:    p.ViewDirectionY(),
			eye:      eye,
		}
		if hasSpeed && speed > 100 {
			s.lastMoveTick[sid] = curTick
		}
	}
	for sid, f := range s.frames {
		if !seen[sid] && f.alive {
			f.alive = false
			s.frames[sid] = f
		}
	}

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
	if s.lastPositionSampleTick != 0 && curTick-s.lastPositionSampleTick < sampleEvery {
		return
	}
	s.lastPositionSampleTick = curTick
	// Skip freezetime + end-of-round walkaround — the replay viewer
	// auto-skips both, so persisting them is pure waste.
	if !s.liveRound {
		return
	}
	// Bomb carrier this sample tick, if any. Match by SteamID rather
	// than pointer — the carrier pointer is generally stable across
	// frames in v5, but some demos churn the participants slice and
	// SteamID is the only identity that survives reliably.
	var carrierSID string
	if b := s.parser.GameState().Bomb(); b != nil && b.Carrier != nil {
		carrierSID = steamIDStr(b.Carrier)
	}
	for _, p := range s.parser.GameState().Participants().Playing() {
		if p == nil {
			continue
		}
		sid := steamIDStr(p)
		if sid == "" {
			continue
		}
		pos := p.Position()
		s.res.Positions = append(s.res.Positions, EventPosition{
			Tick:            curTick,
			Round:           s.currentRound,
			AttackerSteamID: sid,
			Team:            teamCode(p.Team),
			Alive:           p.IsAlive(),
			X:               float32(pos.X),
			Y:               float32(pos.Y),
			Z:               float32(pos.Z),
			Yaw:             p.ViewDirectionX(),
			Pitch:           p.ViewDirectionY(),
			Health:          p.Health(),
			Armor:           p.Armor(),
			HasHelmet:       p.HasHelmet(),
			HasBomb:         carrierSID != "" && sid == carrierSID,
			HasDefuser:      p.Team == common.TeamCounterTerrorists && p.HasDefuseKit(),
			ActiveWeapon:    activeWeaponName(p),
		})
	}
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
			if s.los(eye, enemyEye) {
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

	s.res.ShotsFired = append(s.res.ShotsFired, ev)
	idx := len(s.res.ShotsFired) - 1
	s.lastShot[attackerID] = shotMark{tick: curTick, isSpray: isSpray, enemySpotted: enemySpotted, idx: idx}
	s.recordEngagementShot(attackerID, eye, yaw, pitch, ev.Weapon)
}
