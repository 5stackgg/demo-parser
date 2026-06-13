package parser

import (
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"
)

// Killer/victim may be nil on world damage or partially corrupt
// demos; record what we can either way.
func (s *state) onKill(e events.Kill) {
	if !s.matchStarted {
		return
	}
	s.recordPlayerName(e.Killer)
	s.recordPlayerName(e.Victim)
	s.recordPlayerName(e.Assister)
	k := EventKill{
		Tick:          s.parser.GameState().IngameTick(),
		KillerSteamID: steamIDStr(e.Killer),
		VictimSteamID: steamIDStr(e.Victim),
		AssistSteamID: steamIDStr(e.Assister),
		AssistFlash:   e.AssistedFlash,
		Headshot:      e.IsHeadshot,
		WallBang:      e.IsWallBang(),
		NoScope:       e.NoScope,
		ThroughSmoke:  e.ThroughSmoke,
	}
	if e.Killer != nil {
		k.KillerTeam = teamCode(e.Killer.Team)
		kpos := e.Killer.Position()
		k.AttackerX = f32ptr(kpos.X)
		k.AttackerY = f32ptr(kpos.Y)
		k.AttackerZ = f32ptr(kpos.Z)
	}
	if e.Victim != nil {
		k.VictimTeam = teamCode(e.Victim.Team)
		vpos := e.Victim.Position()
		k.VictimX = f32ptr(vpos.X)
		k.VictimY = f32ptr(vpos.Y)
		k.VictimZ = f32ptr(vpos.Z)
		util := grenadeValue(e.Victim)
		k.VictimUtilityValue = &util
	}
	if e.Weapon != nil {
		k.Weapon = weaponCanonical(e.Weapon)
	}
	s.res.Kills = append(s.res.Kills, k)

	// A CT carrying a defuse kit drops it on death — record the spot
	// so the replay can render a kit icon at that location until
	// another CT picks it up. (We don't yet track pickup; for now the
	// renderer keeps the marker for the rest of the round.)
	if e.Victim != nil &&
		e.Victim.Team == common.TeamCounterTerrorists &&
		e.Victim.HasDefuseKit() {
		pos := e.Victim.Position()
		s.res.KitDrops = append(s.res.KitDrops, EventKitDrop{
			Tick:   s.parser.GameState().IngameTick(),
			Round:  s.currentRound,
			Player: steamIDStr(e.Victim),
			X:      float32(pos.X),
			Y:      float32(pos.Y),
			Z:      float32(pos.Z),
		})
	}
}
