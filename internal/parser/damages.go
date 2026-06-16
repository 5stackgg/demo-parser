package parser

import (
	"github.com/golang/geo/r3"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"
)

// onPlayerHurt records one row per damage instance. Skips self-damage
// and null attackers (world / falling). Engagement metrics come from
// the visStart entry consumed here.
func (s *state) onPlayerHurt(e events.PlayerHurt) {
	if !s.matchStarted || s.currentRound == 0 {
		return
	}
	if e.Player == nil {
		return
	}
	victimID := steamIDStr(e.Player)
	before, ok := s.victimHealth[victimID]
	if !ok {
		before = 100
	}
	damage := e.HealthDamageTaken
	if damage > before {
		damage = before
	}
	if damage < 0 {
		damage = 0
	}
	s.victimHealth[victimID] = e.Health

	if e.Attacker == nil || e.Attacker == e.Player {
		return
	}
	attackerID := steamIDStr(e.Attacker)
	tick := s.parser.GameState().IngameTick()
	// Attribute this damage to the attacker's most-recent shot if it
	// fired within 250ms; inherit the spray flag.
	fromSpray := false
	headshot := int(e.HitGroup) == 1
	if rate := s.parser.TickRate(); rate > 0 {
		if prev, ok := s.lastShot[attackerID]; ok && float64(tick-prev.tick)/rate < sprayWindowSecs {
			if prev.isSpray && prev.enemySpotted {
				fromSpray = true
			}
			// Backfill the firing shot's outcome (for the 3D tracer color)
			// and end-point. First damage wins; headshot beats body.
			if prev.idx >= 0 && prev.idx < len(s.res.ShotsFired) {
				sh := &s.res.ShotsFired[prev.idx]
				if sh.Result == "" {
					if headshot {
						sh.Result = "headshot"
					} else {
						sh.Result = "hit"
					}
					veye, _ := e.Player.PositionEyes()
					sh.ImpactX = f32ptr(veye.X)
					sh.ImpactY = f32ptr(veye.Y)
					sh.ImpactZ = f32ptr(veye.Z)
				}
			}
			// Mark the engagement's first shot as a hit if this damage
			// landed right after it.
			if m := s.engagements[attackerID]; m != nil {
				if eng, ok := m[victimID]; ok && eng.firstShotFired && !eng.firstShotHit {
					if float64(tick-eng.firstShotTick)/rate < sprayWindowSecs {
						eng.firstShotHit = true
					}
				}
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
		Damage:          damage,
		DamageArmor:     e.ArmorDamageTaken,
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
			// Only derive reaction / crosshair-placement from a spot the
			// geometry confirms was a real sightline.
			if s.los(entry.eye, entry.target) {
				if rate := s.parser.TickRate(); rate > 0 {
					secs := float64(tick-entry.tick) / rate
					// Floor at 0.2s — faster than human reaction, so the
					// attacker was pre-aimed, not reacting. Cap at 3s —
					// beyond that this is a hold-angle / trigger-discipline
					// play, not a reaction engagement.
					if secs >= reactionFloorSecs && secs <= reactionCapSecs {
						d.SpotToDamageS = &secs
					}
				}
				spotView := viewVector(entry.yaw, entry.pitch)
				toTarget := r3.Vector{
					X: entry.target.X - entry.eye.X,
					Y: entry.target.Y - entry.eye.Y,
					Z: entry.target.Z - entry.eye.Z,
				}
				angle := angleBetweenDeg(spotView, toTarget)
				if angle >= 0 && angle <= 90 {
					d.CrosshairDeltaDeg = &angle
				}
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
		eye, _ := p.PositionEyes()
		target, _ := e.Spotted.PositionEyes()
		entry := visEntry{
			tick:   tick,
			yaw:    p.ViewDirectionX(),
			pitch:  p.ViewDirectionY(),
			eye:    eye,
			target: target,
		}
		rate := s.parser.TickRate()
		recent := func(fe visEntry) bool { return rate <= 0 || float64(tick-fe.tick)/rate <= fovEntryRecentSecs }
		if w, ok := s.fovEntryWide[spottedID][pid]; ok && recent(w) {
			entry = w
			if t, ok2 := s.fovEntryTight[spottedID][pid]; ok2 && recent(t) {
				entry.yaw, entry.pitch, entry.eye, entry.target = t.yaw, t.pitch, t.eye, t.target
			}
		}
		next[pid] = entry
		// Only treat this as a real spot/engagement when the geometry
		// confirms a clear sightline — CS2's spotted flag can fire through
		// smoke, thin gaps, or the edge of vision.
		if !s.los(entry.eye, entry.target) {
			continue
		}
		// Begin tracking this attacker→victim engagement from first sight.
		s.openEngagement(pid, spottedID, entry)
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
