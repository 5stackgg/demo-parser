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
	// clInterpSecs is how far behind the server an opponent is drawn on a
	// client (cl_interp / cl_interp_ratio, ~2 ticks at 64). Aim angles are
	// measured against that rewound position, not the server's current one.
	clInterpSecs = 0.03125
	// blindCutoffSecs: with more than this much flash left, the player is
	// treated as unable to see.
	blindCutoffSecs = 1.0
	// eyeHistoryTicks is how many ticks of eye positions to retain per player
	// — enough to cover clInterpSecs at 128 tick with room to spare.
	eyeHistoryTicks = 8
)

// recordEye appends this tick's eye position to a player's history ring.
func (s *state) recordEye(sid string, tick int, eye r3.Vector) {
	h := append(s.eyeHistory[sid], eyeSample{tick: tick, eye: eye})
	if len(h) > eyeHistoryTicks {
		h = h[len(h)-eyeHistoryTicks:]
	}
	s.eyeHistory[sid] = h
}

// eyeAsRendered returns where a player's eyes were clInterpSecs before the
// given tick — the position an opponent's client was drawing them at. Falls
// back to the newest sample when the history doesn't reach far enough back.
func (s *state) eyeAsRendered(sid string, tick int, fallback r3.Vector) r3.Vector {
	h := s.eyeHistory[sid]
	if len(h) == 0 {
		return fallback
	}
	if s.tickRate <= 0 {
		return h[len(h)-1].eye
	}
	want := float64(tick) - clInterpSecs*s.tickRate
	if want <= float64(h[0].tick) {
		return h[0].eye
	}
	for i := len(h) - 1; i > 0; i-- {
		if float64(h[i-1].tick) <= want && want <= float64(h[i].tick) {
			span := float64(h[i].tick - h[i-1].tick)
			if span <= 0 {
				return h[i].eye
			}
			f := (want - float64(h[i-1].tick)) / span
			a, b := h[i-1].eye, h[i].eye
			return r3.Vector{
				X: a.X + (b.X-a.X)*f,
				Y: a.Y + (b.Y-a.Y)*f,
				Z: a.Z + (b.Z-a.Z)*f,
			}
		}
	}
	return h[len(h)-1].eye
}

// fovAnchorLookbackSecs bounds how stale a buffered on-screen anchor may be.
// Tied to reactionCapSecs: an anchor older than the reaction cap could only
// produce a spot→damage that gets discarded anyway.
const fovAnchorLookbackSecs = reactionCapSecs

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
			// A blinded attacker isn't tracking anything.
			if af.blindRemaining > blindCutoffSecs {
				continue
			}
			// Tracking = time on a *visible* target. Frames where the victim
			// is behind geometry or smoke don't count (toward either numerator
			// or denominator), so peeking in/out doesn't dilute the ratio.
			if !s.visibleAt(tick, af.eye, vf.eye, vf.pos) {
				continue
			}
			view := viewVector(af.yaw, af.pitch)
			seen := s.eyeAsRendered(victim, tick, vf.eye)
			dir := r3.Vector{X: seen.X - af.eye.X, Y: seen.Y - af.eye.Y, Z: seen.Z - af.eye.Z}
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
	tick := s.parser.GameState().IngameTick()
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
		seen := s.eyeAsRendered(eng.victim, tick, vf.eye)
		dir := r3.Vector{X: seen.X - eye.X, Y: seen.Y - eye.Y, Z: seen.Z - eye.Z}
		ang := angleBetweenDeg(view, dir)
		if ang < bestAng {
			bestAng, best = ang, eng
		}
	}
	if best == nil || bestAng > firstShotConeDeg {
		return
	}
	best.firstShotFired = true
	best.firstShotTick = tick
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
