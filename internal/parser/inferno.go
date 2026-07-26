package parser

import "sort"

// Molotov and incendiary fire needs no reconstruction. Unlike smoke — where the
// demo carries only the simulation inputs and we have to flood the map
// ourselves — an inferno networks the position of every individual flame, each
// with its own burning flag. That is ground truth: the exact ground the fire
// denied, at the exact moments it denied it.
//
// It also means the interactions come for free. A smoke thrown onto a molotov
// puts it out, and the engine simply stops flagging those flames as burning, so
// the recorded footprint shrinks the way it did in the match without us
// modelling anything.
const (
	// infernoSampleTicks throttles how often the flame set is re-read. Fires
	// spread over about a second, so a few times per second resolves the spread
	// without paying for a property walk every frame.
	infernoSampleTicks = 8
)

// infernoFireTrack is one flame's lifetime within an inferno.
type infernoFireTrack struct {
	x, y, z   float32
	startTick int
	endTick   int
}

// infernoTrack accumulates one inferno across its life.
type infernoTrack struct {
	id        int
	round     int
	thrower   string
	team      string
	startTick int
	// Keyed by the flame's index within the inferno, which is stable as the
	// fire spreads — new flames are appended rather than reordered.
	fires map[int]*infernoFireTrack
}

// trackInfernos samples every live inferno's flames. Called from onFrameDone.
func (s *state) trackInfernos() {
	if !s.matchStarted {
		return
	}
	tick := s.parser.GameState().IngameTick()
	if tick-s.lastInfernoSample < infernoSampleTicks {
		return
	}
	s.lastInfernoSample = tick

	for _, inf := range s.parser.GameState().Infernos() {
		if inf == nil || inf.Entity == nil {
			continue
		}
		id := int(inf.UniqueID())
		tr := s.infernos[id]
		if tr == nil {
			tr = &infernoTrack{
				id:        id,
				round:     s.currentRound,
				startTick: tick,
				fires:     map[int]*infernoFireTrack{},
			}
			if th := inf.Thrower(); th != nil {
				tr.thrower = steamIDStr(th)
				tr.team = teamCode(th.Team)
			}
			s.infernos[id] = tr
		}

		for i, f := range inf.Fires().List() {
			if !f.IsBurning {
				continue
			}
			ft := tr.fires[i]
			if ft == nil {
				ft = &infernoFireTrack{
					x:         float32(f.X),
					y:         float32(f.Y),
					z:         float32(f.Z),
					startTick: tick,
				}
				tr.fires[i] = ft
			}
			// Extended while it keeps burning; whatever it last held is the
			// tick it went out, whether that was the fire running its course or
			// a smoke landing on it.
			ft.endTick = tick
		}
	}
}

// The inferno entity outlives its fire by a long way — measured at a median of
// ~15s after the last flame went out — so the burn's end has to come from the
// flames themselves, not from when the entity disappears.

// flushInfernos emits every tracked inferno, ordered so output is stable.
func (s *state) flushInfernos() {
	if len(s.infernos) == 0 {
		return
	}
	ids := make([]int, 0, len(s.infernos))
	for id := range s.infernos {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	for _, id := range ids {
		tr := s.infernos[id]
		if len(tr.fires) == 0 {
			continue
		}
		idxs := make([]int, 0, len(tr.fires))
		for i := range tr.fires {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)

		ev := EventInferno{
			ID:             tr.id,
			Round:          tr.round,
			ThrowerSteamID: tr.thrower,
			ThrowerTeam:    tr.team,
			StartTick:      tr.startTick,
			Fires:          make([]EventInfernoFire, 0, len(idxs)),
		}
		first, last := 0, 0
		for _, i := range idxs {
			f := tr.fires[i]
			if first == 0 || f.startTick < first {
				first = f.startTick
			}
			if f.endTick > last {
				last = f.endTick
			}
			ev.Fires = append(ev.Fires, EventInfernoFire{
				X:         f.x,
				Y:         f.y,
				Z:         f.z,
				StartTick: f.startTick,
				EndTick:   f.endTick,
			})
		}
		ev.StartTick = first
		ev.EndTick = last
		s.res.Infernos = append(s.res.Infernos, ev)
		s.infernoFires += len(ev.Fires)
	}
	s.infernos = map[int]*infernoTrack{}
}
