package parser

import (
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"
)

// matchStarted gates round event collection: demos always include
// warmup + (optional) knife rounds before the actual match. Counting
// those as "rounds 1..N" would mismatch the user's scoreboard.

func (s *state) onMatchStart(_ events.MatchStart) {
	s.matchStarted = true
	// CSGO fires MatchStart once. CS2 sometimes fires it again
	// post-knife — resetting currentRound here keeps post-knife
	// round 1 aligned with scoreboard round 1.
	s.currentRound = 0
	s.res.RoundTicks = s.res.RoundTicks[:0]
}

func (s *state) onRoundStart(_ events.RoundStart) {
	s.captureMaxTick()
	// Sweep all currently-connected participants to catch player
	// names that hadn't synced on m_szPlayerName when their first
	// kill event fired.
	for _, p := range s.parser.GameState().Participants().All() {
		s.recordPlayerName(p)
	}
	if !s.matchStarted {
		return
	}
	s.currentRound++
	s.currentRoundStartTick = s.parser.GameState().IngameTick()
	s.currentFreezeEndTick = 0
	s.liveRound = false
	// Entities are reused across rounds but visibility resets at
	// freeze break, so a fresh sighting always starts each round.
	s.visStart = map[string]map[string]visEntry{}
	// Sprays don't carry across rounds.
	s.lastShot = map[string]shotMark{}
	s.victimHealth = map[string]int{}
	s.fovEntryWide = map[string]map[string]visEntry{}
	s.fovEntryTight = map[string]map[string]visEntry{}
	s.res.RoundTicks = append(s.res.RoundTicks, RoundTick{
		Round:     s.currentRound,
		StartTick: s.currentRoundStartTick,
	})
}

// onRoundFreezetimeEnd marks the moment players can move/shoot. Used
// as the TTD anchor — damage during freeze (knife-out, suicide) is
// ignored downstream. Also snapshots each player's grenade inventory
// for downstream "unused utility $" calculation.
func (s *state) onRoundFreezetimeEnd(_ events.RoundFreezetimeEnd) {
	if !s.matchStarted {
		return
	}
	s.currentFreezeEndTick = s.parser.GameState().IngameTick()
	s.liveRound = true
	if n := len(s.res.RoundTicks); n > 0 {
		s.res.RoundTicks[n-1].FreezeEndTick = s.currentFreezeEndTick
	}
	for _, p := range s.parser.GameState().Participants().Playing() {
		if p == nil {
			continue
		}
		sid := steamIDStr(p)
		if sid == "" {
			continue
		}
		var flash, smoke, he, molotov, decoy int
		var primary, secondary string
		for _, w := range p.Weapons() {
			if w == nil {
				continue
			}
			switch w.Type {
			case common.EqFlash:
				flash++
			case common.EqSmoke:
				smoke++
			case common.EqHE:
				he++
			case common.EqMolotov, common.EqIncendiary:
				molotov++
			case common.EqDecoy:
				decoy++
			}
			switch w.Class() {
			case common.EqClassRifle, common.EqClassSMG, common.EqClassHeavy:
				if primary == "" {
					primary = w.String()
				}
			case common.EqClassPistols:
				if secondary == "" {
					secondary = w.String()
				}
			}
		}
		armor := p.Armor()
		helmet := p.HasHelmet()
		kit := p.Team == common.TeamCounterTerrorists && p.HasDefuseKit()
		empty := flash+smoke+he+molotov+decoy == 0 &&
			primary == "" && secondary == "" &&
			armor == 0 && !kit
		if empty {
			continue
		}
		s.res.RoundInventory = append(s.res.RoundInventory, EventRoundInventory{
			Round:           s.currentRound,
			AttackerSteamID: sid,
			Team:            teamCode(p.Team),
			Flash:           flash,
			Smoke:           smoke,
			HE:              he,
			Molotov:         molotov,
			Decoy:           decoy,
			Primary:         primary,
			Secondary:       secondary,
			Armor:           armor,
			Helmet:          helmet,
			Kit:             kit,
		})
	}
}

func (s *state) onRoundEnd(e events.RoundEnd) {
	// Note: do NOT flip liveRound here. RoundEnd fires the instant a
	// win condition is met (last kill / bomb explode / time-out) but
	// the demo timeline keeps producing useful frames through the
	// death cam and victory walkaround. Cutting position sampling at
	// RoundEnd makes the replay feel chopped — wait for
	// RoundEndOfficial below to flip the gate.
	if !s.matchStarted || len(s.res.RoundTicks) == 0 {
		return
	}
	s.lastRoundEndTick = s.parser.GameState().IngameTick()
	last := &s.res.RoundTicks[len(s.res.RoundTicks)-1]
	last.Winner = teamCode(e.Winner)
	last.Reason = int(e.Reason)

	// Snapshot team money at round end (sum of each side's accounts) the
	// same way the live game-server does, so external imports populate
	// match_map_rounds.lineup_*_money for the economy chart + buy types.
	var ctMoney, tMoney int
	for _, p := range s.parser.GameState().Participants().Playing() {
		if p == nil {
			continue
		}
		switch p.Team {
		case common.TeamCounterTerrorists:
			ctMoney += p.Money()
		case common.TeamTerrorists:
			tMoney += p.Money()
		}
	}
	last.CtMoney = &ctMoney
	last.TMoney = &tMoney
}

func (s *state) onRoundEndOfficial(_ events.RoundEndOfficial) {
	s.captureMaxTick()
	// Stop capturing per-tick data; RoundStart of the next round will
	// reset the gate again (it's already false-on-RoundStart for the
	// fresh freezetime).
	s.liveRound = false
	if !s.matchStarted || len(s.res.RoundTicks) == 0 {
		return
	}
	// RoundEndOfficial fires after the freeze-time delay following
	// RoundEnd; this is the tick the demo timeline considers the
	// round closed.
	s.res.RoundTicks[len(s.res.RoundTicks)-1].EndTick = s.parser.GameState().IngameTick()
}
