package parser

import "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"

// onPlayerHurt records one row per damage instance. Skips self-damage
// and null attackers (world / falling). Engagement metrics come from
// the visStart entry consumed here.
func (s *state) onPlayerHurt(e events.PlayerHurt) {
	if !s.matchStarted || s.currentRound == 0 {
		return
	}
	if e.Attacker == nil || e.Player == nil || e.Attacker == e.Player {
		return
	}
	attackerID := steamIDStr(e.Attacker)
	victimID := steamIDStr(e.Player)
	tick := s.parser.GameState().IngameTick()
	// Attribute this damage to the attacker's most-recent shot if it
	// fired within 250ms; inherit the spray flag.
	fromSpray := false
	if rate := s.parser.TickRate(); rate > 0 {
		if prev, ok := s.lastShot[attackerID]; ok {
			if float64(tick-prev.tick)/rate < 0.25 && prev.isSpray {
				fromSpray = true
			}
		}
	}
	d := EventDamage{
		Tick:            tick,
		Round:           s.currentRound,
		AttackerSteamID: attackerID,
		VictimSteamID:   victimID,
		AttackerTeam:    teamCode(e.Attacker.Team),
		VictimTeam:      teamCode(e.Player.Team),
		Damage:          e.HealthDamage,
		DamageArmor:     e.ArmorDamage,
		Hitgroup:        int(e.HitGroup),
		Health:          e.Health,
		HitOnSpotted:    e.Player.IsSpottedBy(e.Attacker),
		FromSpray:       fromSpray,
	}
	if e.Weapon != nil {
		d.Weapon = weaponCanonical(e.Weapon)
	}
	// Consume the matching visibility entry: attacker saw victim at
	// some earlier tick and this is the first damage in that
	// engagement.
	if vis, ok := s.visStart[victimID]; ok {
		if entry, ok2 := vis[attackerID]; ok2 {
			if rate := s.parser.TickRate(); rate > 0 {
				secs := float64(tick-entry.tick) / rate
				// Cap at 3s — beyond that this is a hold-angle /
				// trigger-discipline play, not a reaction engagement.
				if secs >= 0 && secs <= 3 {
					d.SpotToDamageS = &secs
				}
			}
			spotView := viewVector(entry.yaw, entry.pitch)
			nowView := viewVector(e.Attacker.ViewDirectionX(), e.Attacker.ViewDirectionY())
			angle := angleBetweenDeg(spotView, nowView)
			// Cap at 45° — wider is wraparound noise or a flick
			// through a wall, not crosshair placement.
			if angle >= 0 && angle <= 45 {
				d.CrosshairDeltaDeg = &angle
			}
			delete(vis, attackerID)
		}
	}
	s.res.Damages = append(s.res.Damages, d)
}

// onPlayerSpottersChanged diffs the spotter set for e.Spotted against
// the cached set and emits one EventSpotted per newly-appearing
// spotter. Losses-of-sight are ignored.
func (s *state) onPlayerSpottersChanged(e events.PlayerSpottersChanged) {
	if !s.matchStarted || e.Spotted == nil {
		return
	}
	spottedID := steamIDStr(e.Spotted)
	if spottedID == "" {
		return
	}
	prev := s.visStart[spottedID]
	next := map[string]visEntry{}
	tick := s.parser.GameState().IngameTick()
	for _, p := range s.parser.GameState().Participants().All() {
		if p == nil || p == e.Spotted {
			continue
		}
		if !p.HasSpotted(e.Spotted) {
			continue
		}
		pid := steamIDStr(p)
		if pid == "" {
			continue
		}
		if existing, had := prev[pid]; had {
			// Continuing visibility — preserve the original spot
			// tick so the next PlayerHurt measures from first-sight.
			next[pid] = existing
			continue
		}
		next[pid] = visEntry{
			tick:  tick,
			yaw:   p.ViewDirectionX(),
			pitch: p.ViewDirectionY(),
		}
		s.res.Spotted = append(s.res.Spotted, EventSpotted{
			Tick:           tick,
			Round:          s.currentRound,
			SpotterSteamID: pid,
			SpottedSteamID: spottedID,
			SpotterTeam:    teamCode(p.Team),
		})
	}
	s.visStart[spottedID] = next
}
