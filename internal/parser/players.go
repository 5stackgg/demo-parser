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
	// A rank update is authoritative for both rank and type; the scoreboard
	// only fills in players who never got one.
	if rt > 0 && (!pr.hasPrevious || pr.rankType == 0) {
		pr.rankType = rt
	}
	if r > 0 && !pr.hasPrevious {
		pr.rank = r
	}
	s.playerRanks[sid] = pr
}

// onServerRankUpdate captures the rank change Valve emits at match end — the
// only place RankOld (pre-match rank) is available, giving an exact per-match
// delta. Each entry's rank_type_id names the ladder it moved, which can differ
// from the scoreboard's rank type (a Wingman or Rush match still shows the
// Premier rating there). demoinfocs' RankUpdate event drops that field, so the
// user message is read directly.
func (s *state) onServerRankUpdate(m *msg.CCSUsrMsg_ServerRankUpdate) {
	for _, u := range m.GetRankUpdate() {
		if u.GetAccountId() == 0 {
			continue
		}
		steamID := common.ConvertSteamID32To64(uint32(u.GetAccountId()))
		sid := strconv.FormatUint(steamID, 10)
		pr := s.playerRanks[sid]
		pr.rank = int(u.GetRankNew())
		pr.previousRank = int(u.GetRankOld())
		pr.hasPrevious = true
		pr.winCount = int(u.GetNumWins())
		if rt := int(u.GetRankTypeId()); rt > 0 {
			pr.rankType = rt
		} else if p := s.participantBySteamID(steamID); p != nil {
			if rt := p.RankType(); rt > 0 {
				pr.rankType = rt
			}
		}
		s.playerRanks[sid] = pr
		fmt.Fprintf(
			os.Stderr,
			"[rank-update] steam_id=%s old=%d new=%d change=%.2f type=%d wins=%d\n",
			sid, pr.previousRank, pr.rank, u.GetRankChange(), pr.rankType, pr.winCount,
		)
	}
}

func (s *state) participantBySteamID(steamID uint64) *common.Player {
	if s.parser == nil {
		return nil
	}
	for _, p := range s.parser.GameState().Participants().All() {
		if p != nil && p.SteamID64 == steamID {
			return p
		}
	}
	return nil
}

// Only the first write sticks — sides swap at halftime, so re-reading later
// would flip half the lineup.
func (s *state) recordPlayerStartSide(p *common.Player) {
	if p == nil || p.IsBot {
		return
	}
	sid := steamIDStr(p)
	if sid == "" {
		return
	}
	if _, ok := s.playerStartSides[sid]; ok {
		return
	}
	side := teamCode(p.Team)
	// Unassigned/spectator: wait for a round where they're on a team.
	if side == "" {
		return
	}
	s.playerStartSides[sid] = side
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
