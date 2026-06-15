package parser

import (
	"fmt"
	"os"
	"strconv"

	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/msg"
)

func (s *state) onServerInfo(m *msg.CSVCMsg_ServerInfo) {
	if host := m.GetHostName(); host != "" {
		s.res.ServerName = host
	}
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
	rank         int
	rankType     int
	previousRank int
	hasPrevious  bool
	winCount     int
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
	pr := s.playerRanks[sid]
	if rt > 0 {
		pr.rankType = rt
	}
	// RankUpdate's RankNew is authoritative; only fall back to the scoreboard
	// rank for players who never got an update event.
	if r > 0 && !pr.hasPrevious {
		pr.rank = r
	}
	s.playerRanks[sid] = pr
}

// onRankUpdate captures the rank change Valve emits at match end — the only
// place RankOld (pre-match rank) is available, giving an exact per-match delta.
func (s *state) onRankUpdate(e events.RankUpdate) {
	sid := strconv.FormatUint(e.SteamID64(), 10)
	if sid == "" || sid == "0" {
		return
	}
	pr := s.playerRanks[sid]
	pr.rank = e.RankNew
	pr.previousRank = e.RankOld
	pr.hasPrevious = true
	pr.winCount = e.WinCount
	if e.Player != nil {
		if rt := e.Player.RankType(); rt > 0 {
			pr.rankType = rt
		}
	}
	s.playerRanks[sid] = pr
	fmt.Fprintf(
		os.Stderr,
		"[rank-update] steam_id=%s old=%d new=%d change=%.2f type=%d wins=%d\n",
		sid, e.RankOld, e.RankNew, e.RankChange, pr.rankType, e.WinCount,
	)
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
