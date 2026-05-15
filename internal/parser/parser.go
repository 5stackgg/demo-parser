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
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"

	"github.com/golang/geo/r3"
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

type EventShotFired struct {
	Tick            int    `json:"tick"`
	Round           int    `json:"round,omitempty"`
	AttackerSteamID string `json:"attacker,omitempty"`
	AttackerTeam    string `json:"attacker_team,omitempty"`
	Weapon          string `json:"weapon,omitempty"`
	// Pointers so we can distinguish "no frame snapshot" from "zero".
	Speed             *float32 `json:"speed,omitempty"`
	CounterStrafed    *bool    `json:"counter_strafed,omitempty"`
	CrosshairAngleDeg *float32 `json:"crosshair_angle_deg,omitempty"`
}

type EventDamage struct {
	Tick            int     `json:"tick"`
	Round           int     `json:"round,omitempty"`
	AttackerSteamID string  `json:"attacker,omitempty"`
	VictimSteamID   string  `json:"victim,omitempty"`
	AttackerTeam    string  `json:"attacker_team,omitempty"`
	VictimTeam      string  `json:"victim_team,omitempty"`
	Weapon          string  `json:"weapon,omitempty"`
	Damage          int     `json:"damage"`
	DamageArmor     int     `json:"damage_armor,omitempty"`
	Hitgroup        int     `json:"hitgroup,omitempty"`
	Health          int     `json:"health,omitempty"`
	SinceRoundStart float64 `json:"since_round_start,omitempty"`
}

type EventSpotted struct {
	Tick           int    `json:"tick"`
	Round          int    `json:"round,omitempty"`
	SpotterSteamID string `json:"spotter,omitempty"`
	SpottedSteamID string `json:"spotted,omitempty"`
	SpotterTeam    string `json:"spotter_team,omitempty"`
}

type EventGrenadeThrow struct {
	Tick           int     `json:"tick"`
	Round          int     `json:"round,omitempty"`
	ThrowerSteamID string  `json:"thrower,omitempty"`
	ThrowerTeam    string  `json:"thrower_team,omitempty"`
	Type           string  `json:"type"` // "Flash" | "HE" | "Smoke" | "Molotov" | "Decoy"
	OriginX        float32 `json:"ox,omitempty"`
	OriginY        float32 `json:"oy,omitempty"`
	OriginZ        float32 `json:"oz,omitempty"`
}

type EventGrenadeDetonate struct {
	Tick           int     `json:"tick"`
	Round          int     `json:"round,omitempty"`
	ThrowerSteamID string  `json:"thrower,omitempty"`
	Type           string  `json:"type"`
	X              float32 `json:"x,omitempty"`
	Y              float32 `json:"y,omitempty"`
	Z              float32 `json:"z,omitempty"`
}

// PlayerInfo — steam_id → in-game name observed in the demo. Populated
// from kill/death events (every match touches every player). The api
// reads this at clip-render enqueue time so the auto-generated titles
// in the render queue UI ("CabessaaR — Best Round (4K)" instead of
// "Player 6843 — Best Round (4K)") have real names BEFORE cs2 spins
// up — the existing GSI-based name patch only fires once a render is
// already in progress, which left the queued rows reading "Player NNNN".
type PlayerInfo struct {
	SteamID string `json:"steam_id"`
	Name    string `json:"name"`
}

type Result struct {
	TotalTicks int         `json:"total_ticks"`
	TickRate   float64     `json:"tick_rate"`
	MapName    string      `json:"map_name"`
	// Set when MapName is a workshop map (`workshop/<id>/<name>`).
	// Empty for stock maps. The streamer pod uses this to pre-download
	// the .vpk via steamcmd before launching CS2.
	WorkshopID string       `json:"workshop_id,omitempty"`
	RoundTicks []RoundTick  `json:"round_ticks"`
	Kills      []EventKill  `json:"kills"`
	Bombs      []EventBomb  `json:"bombs"`
	Players    []PlayerInfo `json:"players,omitempty"`

	ShotsFired         []EventShotFired       `json:"shots_fired,omitempty"`
	Damages            []EventDamage          `json:"damages,omitempty"`
	Spotted            []EventSpotted         `json:"spotted,omitempty"`
	GrenadeThrows      []EventGrenadeThrow    `json:"grenade_throws,omitempty"`
	GrenadeDetonations []EventGrenadeDetonate `json:"grenade_detonations,omitempty"`
}

