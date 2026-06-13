package parser

import (
	"github.com/golang/geo/r3"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
)

const fovWideDeg = 45.0
const fovTightDeg = 40.0

func (s *state) trackFOV() {
	if !s.matchStarted {
		return
	}
	tick := s.parser.GameState().IngameTick()

	type pinfo struct {
		sid        string
		eye        r3.Vector
		view       r3.Vector
		yaw, pitch float32
		team       common.Team
	}
	var infos []pinfo
	for _, p := range s.parser.GameState().Participants().Playing() {
		if p == nil || !p.IsAlive() {
			continue
		}
		sid := steamIDStr(p)
		if sid == "" {
			continue
		}
		eye, _ := p.PositionEyes()
		yaw, pitch := p.ViewDirectionX(), p.ViewDirectionY()
		infos = append(infos, pinfo{sid, eye, viewVector(yaw, pitch), yaw, pitch, p.Team})
	}

	seen := map[[2]string]bool{}
	for _, a := range infos {
		for _, v := range infos {
			if a.sid == v.sid || a.team == v.team {
				continue
			}
			dir := r3.Vector{X: v.eye.X - a.eye.X, Y: v.eye.Y - a.eye.Y, Z: v.eye.Z - a.eye.Z}
			angle := angleBetweenDeg(a.view, dir)
			if angle > fovWideDeg {
				continue
			}
			key := [2]string{v.sid, a.sid}
			seen[key] = true
			entry := visEntry{tick: tick, yaw: a.yaw, pitch: a.pitch, eye: a.eye, target: v.eye}
			if s.fovEntryWide[v.sid] == nil {
				s.fovEntryWide[v.sid] = map[string]visEntry{}
			}
			if _, had := s.fovEntryWide[v.sid][a.sid]; !had {
				s.fovEntryWide[v.sid][a.sid] = entry
			}
			if angle <= fovTightDeg {
				if s.fovEntryTight[v.sid] == nil {
					s.fovEntryTight[v.sid] = map[string]visEntry{}
				}
				if _, had := s.fovEntryTight[v.sid][a.sid]; !had {
					s.fovEntryTight[v.sid][a.sid] = entry
				}
			}
		}
	}

	for vsid, m := range s.fovEntryWide {
		for asid := range m {
			if !seen[[2]string{vsid, asid}] {
				delete(m, asid)
				if s.fovEntryTight[vsid] != nil {
					delete(s.fovEntryTight[vsid], asid)
				}
			}
		}
	}
}
