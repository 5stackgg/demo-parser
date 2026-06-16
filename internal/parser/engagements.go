package parser

import (
	"github.com/golang/geo/r3"
)

// trackingConeDeg is how close (in degrees) the attacker's view vector
// must be to the live victim to count a frame as "on target".
const trackingConeDeg = 5.0

// maxEngagementSecs caps how long an engagement stays open after first
// sight. Beyond this it's a hold-angle / off-target situation, not the
// reaction+tracking window we want to measure.
const maxEngagementSecs = 5.0

// firstShotConeDeg gates first-shot attribution: a shot only counts as an
// engagement's first shot when the crosshair is within this cone of the
// victim. Without it any shot fired while an engagement is open (even one
// aimed ~180° away) would mark the first shot.
const firstShotConeDeg = 30.0

const (
	// sprayWindowSecs: a shot following the same attacker's previous shot
	// within this window is a held-trigger spray; also the window in which
	// damage is attributed back to its firing shot / first-shot hit.
	sprayWindowSecs = 0.25
	// reactionFloorSecs: spot→damage faster than this means the attacker was
	// pre-aimed, not reacting.
	reactionFloorSecs = 0.2
	// reactionCapSecs: spot→damage slower than this is a hold-angle play, not
	// a reaction engagement.
	reactionCapSecs = 3.0
	// fovEntryRecentSecs: how long a buffered FOV-entry spot stays usable as
	// the engagement's true first-sight tick.
	fovEntryRecentSecs = 1.5
)

// openEngagement starts tracking attacker→victim on first sight.
func (s *state) openEngagement(attacker, victim string, e visEntry) {
	if !s.liveRound || attacker == "" || victim == "" {
		return
	}
	if s.engagements[attacker] == nil {
		s.engagements[attacker] = map[string]*engagement{}
	}
	if _, exists := s.engagements[attacker][victim]; exists {
		return
	}
	s.engagements[attacker][victim] = &engagement{
		attacker: attacker,
		victim:   victim,
		round:    s.currentRound,
		spotTick: e.tick,
	}
}

// trackEngagements runs each frame: for every open engagement with both
// players alive, accumulate a tracking sample and close on timeout.
func (s *state) trackEngagements() {
	rate := s.parser.TickRate()
	tick := s.parser.GameState().IngameTick()
	for attacker, m := range s.engagements {
		af, aok := s.frames[attacker]
		for victim, eng := range m {
			if rate > 0 && float64(tick-eng.spotTick)/rate > maxEngagementSecs {
				s.closeEngagement(attacker, victim)
				continue
			}
			vf, vok := s.frames[victim]
			if !aok || !vok || !af.alive || !vf.alive {
				continue
			}
			// Tracking = time on a *visible* target. Frames where the victim
			// is behind geometry don't count (toward either numerator or
			// denominator), so peeking in/out doesn't dilute the ratio.
			if !s.los(af.eye, vf.eye) {
				continue
			}
			view := viewVector(af.yaw, af.pitch)
			dir := r3.Vector{X: vf.eye.X - af.eye.X, Y: vf.eye.Y - af.eye.Y, Z: vf.eye.Z - af.eye.Z}
			eng.totalFrames++
			if angleBetweenDeg(view, dir) <= trackingConeDeg {
				eng.onTargetFrames++
			}
		}
	}
}

// recordEngagementShot attributes a shot to the open engagement whose victim
// is nearest the crosshair and marks its first shot (for first-bullet accuracy).
func (s *state) recordEngagementShot(attacker string, eye r3.Vector, yaw, pitch float32, weapon string) {
	m := s.engagements[attacker]
	if len(m) == 0 {
		return
	}
	view := viewVector(yaw, pitch)
	var best *engagement
	bestAng := float32(360)
	for _, eng := range m {
		if eng.firstShotFired {
			continue
		}
		vf, ok := s.frames[eng.victim]
		if !ok || !vf.alive {
			continue
		}
		dir := r3.Vector{X: vf.eye.X - eye.X, Y: vf.eye.Y - eye.Y, Z: vf.eye.Z - eye.Z}
		ang := angleBetweenDeg(view, dir)
		if ang < bestAng {
			bestAng, best = ang, eng
		}
	}
	if best == nil || bestAng > firstShotConeDeg {
		return
	}
	best.firstShotFired = true
	best.firstShotTick = s.parser.GameState().IngameTick()
	best.weaponClass = weaponClass(weapon)
}

func (s *state) closeEngagement(attacker, victim string) {
	m := s.engagements[attacker]
	if m == nil {
		return
	}
	eng, ok := m[victim]
	if !ok {
		return
	}
	delete(m, victim)
	if len(m) == 0 {
		delete(s.engagements, attacker)
	}
	s.flushEngagement(eng)
}

// flushEngagement emits a closed engagement, skipping ones with no signal
// (never fired and never tracked).
func (s *state) flushEngagement(eng *engagement) {
	if eng == nil || (!eng.firstShotFired && eng.totalFrames == 0) {
		return
	}
	s.res.AimEngagements = append(s.res.AimEngagements, EventAimEngagement{
		AttackerSteamID: eng.attacker,
		Round:           eng.round,
		FirstShotFired:  eng.firstShotFired,
		FirstShotHit:    eng.firstShotHit,
		OnTargetFrames:  eng.onTargetFrames,
		TotalFrames:     eng.totalFrames,
		WeaponClass:     eng.weaponClass,
	})
}

// closeEngagementsFor flushes every engagement that the given player is
// part of (as attacker or victim) — used when the player dies.
func (s *state) closeEngagementsFor(sid string) {
	if sid == "" {
		return
	}
	for victim := range s.engagements[sid] {
		s.closeEngagement(sid, victim)
	}
	for attacker := range s.engagements {
		if _, ok := s.engagements[attacker][sid]; ok {
			s.closeEngagement(attacker, sid)
		}
	}
}

// closeAllEngagements flushes everything still open (round end / finalize).
func (s *state) closeAllEngagements() {
	for _, m := range s.engagements {
		for _, eng := range m {
			s.flushEngagement(eng)
		}
	}
	s.engagements = map[string]map[string]*engagement{}
}
