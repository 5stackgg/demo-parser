package parser

import "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"

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
		Headshot:      e.IsHeadshot,
		WallBang:      e.IsWallBang(),
		NoScope:       e.NoScope,
		ThroughSmoke:  e.ThroughSmoke,
	}
	if e.Killer != nil {
		k.KillerTeam = teamCode(e.Killer.Team)
	}
	if e.Victim != nil {
		k.VictimTeam = teamCode(e.Victim.Team)
	}
	if e.Weapon != nil {
		k.Weapon = e.Weapon.String()
	}
	s.res.Kills = append(s.res.Kills, k)
}
