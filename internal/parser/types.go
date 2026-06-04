package parser

import (
	"github.com/golang/geo/r3"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
)

type RoundTick struct {
	Round         int    `json:"round"`
	StartTick     int    `json:"start_tick"`
	FreezeEndTick int    `json:"freeze_end_tick,omitempty"`
	EndTick       int    `json:"end_tick"`
	Winner        string `json:"winner,omitempty"`
	Reason        int    `json:"reason,omitempty"`
	// Team money summed at round end (per side), mirroring the live
	// game-server's GetTeamMoney capture. The importer maps these to
	// lineup_1/lineup_2 by the side each lineup held that round.
	CtMoney *int `json:"ct_money,omitempty"`
	TMoney  *int `json:"t_money,omitempty"`
}

type EventKill struct {
	Tick          int    `json:"tick"`
	KillerSteamID string `json:"killer,omitempty"`
	VictimSteamID string `json:"victim,omitempty"`
	AssistSteamID string `json:"assist,omitempty"`
	AssistFlash   bool   `json:"assist_flash,omitempty"`
	KillerTeam    string `json:"killer_team,omitempty"`
	VictimTeam    string `json:"victim_team,omitempty"`
	Weapon        string `json:"weapon,omitempty"`
	Headshot      bool   `json:"headshot,omitempty"`
	WallBang      bool   `json:"wallbang,omitempty"`
	NoScope       bool   `json:"noscope,omitempty"`
	ThroughSmoke  bool   `json:"smoke,omitempty"`
	// World coordinates at the moment of the kill — killer's position and
	// the victim's last-alive position. Lets the importer store kill/death
	// locations without the heatmap having to download the demo blob.
	AttackerX *float32 `json:"attacker_x,omitempty"`
	AttackerY *float32 `json:"attacker_y,omitempty"`
	AttackerZ *float32 `json:"attacker_z,omitempty"`
	VictimX   *float32 `json:"victim_x,omitempty"`
	VictimY   *float32 `json:"victim_y,omitempty"`
	VictimZ   *float32 `json:"victim_z,omitempty"`
}

// EventBomb is a single timeline entry for a bomb interaction. Type
// values: "planted", "defused", "exploded" (terminal events) and
// "plant_begin", "plant_abort", "defuse_begin", "defuse_abort",
// "dropped", "pickup" (in-flight events for visualizing carrier and
// active plant/defuse states on the 2D replay).
type EventBomb struct {
	Tick   int    `json:"tick"`
	Type   string `json:"type"`
	Player string `json:"player,omitempty"`
	Site   string `json:"site,omitempty"`
	// HasKit is set on "defuse_begin" — tells the consumer whether to
	// show a 5s (kit) or 10s (no kit) defuse window on the player.
	HasKit bool `json:"has_kit,omitempty"`
	// Position of the bomb at this event. Captured on "dropped" and
	// "planted" so the 2D replay can render the bomb on the ground
	// between drop and pickup, and at the plant site after detonation.
	X float32 `json:"x,omitempty"`
	Y float32 `json:"y,omitempty"`
	Z float32 `json:"z,omitempty"`
}

// EventKitDrop marks the spot where a CT lost their defuse kit
// (currently only emitted when the kit-holder dies, since that's when
// the kit physically becomes pickable on the ground). The 2D replay
// renders a small kit icon at this location until another CT moves
// over it (the consumer doesn't currently see the pickup event — kit
// stays rendered for the rest of the round).
type EventKitDrop struct {
	Tick   int     `json:"tick"`
	Round  int     `json:"round,omitempty"`
	Player string  `json:"player,omitempty"`
	X      float32 `json:"x"`
	Y      float32 `json:"y"`
	Z      float32 `json:"z"`
}

