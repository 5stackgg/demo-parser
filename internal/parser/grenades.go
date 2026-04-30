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
}

// emitDetonate is the shared handler for the four detonation events.
// FireGrenadeStart's Thrower is always nil in Source 2; the
// ingestion side can join back to the most recent matching throw if
// it needs to attribute mollies.
func (s *state) emitDetonate(base events.GrenadeEvent, typeOverride string) {
	gtype := typeOverride
	if gtype == "" {
		gtype = grenadeTypeCode(base.GrenadeType)
	}
	if gtype == "" {
		return
	}
	ev := EventGrenadeDetonate{
		Tick:  s.parser.GameState().IngameTick(),
		Round: s.currentRound,
		Type:  gtype,
		X:     float32(base.Position.X),
		Y:     float32(base.Position.Y),
		Z:     float32(base.Position.Z),
	}
	if base.Thrower != nil {
		ev.ThrowerSteamID = steamIDStr(base.Thrower)
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
