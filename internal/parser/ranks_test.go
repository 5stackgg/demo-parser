package parser

import (
	"testing"

	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/msg"
	st "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/sendtables"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/sendtables/fake"
	"google.golang.org/protobuf/proto"
)

func newRankState() *state {
	return &state{
		res:         &Result{},
		playerRanks: map[string]playerRank{},
	}
}

func scoreboardPlayer(steamID uint64, rankType, rank int32) *common.Player {
	entity := new(fake.Entity)
	entity.On("PropertyValueMust", "m_iCompetitiveRankType").Return(st.PropertyValue{Any: rankType})
	entity.On("PropertyValueMust", "m_iCompetitiveRanking").Return(st.PropertyValue{Any: rank})
	return &common.Player{SteamID64: steamID, Entity: entity}
}

func rankUpdate(accountID int32, rankType int32, oldRank, newRank int32) *msg.CCSUsrMsg_ServerRankUpdate {
	return &msg.CCSUsrMsg_ServerRankUpdate{
		RankUpdate: []*msg.CCSUsrMsg_ServerRankUpdate_RankUpdate{{
			AccountId:  proto.Int32(accountID),
			RankOld:    proto.Int32(oldRank),
			RankNew:    proto.Int32(newRank),
			NumWins:    proto.Int32(3),
			RankTypeId: proto.Int32(rankType),
		}},
	}
}

func TestServerRankUpdateTakesTheMessageRankType(t *testing.T) {
	s := newRankState()
	s.onServerRankUpdate(rankUpdate(12345, 6, 9, 10))

	sid := "76561197960278073"
	got := s.playerRanks[sid]
	if got.rankType != 6 || got.rank != 10 || got.previousRank != 9 || got.winCount != 3 {
		t.Fatalf("rank = %+v, want type 6 rank 10 previous 9 wins 3", got)
	}
}

// The scoreboard of a Wingman or Rush match still carries the Premier rating,
// so the finalize sweep must not relabel the ladder the rank update moved.
func TestScoreboardDoesNotOverrideRankUpdate(t *testing.T) {
	s := newRankState()
	s.onServerRankUpdate(rankUpdate(12345, 6, 9, 10))
	s.recordPlayerRank(scoreboardPlayer(76561197960278073, 11, 15000))

	got := s.playerRanks["76561197960278073"]
	if got.rankType != 6 || got.rank != 10 {
		t.Fatalf("rank = %+v, want type 6 rank 10", got)
	}
}

func TestScoreboardRankWithoutUpdate(t *testing.T) {
	s := newRankState()
	s.recordPlayerRank(scoreboardPlayer(76561197960278073, 11, 15000))

	got := s.playerRanks["76561197960278073"]
	if got.rankType != 11 || got.rank != 15000 || got.hasPrevious {
		t.Fatalf("rank = %+v, want type 11 rank 15000 without previous", got)
	}
}
