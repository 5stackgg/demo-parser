// Package parser wraps markus-wa/demoinfocs-golang to extract the
// metadata the 5stack platform needs for demo playback.
//
// Today: total_ticks, tick_rate, map_name, and per-round tick boundaries.
//
// Tomorrow (the 2D-playback follow-up planned in
// /Users/luke/.claude/plans/now-that-we-have-foamy-newt.md): per-tick
// player positions and grenade trajectories. Keeping the parser as
// an importable package — separate from cmd/server — means the same
// binary can grow a /frames endpoint without rewriting the demo-fetch
// or s3-write paths.
package parser

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"

	dem "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/msg"
)

func teamCode(t common.Team) string {
	switch t {
	case common.TeamCounterTerrorists:
		return "ct"
	case common.TeamTerrorists:
		return "t"
	default:
		return ""
	}
}

func steamIDStr(p *common.Player) string {
	if p == nil || p.SteamID64 == 0 {
		return ""
	}
	return strconv.FormatUint(p.SteamID64, 10)
}

func bombSiteCode(s events.Bombsite) string {
	switch s {
	case events.BombsiteA:
		return "A"
	case events.BombsiteB:
		return "B"
	default:
		return ""
	}
}

// CS2 demos record workshop maps as `workshop/<numeric-id>/<map-name>`
// (e.g. `workshop/3070821578/de_torn`). Stock maps record as plain
// names (`de_dust2`). Capturing the id lets the streamer pre-download
// the .vpk via steamcmd before launching CS2 — without it, CS2 stalls
// on a Subscribe? dialog and the demo never starts.
var workshopMapRe = regexp.MustCompile(`^workshop/(\d+)/`)

type RoundTick struct {
	Round     int `json:"round"`
	StartTick int `json:"start_tick"`
	EndTick   int `json:"end_tick"`
	// Populated by RoundEnd. "ct" / "t" / "" (draw).
	Winner string `json:"winner,omitempty"`
	// Numeric reason from demoinfocs (T_Win, CT_Win, BombDefused,
	// TargetBombed, etc.). Web maps to a label.
	Reason int `json:"reason,omitempty"`
}

// Compact event records — small, tick-anchored, easy to render as
// markers on the seek bar. We deliberately keep payloads tiny: web
// fetches the whole list once per session, larger structures slow
// the GraphQL subscription tick. Steam IDs as strings (bigint
// overflow safety in JS).
type EventKill struct {
	Tick          int    `json:"tick"`
	KillerSteamID string `json:"killer,omitempty"`
	VictimSteamID string `json:"victim,omitempty"`
	AssistSteamID string `json:"assist,omitempty"`
	// "ct" / "t" / "" — team membership at the moment of the kill.
	// CS2 demos swap teams at halftime, so capturing this per-kill is
	// the only way the web side can color-code markers without
	// replaying side-swap math.
	KillerTeam   string `json:"killer_team,omitempty"`
	VictimTeam   string `json:"victim_team,omitempty"`
	Weapon       string `json:"weapon,omitempty"`
	Headshot     bool   `json:"headshot,omitempty"`
	WallBang     bool   `json:"wallbang,omitempty"`
	NoScope      bool   `json:"noscope,omitempty"`
	ThroughSmoke bool   `json:"smoke,omitempty"`
}

type EventBomb struct {
	Tick   int    `json:"tick"`
	Type   string `json:"type"` // "planted" | "defused" | "exploded"
	Player string `json:"player,omitempty"`
	Site   string `json:"site,omitempty"` // "A" | "B"
}

type Result struct {
	TotalTicks int         `json:"total_ticks"`
	TickRate   float64     `json:"tick_rate"`
	MapName    string      `json:"map_name"`
	// Set when MapName is a workshop map (`workshop/<id>/<name>`).
	// Empty for stock maps. The streamer pod uses this to pre-download
	// the .vpk via steamcmd before launching CS2.
	WorkshopID string      `json:"workshop_id,omitempty"`
	RoundTicks []RoundTick `json:"round_ticks"`
	Kills      []EventKill `json:"kills"`
	Bombs      []EventBomb `json:"bombs"`
}