type EventShotFired struct {
	Tick            int    `json:"tick"`
	Round           int    `json:"round,omitempty"`
	AttackerSteamID string `json:"attacker,omitempty"`
	AttackerTeam    string `json:"attacker_team,omitempty"`
	Weapon          string `json:"weapon,omitempty"`
	IsRifle         bool   `json:"is_rifle,omitempty"`
	IsCrouched      bool   `json:"is_crouched,omitempty"`
	EnemySpotted    bool   `json:"enemy_spotted,omitempty"`
	// IsSpray: this shot followed the same attacker's previous shot
	// within 250ms — i.e. trigger held. First shot of a burst is false.
	IsSpray    bool     `json:"is_spray,omitempty"`
	Speed      *float32 `json:"speed,omitempty"`
	WasStopped *bool    `json:"was_stopped,omitempty"`
	// AmmoInMagazine = rounds remaining in the magazine BEFORE this
	// shot was fired. Consumer derives "wasted magazine" by detecting
	// upward jumps between consecutive shots in the same round —
	// any leftover ammo when the count resets was wasted on a reload.
	AmmoInMagazine *int `json:"ammo_in_magazine,omitempty"`
}

// EventPosition is a low-frequency (~4Hz) sample of a single player's
// world position + view yaw. The replay viewer interpolates between
// adjacent samples to render a 2D radar timeline.
type EventPosition struct {
	Tick            int     `json:"tick"`
	Round           int     `json:"round,omitempty"`
	AttackerSteamID string  `json:"attacker,omitempty"`
	Team            string  `json:"team,omitempty"`
	Alive           bool    `json:"alive,omitempty"`
	X               float32 `json:"x"`
	Y               float32 `json:"y"`
	Z               float32 `json:"z"`
	Yaw             float32 `json:"yaw,omitempty"`
	// Current HP at sample time. Lets the replay viewer render a
	// boltobserv-style "wounded back" arc on the player dot.
	Health int `json:"health,omitempty"`
	// Current armor at sample time (0–100). Rendered behind the HP
	// bar in the replay so a coach can see who still has kevlar.
	Armor int `json:"armor,omitempty"`
	// Helmet is true when the player has a helmet on this sample.
	// The replay tints the armor bar based on this so a coach can
	// instantly tell kevlar from kevlar+helmet.
	HasHelmet bool `json:"helmet,omitempty"`
	// HasBomb is true when this player is the bomb carrier at this
	// sample tick. The 2D replay uses it to render a small bomb icon
	// on the carrier's marker between pickup and plant/drop.
	HasBomb bool `json:"has_bomb,omitempty"`
	// HasDefuser is true for CTs carrying a defuse kit. Lets the
	// replay overlay show a small kit indicator so viewers can see
	// which CT will get the 5s defuse window.
	HasDefuser bool `json:"has_defuser,omitempty"`
}

type EventFlash struct {
	Tick            int     `json:"tick"`
	Round           int     `json:"round,omitempty"`
	AttackerSteamID string  `json:"attacker,omitempty"`
	AttackerTeam    string  `json:"attacker_team,omitempty"`
	VictimSteamID   string  `json:"victim,omitempty"`
	VictimTeam      string  `json:"victim_team,omitempty"`
	Duration        float64 `json:"duration,omitempty"`
	TeamFlash       bool    `json:"team_flash,omitempty"`
}

type EventRoundInventory struct {
	Round           int    `json:"round,omitempty"`
	AttackerSteamID string `json:"attacker,omitempty"`
	Team            string `json:"team,omitempty"`
	Flash           int    `json:"flash,omitempty"`
	Smoke           int    `json:"smoke,omitempty"`
	HE              int    `json:"he,omitempty"`
	Molotov         int    `json:"molotov,omitempty"`
	Decoy           int    `json:"decoy,omitempty"`
	Primary         string `json:"primary,omitempty"`
	Secondary       string `json:"secondary,omitempty"`
	Armor           int    `json:"armor,omitempty"`
	Helmet          bool   `json:"helmet,omitempty"`
	Kit             bool   `json:"kit,omitempty"`
}

