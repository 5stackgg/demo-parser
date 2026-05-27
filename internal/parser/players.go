package parser

import (
	"strconv"

	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/msg"
)

func (s *state) onServerInfo(m *msg.CSVCMsg_ServerInfo) {
	name := m.GetMapName()
	if name == "" {
		return
	}
	s.res.MapName = name
	if mm := workshopMapRe.FindStringSubmatch(name); len(mm) == 2 {
		s.res.WorkshopID = mm[1]
	}
}

// The demo's userinfo string-table is the authoritative source for
// (steam_id, name) pairs — the same data CS2 ships to connected
// clients. Every player who ever connected gets a row, even if they
// disconnected before any kill event fired. Hooking events.PlayerInfo
// keeps disconnected players in res.Players.
func (s *state) onPlayerInfo(e events.PlayerInfo) {
	info := e.Info
	if info.IsFakePlayer || info.GUID == "BOT" {
		return
	}
	if info.XUID == 0 || info.Name == "" {
		return
	}
	s.playerNames[strconv.FormatUint(info.XUID, 10)] = info.Name
}

func (s *state) onPlayerConnect(e events.PlayerConnect) {
	s.recordPlayerName(e.Player)
}

func (s *state) onPlayerNameChange(e events.PlayerNameChange) {
	s.recordPlayerName(e.Player)
}

type playerRank struct {
	rank     int
	rankType int
}

func (s *state) recordPlayerRank(p *common.Player) {
	if p == nil || p.IsBot {
		return
	}
	sid := steamIDStr(p)
	if sid == "" {
		return
	}
	rt := p.RankType()
	r := p.Rank()
	if rt <= 0 && r <= 0 {
		return
	}
	s.playerRanks[sid] = playerRank{rank: r, rankType: rt}
}

func (s *state) recordPlayerName(p *common.Player) {
	if p == nil || p.IsBot {
		return
	}
	sid := steamIDStr(p)
	if sid == "" {
		return
	}
	name := p.Name
	// "unknown" is the demoinfocs placeholder for GOTV players whose
	// raw player-info row is missing. Skip so a transient lookup
	// failure doesn't shadow a later valid name.
	if name == "" || name == "unknown" {
		return
	}
	// Last write wins — players can rename mid-match.
	s.playerNames[sid] = name
}
