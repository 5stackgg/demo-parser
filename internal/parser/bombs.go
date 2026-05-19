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

func (s *state) onBombPlantBegin(e events.BombPlantBegin) {
	if !s.matchStarted {
		return
	}
	s.res.Bombs = append(s.res.Bombs, EventBomb{
		Tick:   s.parser.GameState().IngameTick(),
		Type:   "plant_begin",
		Player: steamIDStr(e.Player),
		Site:   bombSiteCode(e.Site),
	})
}

func (s *state) onBombPlantAborted(e events.BombPlantAborted) {
	if !s.matchStarted {
		return
	}
	s.res.Bombs = append(s.res.Bombs, EventBomb{
		Tick:   s.parser.GameState().IngameTick(),
		Type:   "plant_abort",
		Player: steamIDStr(e.Player),
	})
}

func (s *state) onBombDefuseStart(e events.BombDefuseStart) {
	if !s.matchStarted {
		return
	}
	s.res.Bombs = append(s.res.Bombs, EventBomb{
		Tick:   s.parser.GameState().IngameTick(),
		Type:   "defuse_begin",
		Player: steamIDStr(e.Player),
		HasKit: e.HasKit,
	})
}

func (s *state) onBombDefuseAborted(e events.BombDefuseAborted) {
	if !s.matchStarted {
		return
	}
	s.res.Bombs = append(s.res.Bombs, EventBomb{
		Tick:   s.parser.GameState().IngameTick(),
		Type:   "defuse_abort",
		Player: steamIDStr(e.Player),
	})
}

func (s *state) onBombDropped(e events.BombDropped) {
	if !s.matchStarted {
		return
	}
	s.res.Bombs = append(s.res.Bombs, EventBomb{
		Tick:   s.parser.GameState().IngameTick(),
		Type:   "dropped",
		Player: steamIDStr(e.Player),
	})
}

func (s *state) onBombPickup(e events.BombPickup) {
	if !s.matchStarted {
		return
	}
	s.res.Bombs = append(s.res.Bombs, EventBomb{
		Tick:   s.parser.GameState().IngameTick(),
		Type:   "pickup",
		Player: steamIDStr(e.Player),
	})
}