type EventDamage struct {
	Tick            int    `json:"tick"`
	Round           int    `json:"round,omitempty"`
	AttackerSteamID string `json:"attacker,omitempty"`
	VictimSteamID   string `json:"victim,omitempty"`
	AttackerTeam    string `json:"attacker_team,omitempty"`
	VictimTeam      string `json:"victim_team,omitempty"`
	Weapon          string `json:"weapon,omitempty"`
	Damage          int    `json:"damage"`
	DamageArmor     int    `json:"damage_armor,omitempty"`
	Hitgroup        int    `json:"hitgroup,omitempty"`
	Health          int    `json:"health,omitempty"`
	HitOnSpotted    bool   `json:"hit_on_spotted,omitempty"`
	// FromSpray: the attacker's most-recent shot was a spray shot and
	// fired close enough to this damage to plausibly have produced it.
	FromSpray         bool     `json:"from_spray,omitempty"`
	SpotToDamageS     *float64 `json:"spot_to_damage,omitempty"`
	CrosshairDeltaDeg *float32 `json:"crosshair_delta_deg,omitempty"`
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
	GrenadeID      int     `json:"gid,omitempty"`
	ThrowerSteamID string  `json:"thrower,omitempty"`
	ThrowerTeam    string  `json:"thrower_team,omitempty"`
	Type           string  `json:"type"`
	OriginX        float32 `json:"ox,omitempty"`
	OriginY        float32 `json:"oy,omitempty"`
	OriginZ        float32 `json:"oz,omitempty"`
}

type EventGrenadeDetonate struct {
	Tick           int     `json:"tick"`
	Round          int     `json:"round,omitempty"`
	GrenadeID      int     `json:"gid,omitempty"`
	ThrowerSteamID string  `json:"thrower,omitempty"`
	Type           string  `json:"type"`
	X              float32 `json:"x,omitempty"`
	Y              float32 `json:"y,omitempty"`
	Z              float32 `json:"z,omitempty"`
}

type PlayerInfo struct {
	SteamID      string `json:"steam_id"`
	Name         string `json:"name"`
	Rank         int    `json:"rank,omitempty"`
	RankType     int    `json:"rank_type,omitempty"`
	PreviousRank int    `json:"previous_rank,omitempty"`
	WinCount     int    `json:"win_count,omitempty"`
}

type Result struct {
	TotalTicks int     `json:"total_ticks"`
	TickRate   float64 `json:"tick_rate"`
	MapName    string  `json:"map_name"`
	WorkshopID string  `json:"workshop_id,omitempty"`
	// Game-rule signals used by the importer to classify the match type.
	ServerName      string       `json:"server_name,omitempty"`
	MaxRounds       int          `json:"max_rounds,omitempty"`
	OvertimeEnabled bool         `json:"overtime_enabled,omitempty"`
	PlayerCount     int          `json:"player_count,omitempty"`
	RoundTicks      []RoundTick  `json:"round_ticks"`
	Kills           []EventKill  `json:"kills"`
	Bombs           []EventBomb  `json:"bombs"`
	Players         []PlayerInfo `json:"players,omitempty"`

	ShotsFired         []EventShotFired       `json:"shots_fired,omitempty"`
	Damages            []EventDamage          `json:"damages,omitempty"`
	Spotted            []EventSpotted         `json:"spotted,omitempty"`
	GrenadeThrows      []EventGrenadeThrow    `json:"grenade_throws,omitempty"`
	GrenadeDetonations []EventGrenadeDetonate `json:"grenade_detonations,omitempty"`
	Flashes            []EventFlash           `json:"flashes,omitempty"`
	RoundInventory     []EventRoundInventory  `json:"round_inventory,omitempty"`
	Positions          []EventPosition        `json:"positions,omitempty"`
	KitDrops           []EventKitDrop         `json:"kit_drops,omitempty"`
}

// Speed is derived from position deltas between FrameDone events.
// m_vecVelocity off the pawn isn't reliable across CS2 demo formats.
type playerFrame struct {
	pos      r3.Vector
	speed    float32
	hasSpeed bool
	team     common.Team
	alive    bool
	tick     int
}

// visEntry records when a spotter gained sight of a player and the
// spotter's eye angles at that instant. Consumed by the next matching
// PlayerHurt event to compute spot-to-damage and crosshair delta.
type visEntry struct {
	tick  int
	yaw   float32
	pitch float32
}

// shotMark records the attacker's last shot tick + whether that shot
// was a spray shot. PlayerHurt attributes a damage to the most-recent
// shot to inherit the spray flag.
type shotMark struct {
	tick    int
	isSpray bool
}
