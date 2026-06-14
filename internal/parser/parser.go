// Package parser wraps markus-wa/demoinfocs-golang to extract playback
// metadata, events, and player stats from CS2 demo files.
package parser

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"

	dem "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs"
)

// state holds the mutable parser state shared by all event handlers
// across one Parse() call.
type state struct {
	parser dem.Parser
	res    *Result

	matchStarted          bool
	currentRound          int
	currentRoundStartTick int
	currentFreezeEndTick  int
	maxTick               int
	// Tick of the most recent RoundEnd event (win condition met).
	// Used to backfill the final round's EndTick to a tight bound
	// instead of s.maxTick, which spans the entire post-match
	// cinematic / victory walkaround.
	lastRoundEndTick int
	// True only between RoundFreezetimeEnd and RoundEnd — the window
	// when players can actually move and shoot. Per-tick data captured
	// outside this window (freezetime, end-of-round walkaround) is
	// discarded by the inLiveRound() gate since the replay viewer
	// auto-skips it anyway; persisting it wastes DB rows and bandwidth.
	liveRound bool

	// (spotted, spotter) → first-sight tick + spotter eye angles.
	// Set on rising edge of visibility, cleared on falling edge or
	// RoundStart, consumed by the next matching PlayerHurt.
	visStart map[string]map[string]visEntry

	// Per-player position/velocity sampled each FrameDone.
	frames map[string]playerFrame

	// Per-attacker last shot: used to flag spray shots (250ms window)
	// and inherit the spray flag onto damages.
	lastShot map[string]shotMark

	victimHealth map[string]int

	lastMoveTick map[string]int

	fovEntryWide  map[string]map[string]visEntry
	fovEntryTight map[string]map[string]visEntry

	// steam_id → display name. Flattened to res.Players at the end.
	playerNames map[string]string

	// steam_id → most recent observed rank + rank_type from the demo
	// scoreboard. Premier (rank_type=11) gives the CS Rating number.
	playerRanks map[string]playerRank

	// Last tick at which we emitted a position sample. Throttles
	// per-tick FrameDone events down to ~4Hz for the 2D replay table.
	lastPositionSampleTick int

	// Grenade projectile last-known positions, keyed by entity id.
	// demoinfocs' GrenadeEvent.Position is stale or zeroed for some
	// CS2 demos; tracking the projectile entity's own Position() each
	// frame and consulting it on the detonate event gives reliable
	// coords.
	grenadePos map[int]grenadeProjectile

	grenadeSeq int

	grenadePaths map[int][]GrenadePathPt
}

type grenadeProjectile struct {
	id          int
	x, y, z     float32
	gtype       string
	thrower     string
	team        string
	destroyTick int
	matched     bool
}

// Parse reads a CS2 demo from r and returns the playback metadata,
// events, and per-player stats. The reader must carry the entire .dem
// byte stream — chunked partial reads are not supported.
//
// ParseToEnd errors are treated as soft: a partial result is returned
// containing everything observed up to the failing tick. CS2 demos
// occasionally raise mid-stream entity-resolution errors inside
// demoinfocs; the seek bar in a frontend still benefits from the
// events collected before the abort.
func Parse(r io.Reader) (*Result, error) {
	s := &state{
		parser:        dem.NewParser(r),
		res:           &Result{},
		visStart:      map[string]map[string]visEntry{},
		frames:        map[string]playerFrame{},
		lastShot:      map[string]shotMark{},
		victimHealth:  map[string]int{},
		lastMoveTick:  map[string]int{},
		fovEntryWide:  map[string]map[string]visEntry{},
		fovEntryTight: map[string]map[string]visEntry{},
		playerNames:   map[string]string{},
		playerRanks:   map[string]playerRank{},
		grenadePos:    map[int]grenadeProjectile{},
		grenadePaths:  map[int][]GrenadePathPt{},
	}
	defer s.parser.Close()

	s.registerHandlers()

	if err := s.parser.ParseToEnd(); err != nil {
		fmt.Fprintf(os.Stderr, "parse-to-end error (returning partial result): %v\n", err)
	}

	s.finalize()
	return s.res, nil
}

func (s *state) registerHandlers() {
	s.parser.RegisterNetMessageHandler(s.onServerInfo)

	s.parser.RegisterEventHandler(s.onPlayerInfo)
	s.parser.RegisterEventHandler(s.onPlayerConnect)
	s.parser.RegisterEventHandler(s.onPlayerNameChange)
	s.parser.RegisterEventHandler(s.onRankUpdate)

	s.parser.RegisterEventHandler(s.onMatchStart)
	s.parser.RegisterEventHandler(s.onRoundStart)
	s.parser.RegisterEventHandler(s.onRoundFreezetimeEnd)
	s.parser.RegisterEventHandler(s.onRoundEnd)
	s.parser.RegisterEventHandler(s.onRoundEndOfficial)

	s.parser.RegisterEventHandler(s.onKill)

	s.parser.RegisterEventHandler(s.onBombPlanted)
	s.parser.RegisterEventHandler(s.onBombDefused)
	s.parser.RegisterEventHandler(s.onBombExplode)
	s.parser.RegisterEventHandler(s.onBombPlantBegin)
	s.parser.RegisterEventHandler(s.onBombPlantAborted)
	s.parser.RegisterEventHandler(s.onBombDefuseStart)
	s.parser.RegisterEventHandler(s.onBombDefuseAborted)
	s.parser.RegisterEventHandler(s.onBombDropped)
	s.parser.RegisterEventHandler(s.onBombPickup)

	s.parser.RegisterEventHandler(s.onFrameDone)
	s.parser.RegisterEventHandler(s.onWeaponFire)
	s.parser.RegisterEventHandler(s.onPlayerHurt)
	s.parser.RegisterEventHandler(s.onPlayerSpottersChanged)

	s.parser.RegisterEventHandler(s.onGrenadeProjectileThrow)
	s.parser.RegisterEventHandler(s.onGrenadeProjectileDestroy)
	s.parser.RegisterEventHandler(s.onHeExplode)
	s.parser.RegisterEventHandler(s.onFlashExplode)
	s.parser.RegisterEventHandler(s.onSmokeStart)
	s.parser.RegisterEventHandler(s.onFireGrenadeStart)
	s.parser.RegisterEventHandler(s.onPlayerFlashed)
}

