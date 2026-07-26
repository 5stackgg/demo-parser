package parser

import (
	"github.com/golang/geo/r3"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"
)

// liveBombPos returns the bomb's current world position — either the
// carrier's position or LastOnGroundPosition. Returns a zero vector
// (and false) if the GameState() doesn't know about the bomb yet.
func (s *state) liveBombPos() (r3.Vector, bool) {
	b := s.parser.GameState().Bomb()
	if b == nil {
		return r3.Vector{}, false
	}
	return b.Position(), true
}

// Bomb events — three distinct types collapsed into a single list
// with a `type` discriminator. Reads naturally on a frontend as one
// timeline.

func (s *state) onBombPlanted(e events.BombPlanted) {
	if !s.matchStarted {
		return
	}
	ev := EventBomb{
		Tick:   s.parser.GameState().IngameTick(),
		Type:   "planted",
		Player: steamIDStr(e.Player),
		Site:   bombSiteCode(e.Site),
	}
	if pos, ok := s.liveBombPos(); ok {
		ev.X, ev.Y, ev.Z = float32(pos.X), float32(pos.Y), float32(pos.Z)
	}
	s.res.Bombs = append(s.res.Bombs, ev)
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
	ev := EventBomb{
		Tick: s.parser.GameState().IngameTick(),
		Type: "exploded",
		Site: bombSiteCode(e.Site),
	}
	// Carry the detonation point on the event itself so consumers don't have to
	// join back to the plant, and so the blast can displace smoke like an HE
	// does — on a far larger scale.
	if pos, ok := s.liveBombPos(); ok {
		ev.X, ev.Y, ev.Z = float32(pos.X), float32(pos.Y), float32(pos.Z)
		s.recordBlast(ev.Tick, pos, c4BlastRadius, c4BlastFullRadius)
	} else if p, ok := s.lastPlantPos(); ok {
		ev.X, ev.Y, ev.Z = float32(p.X), float32(p.Y), float32(p.Z)
		s.recordBlast(ev.Tick, p, c4BlastRadius, c4BlastFullRadius)
	}
	s.res.Bombs = append(s.res.Bombs, ev)
}

// lastPlantPos recovers the plant location from the round's own bomb timeline.
// The bomb entity is gone by the time it detonates on some demos, so
// liveBombPos can come back empty.
func (s *state) lastPlantPos() (r3.Vector, bool) {
	for i := len(s.res.Bombs) - 1; i >= 0; i-- {
		b := s.res.Bombs[i]
		if b.Type != "planted" {
			continue
		}
		if b.X == 0 && b.Y == 0 && b.Z == 0 {
			return r3.Vector{}, false
		}
		return r3.Vector{X: float64(b.X), Y: float64(b.Y), Z: float64(b.Z)}, true
	}
	return r3.Vector{}, false
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
	ev := EventBomb{
		Tick:   s.parser.GameState().IngameTick(),
		Type:   "dropped",
		Player: steamIDStr(e.Player),
	}
	// Bomb position right at drop time = where the player was standing.
	// `e.Player.Position()` is more reliable than reading `GameState().
	// Bomb().Position()` during the event itself, since the bomb entity
	// may not yet be marked as on-ground.
	if e.Player != nil {
		pos := e.Player.Position()
		ev.X, ev.Y, ev.Z = float32(pos.X), float32(pos.Y), float32(pos.Z)
	} else if pos, ok := s.liveBombPos(); ok {
		ev.X, ev.Y, ev.Z = float32(pos.X), float32(pos.Y), float32(pos.Z)
	}
	s.res.Bombs = append(s.res.Bombs, ev)
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
