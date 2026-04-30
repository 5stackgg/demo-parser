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
		s.frames[sid] = playerFrame{
			pos:      pos,
			speed:    speed,
			hasSpeed: hasSpeed,
			team:     p.Team,
			alive:    true,
			tick:     curTick,
		}
	}
	for sid, f := range s.frames {
		if !seen[sid] && f.alive {
			f.alive = false
			s.frames[sid] = f
		}
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

	enemySpotted := false
	for _, p := range s.parser.GameState().Participants().Playing() {
		if p == nil || p == e.Shooter || !p.IsAlive() {
			continue
		}
		if p.Team == e.Shooter.Team {
			continue
		}
		if p.IsSpottedBy(e.Shooter) {
			enemySpotted = true
			break
		}
	}

	curTick := s.parser.GameState().IngameTick()
	attackerID := steamIDStr(e.Shooter)

	// Spray = this shot followed the same attacker's previous shot
	// within 250ms (trigger held). Tap shots fall outside the window.
	isSpray := false
	if rate := s.parser.TickRate(); rate > 0 {
		if prev, ok := s.lastShot[attackerID]; ok {
			if float64(curTick-prev.tick)/rate < 0.25 {
				isSpray = true
			}
		}
	}
	s.lastShot[attackerID] = shotMark{tick: curTick, isSpray: isSpray}

	ev := EventShotFired{
		Tick:            curTick,
		Round:           s.currentRound,
		AttackerSteamID: attackerID,
		AttackerTeam:    teamCode(e.Shooter.Team),
		Weapon:          e.Weapon.String(),
		IsRifle:         isRifle,
		IsCrouched:      e.Shooter.IsDucking(),
		EnemySpotted:    enemySpotted,
		IsSpray:         isSpray,
	}

	if sf, ok := s.frames[ev.AttackerSteamID]; ok && sf.alive && sf.hasSpeed {
		speed := sf.speed
		ev.Speed = &speed
		if maxSpd, ok := weaponMaxSpeed[e.Weapon.Type]; ok && maxSpd > 0 {
			stopped := speed < 0.34*maxSpd
			ev.WasStopped = &stopped
		}
	}

	s.res.ShotsFired = append(s.res.ShotsFired, ev)
}