type playerFrame struct {
	pos   r3.Vector
	speed float32
	team  common.Team
	alive bool
}

// Source 2 pawn velocity. PropertyValue.R3Vec() panics on unexpected
// shapes; defer/recover keeps a malformed entity from killing the parse.
func pawnVelocity(p *common.Player) (r3.Vector, bool) {
	if p == nil {
		return r3.Vector{}, false
	}
	pawn := p.PlayerPawnEntity()
	if pawn == nil {
		return r3.Vector{}, false
	}
	pv, ok := pawn.PropertyValue("m_vecVelocity")
	if !ok {
		return r3.Vector{}, false
	}
	defer func() { _ = recover() }()
	return pv.R3Vec(), true
}

func angleBetweenDeg(a, b r3.Vector) float32 {
	la := math.Sqrt(a.X*a.X + a.Y*a.Y + a.Z*a.Z)
	lb := math.Sqrt(b.X*b.X + b.Y*b.Y + b.Z*b.Z)
	if la == 0 || lb == 0 {
		return 180
	}
	cos := (a.X*b.X + a.Y*b.Y + a.Z*b.Z) / (la * lb)
	if cos > 1 {
		cos = 1
	} else if cos < -1 {
		cos = -1
	}
	return float32(math.Acos(cos) * 180 / math.Pi)
}

// CS eye angles → unit vector. Z-up; pitch>0 looks down.
func viewVector(yawDeg, pitchDeg float32) r3.Vector {
	yaw := float64(yawDeg) * math.Pi / 180
	pitch := float64(pitchDeg) * math.Pi / 180
	cp := math.Cos(pitch)
	return r3.Vector{
		X: cp * math.Cos(yaw),
		Y: cp * math.Sin(yaw),
		Z: -math.Sin(pitch),
	}
}

