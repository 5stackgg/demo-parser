package parser

import (
	"github.com/golang/geo/r3"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
)

type RoundTick struct {
	Round     int    `json:"round"`
	StartTick int    `json:"start_tick"`
	EndTick   int    `json:"end_tick"`
	Winner    string `json:"winner,omitempty"`
	Reason    int    `json:"reason,omitempty"`
}

type EventKill struct {
	Tick          int    `json:"tick"`
	KillerSteamID string `json:"killer,omitempty"`
	VictimSteamID string `json:"victim,omitempty"`
	AssistSteamID string `json:"assist,omitempty"`
	KillerTeam    string `json:"killer_team,omitempty"`
	VictimTeam    string `json:"victim_team,omitempty"`
	Weapon        string `json:"weapon,omitempty"`
	Headshot      bool   `json:"headshot,omitempty"`
	WallBang      bool   `json:"wallbang,omitempty"`
	NoScope       bool   `json:"noscope,omitempty"`
	ThroughSmoke  bool   `json:"smoke,omitempty"`
}

type EventBomb struct {
	Tick   int    `json:"tick"`
	Type   string `json:"type"`
	Player string `json:"player,omitempty"`
	Site   string `json:"site,omitempty"`
}

type EventShotFired struct {
	Tick            int      `json:"tick"`
	Round           int      `json:"round,omitempty"`
	AttackerSteamID string   `json:"attacker,omitempty"`
	AttackerTeam    string   `json:"attacker_team,omitempty"`
	Weapon          string   `json:"weapon,omitempty"`
	IsRifle         bool     `json:"is_rifle,omitempty"`
	IsCrouched      bool     `json:"is_crouched,omitempty"`
	EnemySpotted    bool     `json:"enemy_spotted,omitempty"`
	// IsSpray: this shot followed the same attacker's previous shot
	// within 250ms — i.e. trigger held. First shot of a burst is false.
	IsSpray    bool     `json:"is_spray,omitempty"`
	Speed      *float32 `json:"speed,omitempty"`
	WasStopped *bool    `json:"was_stopped,omitempty"`
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
	ThrowerSteamID string  `json:"thrower,omitempty"`
	Type           string  `json:"type"`
	X              float32 `json:"x,omitempty"`
	Y              float32 `json:"y,omitempty"`
	Z              float32 `json:"z,omitempty"`
}

type PlayerInfo struct {
	SteamID string `json:"steam_id"`
	Name    string `json:"name"`
}

type Result struct {
	TotalTicks int          `json:"total_ticks"`
	TickRate   float64      `json:"tick_rate"`
	MapName    string       `json:"map_name"`
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
