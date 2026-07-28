package parser

import (
	"testing"

	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
)

func newSideState() *state {
	return &state{
		res:              &Result{},
		playerStartSides: map[string]string{},
	}
}

func TestRecordPlayerStartSide(t *testing.T) {
	tests := []struct {
		name   string
		player *common.Player
		want   string
	}{
		{
			name:   "terrorist",
			player: &common.Player{SteamID64: 1, Team: common.TeamTerrorists},
			want:   "t",
		},
		{
			name:   "counter terrorist",
			player: &common.Player{SteamID64: 2, Team: common.TeamCounterTerrorists},
			want:   "ct",
		},
		{
			name:   "spectator is not a side",
			player: &common.Player{SteamID64: 3, Team: common.TeamSpectators},
			want:   "",
		},
		{
			name:   "unassigned is not a side",
			player: &common.Player{SteamID64: 4, Team: common.TeamUnassigned},
			want:   "",
		},
		{
			name:   "bot",
			player: &common.Player{SteamID64: 5, Team: common.TeamTerrorists, IsBot: true},
			want:   "",
		},
		{
			name:   "no steam id",
			player: &common.Player{SteamID64: 0, Team: common.TeamTerrorists},
			want:   "",
		},
		{
			name:   "nil player",
			player: nil,
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSideState()
			s.recordPlayerStartSide(tt.player)

			got := ""
			if tt.player != nil {
				got = s.playerStartSides[steamIDStr(tt.player)]
			}
			if got != tt.want {
				t.Fatalf("start side = %q, want %q", got, tt.want)
			}
		})
	}
}

// Sides swap at halftime, so only the first observation may stick.
func TestRecordPlayerStartSideFirstWriteWins(t *testing.T) {
	s := newSideState()
	p := &common.Player{SteamID64: 42, Team: common.TeamTerrorists}

	s.recordPlayerStartSide(p)
	p.Team = common.TeamCounterTerrorists
	s.recordPlayerStartSide(p)

	if got := s.playerStartSides["42"]; got != "t" {
		t.Fatalf("start side = %q, want %q (halftime swap must not overwrite)", got, "t")
	}
}

// A player who is unassigned on their first round must stay eligible, or
// anyone who spawns in late is left with no side at all.
func TestRecordPlayerStartSideSkipsUnassignedUntilTeamed(t *testing.T) {
	s := newSideState()
	p := &common.Player{SteamID64: 7, Team: common.TeamUnassigned}

	s.recordPlayerStartSide(p)
	if _, ok := s.playerStartSides["7"]; ok {
		t.Fatal("unassigned player must not be recorded")
	}

	p.Team = common.TeamCounterTerrorists
	s.recordPlayerStartSide(p)

	if got := s.playerStartSides["7"]; got != "ct" {
		t.Fatalf("start side = %q, want %q", got, "ct")
	}
}
