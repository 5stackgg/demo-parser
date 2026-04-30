package parser

import "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"

// Bomb events — three distinct types collapsed into a single list
// with a `type` discriminator. Reads naturally on a frontend as one
// timeline.

func (s *state) onBombPlanted(e events.BombPlanted) {
	if !s.matchStarted {
		return
	}
	s.res.Bombs = append(s.res.Bombs, EventBomb{
		Tick:   s.parser.GameState().IngameTick(),
		Type:   "planted",
		Player: steamIDStr(e.Player),
		Site:   bombSiteCode(e.Site),
	})
}

func (s *state) onBombDefused(e events.BombDefused) {
	if !s.matchStarted {
		return
	}
	s.res.Bombs = append(s.res.Bombs, EventBomb{
		Tick:   s.parser.GameState().IngameTick(),
		Type:   "defused",
		Player: steamIDStr(e.Player),
		Site:   bombSiteCode(e.Site),
	})
}

func (s *state) onBombExplode(e events.BombExplode) {
	if !s.matchStarted {
		return
	}
	s.res.Bombs = append(s.res.Bombs, EventBomb{
		Tick: s.parser.GameState().IngameTick(),
		Type: "exploded",
		Site: bombSiteCode(e.Site),
	})
}