// grenadeTypeCode maps an EquipmentType to the wire string used in
// EventGrenadeThrow / EventGrenadeDetonate. Returns "" for non-grenades.
func grenadeTypeCode(t common.EquipmentType) string {
	switch t {
	case common.EqFlash:
		return "Flash"
	case common.EqHE:
		return "HE"
	case common.EqSmoke:
		return "Smoke"
	case common.EqMolotov, common.EqIncendiary:
		return "Molotov"
	case common.EqDecoy:
		return "Decoy"
	default:
		return ""
	}
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
	// Tick the most recent RoundStart fired at — used to compute
	// since_round_start for damage events (and would be reused by any
	// future round-relative metric). Captured via closure by all
	// handlers below.
	currentRoundStartTick := 0
	// Per-player set of "who currently has me spotted", keyed by
	// spotted player's steam_id then by spotter steam_id. We only emit
	// an EventSpotted on the rising edge (new spotter appears) — losing
	// sight is implicit and would just spam the timeline.
	seenSpotters := map[string]map[string]struct{}{}

	frames := map[string]playerFrame{}

	// Accumulate player names as we observe them via kill events. Map
	// here for de-dup; we flatten to the slice form on the Result at
	// the very end. Skipping bots (no real steam_id) and the empty
	// string (steamIDStr returns "" for nil players).
	playerNames := map[string]string{}
	recordPlayerName := func(p *common.Player) {
		if p == nil || p.IsBot {
			return
		}
		sid := steamIDStr(p)
		if sid == "" {
			return
		}
		name := p.Name
		// "unknown" is the placeholder demoinfocs assigns to GOTV
		// players whose raw player-info row is missing — never a real
		// player name. Skip so a transient lookup failure doesn't
		// permanently shadow a later valid name from the userinfo
		// string-table.
		if name == "" || name == "unknown" {
			return
		}
		// Last write wins — players can rename mid-match. The most
		// recent is what the streamer pod's GSI would also report.
		playerNames[sid] = name
	}

	// The demo's userinfo string-table is the AUTHORITATIVE source for
	// (steam_id, name) pairs — it's the same data CS2 itself ships to
	// connected clients (and what GSI reports on the streamer pod).
	// Every player who ever connected gets a row, even if they
	// disconnected before any kill event or their player-controller
	// entity got recycled. We hook the events.PlayerInfo dispatch
	// directly so disconnected players still land in res.Players —
	// without this, demos where a coach swapped seats mid-match (or
	// where userinfo arrived ahead of the player-controller binding)
	// produced "Player NNNN" titles on the queue panel until the
	// streamer pod's GSI patch fired.
	recordPlayerInfo := func(info common.PlayerInfo) {
		if info.IsFakePlayer || info.GUID == "BOT" {
			return
		}
		if info.XUID == 0 || info.Name == "" {
			return
		}
		playerNames[strconv.FormatUint(info.XUID, 10)] = info.Name
	}
	parser.RegisterEventHandler(func(e events.PlayerInfo) {
		recordPlayerInfo(e.Info)
	})
	parser.RegisterEventHandler(func(e events.PlayerConnect) {
		recordPlayerName(e.Player)
	})
	parser.RegisterEventHandler(func(e events.PlayerNameChange) {
		recordPlayerName(e.Player)
	})

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
		// Sweep all currently-connected participants on every round
		// start so we capture player names from demos where m_szPlayerName
		// hasn't synced by the first time a player shows up in a Kill
		// event. Without this, a player who got their kills early in
		// the demo (when their name net-field was still empty) ends up
		// missing from res.Players, and the api ends up titling clips
		// "Player NNNN" until the streamer pod can patch it via GSI.
		for _, p := range parser.GameState().Participants().All() {
			recordPlayerName(p)
		}
		if !matchStarted {
			return
		}
		currentRound++
		currentRoundStartTick = parser.GameState().IngameTick()
		// Reset the spotter cache each round — the engine reuses
		// player entities across rounds but visibility resets at the
		// freeze break, so the first PlayerSpottersChanged of the new
		// round should always emit (it's a brand-new sighting).
		seenSpotters = map[string]map[string]struct{}{}
		res.RoundTicks = append(res.RoundTicks, RoundTick{
			Round:     currentRound,
			StartTick: currentRoundStartTick,
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
		recordPlayerName(e.Killer)
		recordPlayerName(e.Victim)
		recordPlayerName(e.Assister)
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

	parser.RegisterEventHandler(func(_ events.FrameDone) {
		if !matchStarted {
			return
		}
		clear(frames)
		for _, p := range parser.GameState().Participants().Playing() {
			if p == nil || !p.IsAlive() {
				continue
			}
			sid := steamIDStr(p)
			if sid == "" {
				continue
			}
			vel, ok := pawnVelocity(p)
			speed := float32(0)
			if ok {
				speed = float32(math.Sqrt(vel.X*vel.X + vel.Y*vel.Y))
			}
			frames[sid] = playerFrame{
				pos:   p.Position(),
				speed: speed,
				team:  p.Team,
				alive: true,
			}
		}
	})

	// WeaponFire — one row per shot. Filter to firearm classes only:
	// knives and grenade "fires" would balloon the array and don't
	// participate in accuracy metrics. demoinfocs's EquipmentClass
	// already buckets exactly the way we want (Pistols/SMG/Heavy/Rifle).
	parser.RegisterEventHandler(func(e events.WeaponFire) {
		if !matchStarted || e.Shooter == nil || e.Weapon == nil {
			return
		}
		switch e.Weapon.Class() {
		case common.EqClassPistols, common.EqClassSMG,
			common.EqClassHeavy, common.EqClassRifle:
			// firearm — keep
		default:
			return
		}
		ev := EventShotFired{
			Tick:            parser.GameState().IngameTick(),
			Round:           currentRound,
			AttackerSteamID: steamIDStr(e.Shooter),
			AttackerTeam:    teamCode(e.Shooter.Team),
			Weapon:          e.Weapon.String(),
		}

		if sf, ok := frames[ev.AttackerSteamID]; ok && sf.alive {
			speed := sf.speed
			// CS2 movement-accuracy floor.
			counter := speed < 5
			ev.Speed = &speed
			ev.CounterStrafed = &counter

			var (
				bestDist  = math.Inf(1)
				bestEnemy r3.Vector
				haveEnemy bool
			)
			for sid, ef := range frames {
				if sid == ev.AttackerSteamID || !ef.alive {
					continue
				}
				if ef.team == sf.team || ef.team == common.TeamUnassigned || ef.team == common.TeamSpectators {
					continue
				}
				d := ef.pos.Sub(sf.pos)
				dist := d.X*d.X + d.Y*d.Y + d.Z*d.Z
				if dist < bestDist {
					bestDist = dist
					bestEnemy = ef.pos
					haveEnemy = true
				}
			}
			if haveEnemy {
				eyes, eok := e.Shooter.PositionEyes()
				if !eok {
					eyes = sf.pos
				}
				view := viewVector(e.Shooter.ViewDirectionX(), e.Shooter.ViewDirectionY())
				toEnemy := bestEnemy.Sub(eyes)
				angle := angleBetweenDeg(view, toEnemy)
				ev.CrosshairAngleDeg = &angle
			}
		}

		res.ShotsFired = append(res.ShotsFired, ev)
	})

	// PlayerHurt — one row per damage instance. Skip self-damage
	// (HE/molly on yourself) and null attackers (world / falling), both
	// of which would skew aim/damage stats on the ingestion side.
	parser.RegisterEventHandler(func(e events.PlayerHurt) {
		if !matchStarted || e.Attacker == nil || e.Player == nil {
			return
		}
		if e.Attacker == e.Player {
			return
		}
		tick := parser.GameState().IngameTick()
		sinceRound := 0.0
		if rate := parser.TickRate(); rate > 0 {
			sinceRound = float64(tick-currentRoundStartTick) / rate
		}
		d := EventDamage{
			Tick:            tick,
			Round:           currentRound,
			AttackerSteamID: steamIDStr(e.Attacker),
			VictimSteamID:   steamIDStr(e.Player),
			AttackerTeam:    teamCode(e.Attacker.Team),
			VictimTeam:      teamCode(e.Player.Team),
			Damage:          e.HealthDamage,
			DamageArmor:     e.ArmorDamage,
			Hitgroup:        int(e.HitGroup),
			Health:          e.Health,
			SinceRoundStart: sinceRound,
		}
		if e.Weapon != nil {
			d.Weapon = e.Weapon.String()
		}
		res.Damages = append(res.Damages, d)
	})

	// PlayerSpottersChanged — v5 fires this whenever the set of
	// players that can see e.Spotted changes. The event doesn't tell
	// us *who* changed, so we diff against a cached set and emit one
	// EventSpotted per newly-appearing spotter. Losses-of-sight are
	// ignored (an EventUnspotted would just double the wire size for
	// no analytical value on the leetify-parity dashboards).
	parser.RegisterEventHandler(func(e events.PlayerSpottersChanged) {
		if !matchStarted || e.Spotted == nil {
			return
		}
		spottedID := steamIDStr(e.Spotted)
		if spottedID == "" {
			return
		}
		prev := seenSpotters[spottedID]
		next := map[string]struct{}{}
		tick := parser.GameState().IngameTick()
		for _, p := range parser.GameState().Participants().All() {
			if p == nil || p == e.Spotted {
				continue
			}
			if !p.HasSpotted(e.Spotted) {
				continue
			}
			pid := steamIDStr(p)
			if pid == "" {
				continue
			}
			next[pid] = struct{}{}
			if _, had := prev[pid]; had {
				continue
			}
			res.Spotted = append(res.Spotted, EventSpotted{
				Tick:           tick,
				Round:          currentRound,
				SpotterSteamID: pid,
				SpottedSteamID: spottedID,
				SpotterTeam:    teamCode(p.Team),
			})
		}
		seenSpotters[spottedID] = next
	})

	// GrenadeProjectileThrow — fires when the projectile entity is
	// created (i.e. the moment the grenade leaves the player's hand).
	// Note FireGrenadeStart has a nil Thrower in Source 2 demos, so the
	// throw-side data must come from this event.
	parser.RegisterEventHandler(func(e events.GrenadeProjectileThrow) {
		if !matchStarted || e.Projectile == nil {
			return
		}
		thrower := e.Projectile.Thrower
		if thrower == nil {
			thrower = e.Projectile.Owner
		}
		var gtype string
		if e.Projectile.WeaponInstance != nil {
			gtype = grenadeTypeCode(e.Projectile.WeaponInstance.Type)
		}
		if gtype == "" {
			return
		}
		pos := e.Projectile.Position()
		ev := EventGrenadeThrow{
			Tick:    parser.GameState().IngameTick(),
			Round:   currentRound,
			Type:    gtype,
			OriginX: float32(pos.X),
			OriginY: float32(pos.Y),
			OriginZ: float32(pos.Z),
		}
		if thrower != nil {
			ev.ThrowerSteamID = steamIDStr(thrower)
			ev.ThrowerTeam = teamCode(thrower.Team)
		}
		res.GrenadeThrows = append(res.GrenadeThrows, ev)
	})

	// Detonation handlers — share the same wire shape and pull from
	// the embedded GrenadeEvent. FireGrenadeStart's Thrower is always
	// nil in Source 2; the ingestion side can join back to the most
	// recent matching throw if it needs to attribute mollies.
	emitDetonate := func(base events.GrenadeEvent, typeOverride string) {
		gtype := typeOverride
		if gtype == "" {
			gtype = grenadeTypeCode(base.GrenadeType)
		}
		if gtype == "" {
			return
		}
		ev := EventGrenadeDetonate{
			Tick:  parser.GameState().IngameTick(),
			Round: currentRound,
			Type:  gtype,
			X:     float32(base.Position.X),
			Y:     float32(base.Position.Y),
			Z:     float32(base.Position.Z),
		}
		if base.Thrower != nil {
			ev.ThrowerSteamID = steamIDStr(base.Thrower)
		}
		res.GrenadeDetonations = append(res.GrenadeDetonations, ev)
	}
	parser.RegisterEventHandler(func(e events.HeExplode) {
		if !matchStarted {
			return
		}
		emitDetonate(e.GrenadeEvent, "HE")
	})
	parser.RegisterEventHandler(func(e events.FlashExplode) {
		if !matchStarted {
			return
		}
		emitDetonate(e.GrenadeEvent, "Flash")
	})
	parser.RegisterEventHandler(func(e events.SmokeStart) {
		if !matchStarted {
			return
		}
		emitDetonate(e.GrenadeEvent, "Smoke")
	})
	parser.RegisterEventHandler(func(e events.FireGrenadeStart) {
		if !matchStarted {
			return
		}
		emitDetonate(e.GrenadeEvent, "Molotov")
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

	// Final participant sweep — picks up any player whose name net-field
	// settled after their last kill-event sample (eg. late reconnect
	// at end-of-match scoreboard).
	for _, p := range parser.GameState().Participants().All() {
		recordPlayerName(p)
	}

	// Flatten the accumulated player-name map onto the result. Sort
	// by steam_id for stable JSON output (handy when diffing parser
	// runs across changes).
	if len(playerNames) > 0 {
		res.Players = make([]PlayerInfo, 0, len(playerNames))
		ids := make([]string, 0, len(playerNames))
		for sid := range playerNames {
			ids = append(ids, sid)
		}
		sort.Strings(ids)
		for _, sid := range ids {
			res.Players = append(res.Players, PlayerInfo{
				SteamID: sid,
				Name:    playerNames[sid],
			})
		}
	}

	return res, nil
}