// finalize resolves header-equivalent fields from the live parser
// state and flattens accumulated player names onto the Result. CS2
// demos don't carry tick rate / total ticks in the file header — they
// come from packets observed during ParseToEnd, so this runs even on
// partial parses.
func (s *state) finalize() {
	if rate := s.parser.TickRate(); rate > 0 {
		s.res.TickRate = rate
	}
	if t := s.parser.GameState().IngameTick(); t > s.maxTick {
		s.maxTick = t
	}
	s.res.TotalTicks = s.maxTick

	// Backfill EndTick on the final round: RoundEndOfficial does not
	// fire on the match-winning round (the engine cuts to the post-match
	// scoreboard instead of the normal freeze-time transition), leaving
	// EndTick == 0 and the round looking incomplete to consumers.
	//
	// Using s.maxTick here would extend the round's window across the
	// entire post-match cinematic (victory walkaround, MVP screen,
	// scoreboard) — downstream consumers like the highlight builder
	// would then attribute cinematic-era ticks to the final round and
	// could render clips that bleed into the winner screen. Backfill
	// from the RoundEnd tick (when the win condition was met) plus a
	// buffer matching the typical RoundEndOfficial freeze delay.
	if n := len(s.res.RoundTicks); n > 0 {
		last := &s.res.RoundTicks[n-1]
		if last.EndTick == 0 {
			end := 0
			if s.lastRoundEndTick > 0 {
				buf := 0
				if rate := s.parser.TickRate(); rate > 0 {
					buf = int(rate * 5)
				}
				end = s.lastRoundEndTick + buf
			}
			if end == 0 || (s.maxTick > 0 && end > s.maxTick) {
				if s.maxTick > 0 {
					end = s.maxTick
				}
			}
			if end > 0 {
				last.EndTick = end
			}
		}
	}

	for _, p := range s.parser.GameState().Participants().All() {
		s.recordPlayerName(p)
		s.recordPlayerRank(p)
	}

	s.captureMatchMeta()

	s.computeTrades()

	gids := make([]int, 0, len(s.grenadePaths))
	for gid := range s.grenadePaths {
		gids = append(gids, gid)
	}
	sort.Ints(gids)
	for _, gid := range gids {
		s.res.GrenadeTrajectories = append(s.res.GrenadeTrajectories, GrenadeTrajectory{GrenadeID: gid, Points: s.grenadePaths[gid]})
	}

	if len(s.playerNames) == 0 {
		return
	}
	s.res.Players = make([]PlayerInfo, 0, len(s.playerNames))
	ids := make([]string, 0, len(s.playerNames))
	for sid := range s.playerNames {
		ids = append(ids, sid)
	}
	sort.Strings(ids)
	for _, sid := range ids {
		rank := s.playerRanks[sid]
		s.res.Players = append(s.res.Players, PlayerInfo{
			SteamID:      sid,
			Name:         s.playerNames[sid],
			Rank:         rank.rank,
			RankType:     rank.rankType,
			PreviousRank: rank.previousRank,
			WinCount:     rank.winCount,
		})
	}
}

// captureMatchMeta records game-rule signals (overtime, max rounds, server)
// for match-type classification and logs the per-player rank each demo carries.
func (s *state) captureMatchMeta() {
	cv := s.parser.GameState().Rules().ConVars()
	if v, ok := cv["mp_maxrounds"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			s.res.MaxRounds = n
		}
	}
	if v, ok := cv["mp_overtime_enable"]; ok {
		s.res.OvertimeEnabled = v == "1" || v == "true"
	}
	if v, ok := cv["hostname"]; ok {
		s.res.ServerName = v
	}
	if v, ok := cv["game_type"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			s.res.GameType = n
		}
	}
	if v, ok := cv["game_mode"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			s.res.GameMode = n
		}
	}
	s.res.PlayerCount = len(s.playerNames)

	fmt.Fprintf(
		os.Stderr,
		"[match-meta] map=%s maxRounds=%d overtime=%t players=%d gameType=%d gameMode=%d server=%q\n",
		s.res.MapName, s.res.MaxRounds, s.res.OvertimeEnabled,
		s.res.PlayerCount, s.res.GameType, s.res.GameMode, s.res.ServerName,
	)
	for sid, r := range s.playerRanks {
		fmt.Fprintf(
			os.Stderr,
			"[player-rank] steam_id=%s rank=%d rank_type=%d\n",
			sid, r.rank, r.rankType,
		)
	}
}

func (s *state) captureMaxTick() {
	t := s.parser.GameState().IngameTick()
	if t > s.maxTick {
		s.maxTick = t
	}
}