// Parse reads a CS2/CSGO demo from r and returns the playback metadata.
// The reader must be the entire .dem byte stream — demoinfocs needs the
// header AND the event packets, so streaming with Content-Length and
// piping through a buffered reader is fine; chunked partial-reads are not.
func Parse(r io.Reader) (*Result, error) {
	parser := dem.NewParser(r)
	defer parser.Close()

	// v5 dropped Parser.ParseHeader / common.DemoHeader and only supports
	// CS2 (Source 2) demos. Map name now comes from the CSVCMsg_ServerInfo
	// net message that fires once near the start of every demo; we capture
	// it live instead of reading a header struct after ParseToEnd.

	res := &Result{}
	parser.RegisterNetMessageHandler(func(m *msg.CSVCMsg_ServerInfo) {
		if name := m.GetMapName(); name != "" {
			res.MapName = name
			if mm := workshopMapRe.FindStringSubmatch(name); len(mm) == 2 {
				res.WorkshopID = mm[1]
			}
		}
	})
	// Track the highest in-game tick we observe. ServerInfo /
	// MatchStart fire early; round_starts and frame ticks fire
	// throughout. The max is the de-facto demo length when the
	// header doesn't carry it.
	maxTick := 0
	captureMaxTick := func() {
		t := parser.GameState().IngameTick()
		if t > maxTick {
			maxTick = t
		}
	}

	// matchStarted gates the round event collection: demos always include
	// warmup + (optional) knife rounds before the actual match. Counting
	// those as "rounds 1..N" would mismatch what the user sees in the
	// scoreboard — they want to scrub to scoreboard round 1, not "warmup".
	matchStarted := false
	currentRound := 0

	parser.RegisterEventHandler(func(e events.MatchStart) {
		matchStarted = true
		// In CSGO demos MatchStart fires once at the start of the
		// real match. CS2 demos sometimes fire it again post-knife;
		// resetting currentRound here ensures the post-knife round
		// 1 lines up with scoreboard round 1.
		currentRound = 0
		res.RoundTicks = res.RoundTicks[:0]
	})

	parser.RegisterEventHandler(func(e events.RoundStart) {
		captureMaxTick()
		if !matchStarted {
			return
		}
		currentRound++
		res.RoundTicks = append(res.RoundTicks, RoundTick{
			Round:     currentRound,
			StartTick: parser.GameState().IngameTick(),
		})
	})

	// RoundEnd carries the winner + reason. Fires before
	// RoundEndOfficial. Cache on the most recent round entry so the
	// frontend can render scoreboard-style markers.
	parser.RegisterEventHandler(func(e events.RoundEnd) {
		if !matchStarted || len(res.RoundTicks) == 0 {
			return
		}
		last := &res.RoundTicks[len(res.RoundTicks)-1]
		last.Winner = teamCode(e.Winner)
		last.Reason = int(e.Reason)
	})

	parser.RegisterEventHandler(func(e events.RoundEndOfficial) {
		captureMaxTick()
		if !matchStarted || len(res.RoundTicks) == 0 {
			return
		}
		// Cap the most recent round's end_tick. RoundEndOfficial fires
		// after the freeze-time delay following RoundEnd, so this is
		// the tick the demo timeline considers the round closed —
		// the right place to cut a "round 5" scrub target if the user
		// wants the very end of the round (currently we only seek to
		// start_tick, but having end_tick lets us add round-end seeks
		// without re-parsing).
		res.RoundTicks[len(res.RoundTicks)-1].EndTick = parser.GameState().IngameTick()
	})

	// Kills — the densest signal for "interesting moments". Skip
	// warmup/knife so the seek bar doesn't get cluttered by pre-match
	// noise. Killer/victim may be nil on world damage or partially
	// corrupt demos; record what we can either way.
	parser.RegisterEventHandler(func(e events.Kill) {
		if !matchStarted {
			return
		}
		k := EventKill{
			Tick:          parser.GameState().IngameTick(),
			KillerSteamID: steamIDStr(e.Killer),
			VictimSteamID: steamIDStr(e.Victim),
			AssistSteamID: steamIDStr(e.Assister),
			Headshot:      e.IsHeadshot,
			WallBang:      e.IsWallBang(),
			NoScope:       e.NoScope,
			ThroughSmoke:  e.ThroughSmoke,
		}
		if e.Killer != nil {
			k.KillerTeam = teamCode(e.Killer.Team)
		}
		if e.Victim != nil {
			k.VictimTeam = teamCode(e.Victim.Team)
		}
		if e.Weapon != nil {
			k.Weapon = e.Weapon.String()
		}
		res.Kills = append(res.Kills, k)
	})

	// Bomb events — three distinct types collapsed into a single
	// list with a `type` discriminator. Saves a few JSON keys vs
	// separate slices and reads naturally on the web side as a
	// single timeline.
	parser.RegisterEventHandler(func(e events.BombPlanted) {
		if !matchStarted {
			return
		}
		res.Bombs = append(res.Bombs, EventBomb{
			Tick:   parser.GameState().IngameTick(),
			Type:   "planted",
			Player: steamIDStr(e.Player),
			Site:   bombSiteCode(e.Site),
		})
	})
	parser.RegisterEventHandler(func(e events.BombDefused) {
		if !matchStarted {
			return
		}
		res.Bombs = append(res.Bombs, EventBomb{
			Tick:   parser.GameState().IngameTick(),
			Type:   "defused",
			Player: steamIDStr(e.Player),
			Site:   bombSiteCode(e.Site),
		})
	})
	parser.RegisterEventHandler(func(e events.BombExplode) {
		if !matchStarted {
			return
		}
		res.Bombs = append(res.Bombs, EventBomb{
			Tick: parser.GameState().IngameTick(),
			Type: "exploded",
			Site: bombSiteCode(e.Site),
		})
	})

	// CS2 demos occasionally hit entity-resolution errors mid-stream
	// inside demoinfocs ("unable to find existing entity NNN" or
	// similar). The parser bails at that tick, but we've already
	// collected everything up to that point — round_ticks, kills,
	// bombs all fire from event handlers so the slices are valid as
	// of the last successful tick.
	//
	// Treat ParseToEnd errors as soft: log + return what we have.
	// The api caller persists partial results; the popup gets a
	// shorter timeline than the full match but still has nav.
	if err := parser.ParseToEnd(); err != nil {
		fmt.Fprintf(os.Stderr,
			"parse-to-end error (returning partial result): %v\n", err)
	}

	// Resolve header-equivalent fields from the live parser state.
	// CS2 demos don't carry these on the file header (see ParseHeader
	// note above) — they're inferred from packets observed during
	// ParseToEnd. This block runs even on partial parses so the
	// scrubber gets *some* total/rate.
	if rate := parser.TickRate(); rate > 0 {
		res.TickRate = rate
	}
	// Last observed in-game tick: best signal for "demo length" when
	// the file header is empty. For partial parses this is the
	// abort tick — still better than zero for the UI.
	if t := parser.GameState().IngameTick(); t > maxTick {
		maxTick = t
	}
	res.TotalTicks = maxTick
	// MapName / WorkshopID are set by the CSVCMsg_ServerInfo handler
	// above. If the handler never fired (very early parse abort) the
	// fields stay empty — the popup falls back to "<unknown>" and
	// workshop-map detection no-ops (stock-map demos work fine).

	return res, nil
}
