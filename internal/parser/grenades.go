package parser

import "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"

// onGrenadeProjectileThrow fires when the projectile entity is
// created — i.e. the moment the grenade leaves the player's hand.
// FireGrenadeStart has a nil Thrower in Source 2 demos, so throw-side
// data must come from this event.
func (s *state) onGrenadeProjectileThrow(e events.GrenadeProjectileThrow) {
	if !s.matchStarted || e.Projectile == nil {
		return
	}
	thrower := e.Projectile.Thrower
	if thrower == nil {
		thrower = e.Projectile.Owner
	}
	var gtype string
	if e.Projectile.WeaponInstance != nil {
		gtype = grenadeTypeCode(e.Projectile.WeaponInstance.Type)
	}
	if gtype == "" {
		return
	}
	pos := e.Projectile.Position()
	if (pos.X == 0 && pos.Y == 0) && thrower != nil {
		tp := thrower.Position()
		pos.X = tp.X
		pos.Y = tp.Y
		pos.Z = tp.Z
	}
	ev := EventGrenadeThrow{
		Tick:    s.parser.GameState().IngameTick(),
		Round:   s.currentRound,
		Type:    gtype,
		OriginX: float32(pos.X),
		OriginY: float32(pos.Y),
		OriginZ: float32(pos.Z),
	}
	if thrower != nil {
		ev.ThrowerSteamID = steamIDStr(thrower)
		ev.ThrowerTeam = teamCode(thrower.Team)
	}
	s.res.GrenadeThrows = append(s.res.GrenadeThrows, ev)

	if e.Projectile.Entity != nil {
		entID := e.Projectile.Entity.ID()
		s.grenadePos[entID] = grenadeProjectile{
			x:       float32(pos.X),
			y:       float32(pos.Y),
			z:       float32(pos.Z),
			gtype:   gtype,
			thrower: ev.ThrowerSteamID,
			team:    ev.ThrowerTeam,
		}
	}
}

// onGrenadeProjectileDestroy fires when the projectile entity is
// removed — typically right after the detonation. We snapshot the
// final position here as a fallback for detonate-event Position quirks.
func (s *state) onGrenadeProjectileDestroy(e events.GrenadeProjectileDestroy) {
	if e.Projectile == nil || e.Projectile.Entity == nil {
		return
	}
	pos := e.Projectile.Position()
	entID := e.Projectile.Entity.ID()
	if g, ok := s.grenadePos[entID]; ok {
		g.x = float32(pos.X)
		g.y = float32(pos.Y)
		g.z = float32(pos.Z)
		s.grenadePos[entID] = g
	}
}

// onFrameDoneGrenades samples the live position of every active
// projectile so the detonation handlers below can read a reliable
// "last known" coordinate when demoinfocs returns a stale or zeroed
// Position on the event itself. Called from onFrameDone.
func (s *state) onFrameDoneGrenades() {
	gs := s.parser.GameState()
	if gs == nil {
		return
	}
	for _, p := range gs.GrenadeProjectiles() {
		if p == nil || p.Entity == nil {
			continue
		}
		entID := p.Entity.ID()
		g, ok := s.grenadePos[entID]
		if !ok {
			// First sighting outside of throw event (rare): record what
			// we know so a later detonation has somewhere to read from.
			var gtype string
			if p.WeaponInstance != nil {
				gtype = grenadeTypeCode(p.WeaponInstance.Type)
			}
			thrower := p.Thrower
			if thrower == nil {
				thrower = p.Owner
			}
			g = grenadeProjectile{gtype: gtype}
			if thrower != nil {
				g.thrower = steamIDStr(thrower)
				g.team = teamCode(thrower.Team)
			}
		}
		pos := p.Position()
		g.x = float32(pos.X)
		g.y = float32(pos.Y)
		g.z = float32(pos.Z)
		s.grenadePos[entID] = g
	}
}

// emitDetonate is the shared handler for the four detonation events.
// Prefers the tracked projectile position over the event's Position
// since demoinfocs reports stale/(0,0,0) Position on some CS2 demos.
func (s *state) emitDetonate(base events.GrenadeEvent, typeOverride string) {
	gtype := typeOverride
	if gtype == "" {
		gtype = grenadeTypeCode(base.GrenadeType)
	}
	if gtype == "" {
		return
	}

	x := float32(base.Position.X)
	y := float32(base.Position.Y)
	z := float32(base.Position.Z)
	if g, ok := s.grenadePos[base.GrenadeEntityID]; ok {
		// Tracked projectile position is the source of truth; only use
		// event Position if it's plausibly non-stale (non-zero AND
		// within a sane range of the tracked one).
		if g.x != 0 || g.y != 0 {
			x = g.x
			y = g.y
			z = g.z
		}
	}

	ev := EventGrenadeDetonate{
		Tick:  s.parser.GameState().IngameTick(),
		Round: s.currentRound,
		Type:  gtype,
		X:     x,
		Y:     y,
		Z:     z,
	}
	if base.Thrower != nil {
		ev.ThrowerSteamID = steamIDStr(base.Thrower)
	} else if g, ok := s.grenadePos[base.GrenadeEntityID]; ok && g.thrower != "" {
		ev.ThrowerSteamID = g.thrower
	}
	s.res.GrenadeDetonations = append(s.res.GrenadeDetonations, ev)
}

func (s *state) onHeExplode(e events.HeExplode) {
	if !s.matchStarted {
		return
	}
	s.emitDetonate(e.GrenadeEvent, "HE")
}

func (s *state) onFlashExplode(e events.FlashExplode) {
	if !s.matchStarted {
		return
	}
	s.emitDetonate(e.GrenadeEvent, "Flash")
}

func (s *state) onSmokeStart(e events.SmokeStart) {
	if !s.matchStarted {
		return
	}
	s.emitDetonate(e.GrenadeEvent, "Smoke")
}

func (s *state) onFireGrenadeStart(e events.FireGrenadeStart) {
	if !s.matchStarted {
		return
	}
	s.emitDetonate(e.GrenadeEvent, "Molotov")
}

// onPlayerFlashed fires once per blinded player per flash. We capture
// the attacker (thrower), the victim, and the resulting blind duration
// so player_flashes aggregates (enemies_flashed / team_flashed /
// avg_blind_time) work for imported demos.
func (s *state) onPlayerFlashed(e events.PlayerFlashed) {
	if !s.matchStarted {
		return
	}
	if e.Player == nil || e.Attacker == nil {
		return
	}
	attackerTeam := teamCode(e.Attacker.Team)
	victimTeam := teamCode(e.Player.Team)
	ev := EventFlash{
		Tick:            s.parser.GameState().IngameTick(),
		Round:           s.currentRound,
		AttackerSteamID: steamIDStr(e.Attacker),
		AttackerTeam:    attackerTeam,
		VictimSteamID:   steamIDStr(e.Player),
		VictimTeam:      victimTeam,
		Duration:        e.Player.FlashDurationTime().Seconds(),
		TeamFlash:       attackerTeam != "" && attackerTeam == victimTeam,
	}
	s.res.Flashes = append(s.res.Flashes, ev)
}
