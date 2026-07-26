// Package parser wraps markus-wa/demoinfocs-golang to extract playback
// metadata, events, and player stats from CS2 demo files.
package parser

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"

	"github.com/5stackgg/demo-parser/internal/geometry"
	"github.com/golang/geo/r3"
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
	// Indices into res.ShotsFired that have not yet been matched to a damage
	// event, oldest first, per attacker. A burst lands several rounds inside one
	// spray window, so pairing damage against only the most recent shot leaves
	// the rest of the burst looking like it missed.
	pendingShots map[string][]int

	victimHealth map[string]int

	lastMoveTick map[string]int

	// (spotted, spotter) → the tick that pair first became visible on the
	// spotter's screen. One anchor serves both reaction and crosshair
	// placement; see trackFOV.
	fovEntry map[string]map[string]visEntry

	// (spotted, spotter) → last tick trackFOV cast a sightline ray for a pair
	// that is on screen but not yet confirmed visible. Throttles the
	// per-frame raycasting to fovLosEveryTicks.
	fovLosProbe map[[2]string]int

	// Deployed smoke clouds, and entity-id → index for the ones still active.
	// Consulted by losAt so every sightline-gated stat accounts for smoke.
	smokes     []smokeCloud
	smokeByEnt map[int]int
	// Explosions currently holding a hole open in the smoke.
	blasts []smokeBlast

	// Live infernos, keyed by the library's unique id (entity ids get reused).
	infernos          map[int]*infernoTrack
	lastInfernoSample int
	infernoFires      int
	// Demo tick rate, mirrored off the parser each frame so the smoke bloom
	// ramp can be evaluated from state alone.
	tickRate float64
	// Diagnostics for the [smoke] log line. smokeBlocks counts raycasts, not
	// sightlines: visibleAt escalates a blocked eye ray into the body samples,
	// so one obscured player contributes several. It is a "this is firing"
	// signal — the effect on stats is the spotted/engagement counts.
	smokeOpened int
	smokeBlocks int
	smokeVoxels int
	// Clouds whose flood found almost nothing, so they fall back to a sphere.
	// A non-zero count means the mesh and the demo disagree about where the
	// world is — most likely a surface the mesh pipeline dropped.
	smokeSealed int
	// Blasts recorded, and sightlines that survived because one had cleared the
	// smoke in the way.
	blastCount   int
	blastLetTh   int
	blastQueries int

	// Door leaves reconstructed from entity state, re-scanned each round.
	// Consulted by losAt because the collision mesh contains no doors.
	doors           []*doorLeaf
	doorSeen        map[int]bool
	doorLastScan    int
	doorsTracked    int
	doorsMoved      int
	doorBlocks      int
	doorsNoBounds   int
	doorsDegenerate int
	doorsNoOrigin   int

	// Recent eye positions per player, oldest first. Lets aim angles be
	// measured against the target as the shooter's client rendered it
	// (interpolated ~2 ticks behind the server) rather than where the server
	// had already moved it.
	eyeHistory map[string][]eyeSample

	// [attacker][victim] → in-flight engagement. Opened on first sight,
	// flushed to res.AimEngagements on the victim's death, timeout, or
	// round end.
	engagements map[string]map[string]*engagement

	// Map collision mesh for line-of-sight validation, lazy-loaded once the
	// map name is known. meshTried guards against re-loading; a nil mesh
	// (unknown map / no .tri / disabled) means los() never filters.
	mesh      *geometry.Mesh
	meshTried bool

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
		parser:       dem.NewParser(r),
		res:          &Result{},
		visStart:     map[string]map[string]visEntry{},
		frames:       map[string]playerFrame{},
		lastShot:     map[string]shotMark{},
		pendingShots: map[string][]int{},
		victimHealth: map[string]int{},
		lastMoveTick: map[string]int{},
		fovEntry:     map[string]map[string]visEntry{},
		fovLosProbe:  map[[2]string]int{},
		smokeByEnt:   map[int]int{},
		infernos:     map[int]*infernoTrack{},
		doorSeen:     map[int]bool{},
		eyeHistory:   map[string][]eyeSample{},
		engagements:  map[string]map[string]*engagement{},
		playerNames:  map[string]string{},
		playerRanks:  map[string]playerRank{},
		grenadePos:   map[int]grenadeProjectile{},
		grenadePaths: map[int][]GrenadePathPt{},
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
	s.parser.RegisterEventHandler(s.onSmokeExpired)
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

	// Flush any engagement still open at the demo's end.
	s.closeAllEngagements()

	// Record whether map geometry was available, so consumers can flag the
	// LOS-gated stats as validated vs estimated. Force a load attempt if the
	// demo had no engagements to trigger lazy loading.
	s.ensureMesh()
	s.res.GeometryValidated = s.mesh != nil

	fmt.Fprintf(
		os.Stderr,
		"[smoke] clouds=%d voxels=%d rays_blocked=%d collapsed=%d "+
			"blasts=%d rays_during_blast=%d rays_saved_by_blast=%d\n",
		s.smokeOpened, s.smokeVoxels, s.smokeBlocks, s.smokeSealed,
		s.blastCount, s.blastQueries, s.blastLetTh,
	)
	fmt.Fprintf(
		os.Stderr,
		"[doors] distinct=%d max_concurrent=%d movements=%d sightlines_blocked=%d "+
			"skipped(no_bounds=%d degenerate=%d no_origin=%d)\n",
		len(s.doorSeen), s.doorsTracked, s.doorsMoved, s.doorBlocks,
		s.doorsNoBounds, s.doorsDegenerate, s.doorsNoOrigin,
	)

	s.flushInfernos()
	fmt.Fprintf(
		os.Stderr,
		"[inferno] burns=%d flames=%d\n",
		len(s.res.Infernos), s.infernoFires,
	)

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
	if v, ok := cv["hostname"]; ok && v != "" && s.res.ServerName == "" {
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

// ensureMesh lazy-loads the map collision mesh once the map name is known,
// memoizing the attempt (including a nil result). It's a no-op until MapName
// is set, so early callers retry on the next call.
func (s *state) ensureMesh() {
	if s.meshTried || s.res.MapName == "" {
		return
	}
	s.meshTried = true
	mesh, err := geometry.Load(s.res.MapName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[geometry] mesh load failed for %s: %v\n", s.res.MapName, err)
	}
	s.mesh = mesh
	if s.mesh != nil {
		fmt.Fprintf(os.Stderr, "[geometry] loaded mesh for %s (%d triangles)\n", s.res.MapName, s.mesh.Triangles())
	} else {
		fmt.Fprintf(os.Stderr, "[geometry] no mesh for %s — sightline validation disabled\n", s.res.MapName)
	}
}

// losAt reports whether two points had a clear line of sight at a given tick —
// through the map geometry and through any smoke deployed then. The mesh is
// lazy-loaded on first use (after MapName is known); when no mesh is available
// the world check passes so sightline-gated stats fall back to their
// unvalidated behaviour rather than dropping to zero. Smoke is still applied,
// since it needs no mesh.
//
// The tick matters: some callers re-validate a spot buffered up to
// its lookback window ago, and the smoke covering that sightline may not have
// existed yet.
func (s *state) losAt(tick int, a, b r3.Vector) bool {
	s.ensureMesh()
	if s.mesh != nil {
		if s.mesh.Occluded(a, b) {
			return false
		}
		// Doors are gated on having a mesh: without the surrounding walls they
		// would be the only occluder on the map, which is worse than the
		// existing "treat everything as visible" fallback.
		if s.doorOccluded(a, b) {
			return false
		}
	}
	return !s.smokeOccluded(tick, a, b)
}

func (s *state) los(a, b r3.Vector) bool {
	return s.losAt(s.parser.GameState().IngameTick(), a, b)
}

// visibleAt reports whether any part of a player (eyes at eye, standing on
// feet) was visible from an observer's eye at a tick. The eye-to-eye ray is
// tested first and short-circuits, so the common case costs exactly one ray as
// before; the body samples are only paid for when the eyes are occluded.
func (s *state) visibleAt(tick int, from, eye, feet r3.Vector) bool {
	if s.losAt(tick, from, eye) {
		return true
	}
	for _, p := range bodySamplePoints(from, eye, feet) {
		if s.losAt(tick, from, p) {
			return true
		}
	}
	return false
}

func (s *state) captureMaxTick() {
	t := s.parser.GameState().IngameTick()
	if t > s.maxTick {
		s.maxTick = t
	}
}
