package parser

import (
	"math"

	"github.com/golang/geo/r3"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
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
	if rate := s.parser.TickRate(); rate > 0 {
		if prev, ok := s.lastShot[attackerID]; ok && float64(tick-prev.tick)/rate < sprayWindowSecs {
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
			// geometry confirms was a real sightline — evaluated at the tick
			// the spot happened, since the smoke situation has since moved on.
			if s.visibleAt(entry.tick, entry.eye, entry.target, entry.targetFeet) {
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
				// Measured against where the spotter's client was drawing the
				// target, not where the server had it — crosshair placement is
				// a question about their screen.
				toTarget := r3.Vector{
					X: entry.targetView.X - entry.eye.X,
					Y: entry.targetView.Y - entry.eye.Y,
					Z: entry.targetView.Z - entry.eye.Z,
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
	// The shot that did this has usually not been recorded yet — within a
	// tick the engine reports the damage, then the kill, then the shot — so
	// the damage waits for the end of the tick to find it.
	if isBulletWeapon(e.Weapon) {
		s.pendingHits = append(s.pendingHits, pendingHit{
			damage:   len(s.res.Damages) - 1,
			attacker: attackerID,
			tick:     tick,
			hitGroup: int(e.HitGroup),
			victim:   e.Player.Position(),
			ducking:  e.Player.IsDucking(),
		})
	}
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
			tick:       tick,
			yaw:        p.ViewDirectionX(),
			pitch:      p.ViewDirectionY(),
			eye:        eye,
			target:     target,
			targetFeet: e.Spotted.Position(),
			targetView: s.eyeAsRendered(spottedID, tick, target),
		}
		// Only treat this as a real spot/engagement when the geometry and the
		// smoke situation confirm a clear sightline — CS2's spotted flag can
		// fire through smoke, thin gaps, or the edge of vision. Checked on the
		// engine's own spot, before the on-screen anchor is substituted in;
		// that anchor was already visibility-validated when trackFOV recorded
		// it.
		if !s.visibleAt(tick, entry.eye, entry.target, entry.targetFeet) {
			next[pid] = entry
			continue
		}
		// Prefer the moment the enemy actually appeared on this spotter's
		// screen over the engine's own (late, near-centred) spotted flag.
		rate := s.parser.TickRate()
		if fe, ok := s.fovEntry[spottedID][pid]; ok {
			if rate <= 0 || float64(tick-fe.tick)/rate <= fovAnchorLookbackSecs {
				entry = fe
			}
		}
		next[pid] = entry
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

// pendingHit is a bullet's damage waiting for the end of its tick to be
// matched to the shot that fired it.
type pendingHit struct {
	damage   int
	attacker string
	tick     int
	hitGroup int
	victim   r3.Vector
	ducking  bool
}

// isBulletWeapon reports whether damage from this weapon came from a recorded
// shot: the classes onWeaponFire keeps. A grenade, fire or a knife never
// claims a bullet.
func isBulletWeapon(w *common.Equipment) bool {
	if w == nil {
		return false
	}
	switch w.Class() {
	case common.EqClassPistols, common.EqClassSMG, common.EqClassHeavy, common.EqClassRifle:
		return true
	}
	return false
}

// resolveHits matches the tick's bullet damage to shots, once every shot of
// the tick is in. Called at the end of each tick, and before anything that
// forgets shots.
func (s *state) resolveHits() {
	for _, h := range s.pendingHits {
		s.resolveHit(h)
	}
	s.pendingHits = s.pendingHits[:0]
}

// resolveHit backfills the firing shot's outcome (for the 3D tracer colour) and
// end point, and the damage's spray flag from that shot.
func (s *state) resolveHit(h pendingHit) {
	idx, ok := s.shotForHit(h.attacker, h.tick)
	if !ok {
		return
	}
	sh := &s.res.ShotsFired[idx]
	if sh.Result == "" {
		if h.hitGroup == 1 {
			sh.Result = "headshot"
		} else {
			sh.Result = "hit"
		}
		ix, iy, iz := impactPoint(h.victim, h.ducking, sh, h.hitGroup)
		sh.ImpactX = f32ptr(ix)
		sh.ImpactY = f32ptr(iy)
		sh.ImpactZ = f32ptr(iz)
	}
	if h.damage >= 0 && h.damage < len(s.res.Damages) {
		s.res.Damages[h.damage].FromSpray = sh.IsSpray && sh.EnemySpotted
	}
}

// shotForHit is the shot behind a bullet's damage: the attacker's unclaimed
// shot on the damage's own tick, else their latest shot inside the spray
// window. That latest shot may already be claimed — a second player hit by the
// same bullet through a wall — and then it is still the answer, so the second
// victim does not turn an earlier miss into a hit.
func (s *state) shotForHit(attacker string, tick int) (int, bool) {
	found := -1
	for i := len(s.res.ShotsFired) - 1; i >= 0; i-- {
		sh := &s.res.ShotsFired[i]
		if sh.Tick < tick {
			break
		}
		if sh.Tick == tick && sh.AttackerSteamID == attacker && sh.Result == "" {
			found = i
		}
	}
	if found >= 0 {
		return found, true
	}
	last, ok := s.lastShot[attacker]
	if !ok || last.idx < 0 || last.idx >= len(s.res.ShotsFired) || last.tick > tick {
		return 0, false
	}
	if s.tickRate > 0 && float64(tick-last.tick)/s.tickRate >= sprayWindowSecs {
		return 0, false
	}
	return last.idx, true
}

// Heights above the victim's origin (their feet) that each hit group sits at,
// so a leg shot terminates at a leg rather than at eye level. Source hit group
// ids: 1 head, 2 chest, 3 stomach, 4/5 arms, 6/7 legs.
func hitGroupHeight(hitGroup int) float64 {
	switch hitGroup {
	case 1:
		return 68
	case 3:
		return 44
	case 6, 7:
		return 22
	default:
		return 55
	}
}

// impactPoint is where the round met the victim: their body at the height of
// the hit group, pulled back toward the shooter by roughly half a player's
// width so the tracer stops on the near surface instead of running through to
// the far side, which read as the shot sailing past them.
func impactPoint(pos r3.Vector, ducking bool, sh *EventShotFired, hitGroup int) (float64, float64, float64) {
	height := hitGroupHeight(hitGroup)
	if ducking {
		height *= 0.72
	}
	x, y, z := pos.X, pos.Y, pos.Z+height
	if sh.EyeX == nil || sh.EyeY == nil || sh.EyeZ == nil {
		return x, y, z
	}
	dx, dy, dz := float64(*sh.EyeX)-x, float64(*sh.EyeY)-y, float64(*sh.EyeZ)-z
	dist := math.Sqrt(dx*dx + dy*dy + dz*dz)
	if dist < 1 {
		return x, y, z
	}
	const bodyRadius = 16
	return x + dx/dist*bodyRadius, y + dy/dist*bodyRadius, z + dz/dist*bodyRadius
}
